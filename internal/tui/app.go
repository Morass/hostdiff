// Package tui is hostdiff's interactive mode: pick the other machine and
// what to compare, watch both machines being scanned, then browse the
// differences as a table and install, update or remove items on either
// machine after confirming the exact commands.
package tui

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/snapshot"
)

var (
	styleA     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleB     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleCh    = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleBad   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleBold  = lipgloss.NewStyle().Bold(true)
	styleSel   = lipgloss.NewStyle().Reverse(true)
	styleTitle = lipgloss.NewStyle().Bold(true).Underline(true)
)

// Machine is a machine to choose from.
type Machine struct {
	Name  string
	Where string // "this machine", "ssh laptop"
}

// Group is a section hostdiff can collect.
type Group struct {
	Kind, Title, Reads string
	OS                 string // "darwin", "linux" or empty for both
}

// Side says whether and how commands can run on one machine.
type Side struct {
	Where string
	// Remote is true when commands reach the machine over ssh.
	Remote bool
	// NoInstall says why nothing can change there (a snapshot file).
	NoInstall string
	// Prepare turns an installer script into the run that carries it out on
	// that machine in this terminal.
	Prepare func(script string) (*Started, error)
}

// Started is one prepared installer run.
type Started struct {
	Cmd *exec.Cmd
	// Results returns the exit status of each step, in order, once the
	// command has finished; a step that never ran is missing.
	Results func() (map[int]int, error)
	Cleanup func()
}

// Reach says what stops hostdiff from using a machine on its own.
type Reach int

const (
	Reachable Reach = iota
	NeedsHostKey
	NeedsAuth
	Unreachable
)

// Backend is what the interactive mode needs from hostdiff.
type Backend interface {
	Here() Machine
	Others() []Machine
	Groups() []Group
	// Select chooses the two sides by name, or explains why not.
	Select(a, b string) error
	Labels() [2]string
	// Collect collects sections of one side (0 is A), replacing what was
	// collected for them before. progress may be called from goroutines.
	Collect(side int, kinds []string, progress func(snapshot.Progress)) error
	// Result compares what has been collected, limited to kinds.
	Result(kinds []string) *diff.Result
	Side(side int) Side
	// Reach tries the connection to one side without asking anything; the
	// second result is what ssh said.
	Reach(side int) (Reach, string)
	// Connect hands the terminal to ssh, so a host key can be checked and a
	// password typed once for the whole run.
	Connect(side int) (*Started, error)
}

// Start says where the interactive mode begins.
type Start struct {
	// A defaults to this machine. With B empty, the other machine is chosen
	// first.
	A, B string
	// Kinds are the sections to compare; empty means everything.
	Kinds []string
	// PickGroups shows the group list before scanning even when B is given.
	PickGroups bool
}

// Run runs the interactive mode until the user quits.
func Run(b Backend, st Start) error {
	_, err := tea.NewProgram(newApp(b, st), tea.WithAltScreen()).Run()
	return err
}

type screen int

const (
	screenMachines screen = iota
	screenCheck
	screenConnect
	screenGroups
	screenScan
	screenView
	screenError
)

type sectionState struct {
	running, done bool
	items         int
	status        snapshot.Status
}

type progressMsg struct {
	gen, side int
	p         snapshot.Progress
}

type collectedMsg struct {
	gen, side int
	err       error
}

type tickMsg struct{ gen int }

type checkedMsg struct {
	gen   int
	reach Reach
	msg   string
}

// App is the bubbletea model of the whole interactive mode.
type App struct {
	b             Backend
	screen        screen
	width, height int
	note          string

	here     Machine
	others   []Machine
	mcur     int
	custom   string
	entering bool
	aName    string
	bName    string

	groups []Group
	gcur   int
	gsel   map[string]bool

	kinds   []string
	gen     int
	ch      chan tea.Msg
	state   [2]map[string]*sectionState
	stage   [2]string
	done    [2]bool
	errs    [2]error
	frame   int
	started time.Time

	view  *view
	err   string
	reach Reach
	why   string
	// pick is set when the machine or the groups were chosen here, so the
	// group list is shown again after a connection was sorted out.
	pick bool
}

func newApp(b Backend, st Start) *App {
	a := &App{b: b, width: 100, height: 30, here: b.Here(), others: b.Others(), groups: b.Groups(), gsel: map[string]bool{}}
	for _, k := range st.Kinds {
		a.gsel[k] = true
	}
	a.aName = st.A
	if a.aName == "" {
		a.aName = a.here.Name
	}
	if st.B == "" {
		a.screen, a.pick = screenMachines, true
		return a
	}
	a.pick = st.PickGroups
	a.bName = st.B
	for i, m := range a.others {
		if m.Name == st.B {
			a.mcur = i
		}
	}
	if err := b.Select(a.aName, a.bName); err != nil {
		a.fail(err.Error())
		return a
	}
	a.kinds = st.Kinds
	switch {
	case a.b.Side(1).Remote:
		a.screen = screenCheck
	case st.PickGroups:
		a.screen = screenGroups
	default:
		a.screen = screenScan
	}
	return a
}

func (a *App) fail(msg string) {
	a.screen, a.err = screenError, msg
}

// Init starts where newApp left off.
func (a *App) Init() tea.Cmd {
	switch a.screen {
	case screenScan:
		return a.startScan()
	case screenCheck:
		return a.startCheck()
	}
	return nil
}

// startCheck tries the connection to the other machine before anything is
// collected, so ssh's questions are answered here and not in the dark.
func (a *App) startCheck() tea.Cmd {
	a.gen++
	gen, b := a.gen, a.b
	a.screen, a.note = screenCheck, ""
	a.started = time.Now()
	return tea.Batch(
		func() tea.Msg {
			reach, msg := b.Reach(1)
			return checkedMsg{gen: gen, reach: reach, msg: msg}
		},
		tick(a.gen),
	)
}

// connect hands the terminal to ssh, then checks again.
func (a *App) connect() tea.Cmd {
	run, err := a.b.Connect(1)
	if err != nil {
		a.why = err.Error()
		return nil
	}
	a.screen = screenCheck
	return tea.ExecProcess(run.Cmd, func(error) tea.Msg {
		if run.Cleanup != nil {
			run.Cleanup()
		}
		reach, msg := a.b.Reach(1)
		return checkedMsg{gen: a.gen, reach: reach, msg: msg}
	})
}

func (a *App) allKinds() []string {
	var out []string
	for _, g := range a.groups {
		out = append(out, g.Kind)
	}
	return out
}

func (a *App) title(kind string) string {
	for _, g := range a.groups {
		if g.Kind == kind {
			return g.Title
		}
	}
	return kind
}

func (a *App) startScan() tea.Cmd {
	if len(a.kinds) == 0 {
		a.kinds = a.allKinds()
	}
	a.gen++
	gen := a.gen
	a.screen, a.note = screenScan, ""
	a.ch = make(chan tea.Msg, 512)
	for side := 0; side < 2; side++ {
		a.state[side] = map[string]*sectionState{}
		a.done[side], a.errs[side], a.stage[side] = false, nil, ""
	}
	a.started = time.Now()
	ch, kinds, b := a.ch, a.kinds, a.b
	for side := 0; side < 2; side++ {
		go func() {
			err := b.Collect(side, kinds, func(p snapshot.Progress) { ch <- progressMsg{gen: gen, side: side, p: p} })
			ch <- collectedMsg{gen: gen, side: side, err: err}
		}()
	}
	return tea.Batch(listen(ch), tick(gen))
}

func listen(ch chan tea.Msg) tea.Cmd { return func() tea.Msg { return <-ch } }

func tick(gen int) tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{gen} })
}

// Update handles every message of the interactive mode.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		if a.view != nil {
			a.view.width, a.view.height = msg.Width, msg.Height
			a.view.clamp()
		}
		return a, nil
	case progressMsg:
		if msg.gen != a.gen {
			return a, nil
		}
		st := a.state[msg.side]
		switch msg.p.Stage {
		case "connect":
			a.stage[msg.side] = "connecting over ssh…"
		case "upload":
			a.stage[msg.side] = "hostdiff is not installed there: sending it for this run…"
		case "start":
			st[msg.p.Kind] = &sectionState{running: true}
			a.stage[msg.side] = ""
		case "done":
			st[msg.p.Kind] = &sectionState{done: true, items: msg.p.Items, status: msg.p.Status}
			a.stage[msg.side] = ""
		}
		return a, listen(a.ch)
	case collectedMsg:
		if msg.gen != a.gen {
			return a, nil
		}
		a.done[msg.side], a.errs[msg.side] = true, msg.err
		if !a.done[0] || !a.done[1] {
			return a, listen(a.ch)
		}
		if err := errors.Join(a.errs[0], a.errs[1]); err != nil {
			a.fail(err.Error())
			return a, nil
		}
		res := a.b.Result(a.kinds)
		if a.view == nil {
			a.view = newView(a.b, res)
		} else {
			a.view.setResult(res)
			a.view.sel = map[string]bool{}
		}
		a.view.kinds = a.kinds
		a.view.width, a.view.height = a.width, a.height
		a.view.clamp()
		a.screen = screenView
		return a, nil
	case checkedMsg:
		if msg.gen != a.gen {
			return a, nil
		}
		a.reach, a.why = msg.reach, msg.msg
		if msg.reach == Reachable {
			if !a.pick {
				return a, a.startScan()
			}
			a.screen = screenGroups
			return a, nil
		}
		a.screen = screenConnect
		return a, nil
	case tickMsg:
		if msg.gen != a.gen || (a.screen != screenScan && a.screen != screenCheck) {
			return a, nil
		}
		a.frame++
		return a, tick(msg.gen)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}
		// A fast typist or a terminal multiplexer can deliver several keys
		// as one message; handle them one by one.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !a.typing() {
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				_, cmd := a.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				cmds = append(cmds, cmd)
			}
			return a, tea.Batch(cmds...)
		}
		return a.key(msg)
	}
	if a.view != nil {
		return a, a.view.handle(msg)
	}
	return a, nil
}

// typing reports whether keys are text input rather than commands.
func (a *App) typing() bool {
	if a.screen == screenMachines {
		return a.entering
	}
	return a.screen == screenView && a.view != nil && (a.view.filtering || a.view.needTyped())
}

func (a *App) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch a.screen {
	case screenMachines:
		return a, a.keyMachines(k)
	case screenGroups:
		return a, a.keyGroups(k)
	case screenScan, screenCheck:
		if k == "q" {
			return a, tea.Quit
		}
	case screenConnect:
		switch k {
		case "q":
			return a, tea.Quit
		case "c", "enter":
			return a, a.connect()
		case "r":
			return a, a.startCheck()
		case "esc", "m", "h", "left":
			a.screen = screenMachines
		}
	case screenError:
		switch k {
		case "q", "esc":
			return a, tea.Quit
		case "r":
			if a.bName != "" && a.b.Select(a.aName, a.bName) == nil {
				return a, a.startScan()
			}
		case "m":
			a.screen = screenMachines
		}
	case screenView:
		if k == "esc" && a.view != nil && !a.view.right && a.view.menu == nil && a.view.detail == nil && !a.view.busy && !a.view.filtering {
			a.gsel = map[string]bool{}
			if len(a.kinds) < len(a.groups) {
				for _, kind := range a.kinds {
					a.gsel[kind] = true
				}
			}
			a.screen = screenGroups
			return a, nil
		}
		cmd, to := a.view.key(k, msg)
		switch to {
		case navQuit:
			return a, tea.Quit
		case navGroups:
			a.gsel = map[string]bool{}
			if len(a.kinds) < len(a.groups) {
				for _, k := range a.kinds {
					a.gsel[k] = true
				}
			}
			a.screen = screenGroups
		case navMachines:
			a.screen = screenMachines
		case navRescan:
			return a, a.startScan()
		}
		return a, cmd
	}
	return a, nil
}

func (a *App) keyMachines(k string) tea.Cmd {
	// The last row is typed in: an ssh destination, a snapshot file or
	// NAME@last.
	if a.entering {
		switch k {
		case "esc":
			a.entering, a.custom, a.note = false, "", ""
		case "enter":
			return a.chooseMachine(a.custom)
		case "backspace":
			if r := []rune(a.custom); len(r) > 0 {
				a.custom = string(r[:len(r)-1])
			}
		default:
			if len([]rune(k)) == 1 || k == " " {
				a.custom += k
			}
		}
		return nil
	}
	switch k {
	case "q":
		return tea.Quit
	case "esc":
		if a.view != nil {
			a.screen = screenView
		}
	case "down", "j":
		a.mcur = min(a.mcur+1, len(a.others))
	case "up", "k":
		a.mcur = max(a.mcur-1, 0)
	case "enter", "right", "l":
		if a.mcur >= len(a.others) {
			a.entering, a.note = true, ""
			return nil
		}
		name := a.others[a.mcur].Name
		return a.chooseMachine(name)
	}
	return nil
}

// chooseMachine takes the other side, checks the connection when it is one
// that ssh has to reach, and moves on to the groups.
func (a *App) chooseMachine(name string) tea.Cmd {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	if err := a.b.Select(a.aName, name); err != nil {
		a.note = err.Error()
		return nil
	}
	if name != a.bName {
		a.view = nil
	}
	a.bName, a.note, a.entering = name, "", false
	if a.b.Side(1).Remote {
		a.kinds, a.pick = nil, true
		return a.startCheck()
	}
	a.screen = screenGroups
	return nil
}

func (a *App) keyGroups(k string) tea.Cmd {
	switch k {
	case "q":
		return tea.Quit
	case "esc", "left", "h":
		// One step back: the machine list comes before the groups.
		a.screen = screenMachines
	case "down", "j":
		a.gcur = min(a.gcur+1, len(a.groups)-1)
	case "up", "k":
		a.gcur = max(a.gcur-1, 0)
	case " ", "x":
		if len(a.groups) > 0 {
			g := a.groups[a.gcur].Kind
			a.gsel[g] = !a.gsel[g]
			a.gcur = min(a.gcur+1, len(a.groups)-1)
		}
	case "a":
		all := true
		for _, g := range a.groups {
			all = all && a.gsel[g.Kind]
		}
		for _, g := range a.groups {
			a.gsel[g.Kind] = !all
		}
	case "enter":
		a.kinds = nil
		for _, g := range a.groups {
			if a.gsel[g.Kind] {
				a.kinds = append(a.kinds, g.Kind)
			}
		}
		return a.startScan()
	}
	return nil
}

// View renders the current screen.
func (a *App) View() string {
	switch a.screen {
	case screenMachines:
		return a.viewMachines()
	case screenCheck:
		return a.viewCheck()
	case screenConnect:
		return a.viewConnect()
	case screenGroups:
		return a.viewGroups()
	case screenScan:
		return a.viewScan()
	case screenError:
		return a.viewError()
	}
	if a.view == nil {
		return ""
	}
	return a.view.render()
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

func padTo(s string, w int) string {
	if gap := w - ansi.StringWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// screenLines pads a screen to the window height and adds the footer.
func (a *App) screenLines(lines []string, footer string) string {
	h := max(3, a.height-1)
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = truncate(l, a.width)
	}
	return strings.Join(lines, "\n") + "\n" + styleDim.Render(truncate(footer, a.width))
}

func (a *App) viewMachines() string {
	lines := []string{
		styleTitle.Render("hostdiff") + styleDim.Render(" · compare two machines"),
		"",
		styleA.Render("◀ "+a.here.Name) + styleDim.Render("  "+a.here.Where),
		"",
		styleB.Render("▶") + " Compare with:",
	}
	if len(a.others) == 0 {
		lines = append(lines, "", "  No other machines are configured.", "  Add one to the config file (hostdiff config path), then start hostdiff again.")
	}
	rows := append([]Machine{}, a.others...)
	rows = append(rows, Machine{Name: "something else…", Where: "an ssh destination, a snapshot file, or NAME@last"})
	nw := 0
	for _, m := range rows {
		nw = max(nw, ansi.StringWidth(m.Name))
	}
	top := max(0, a.mcur-(a.height-10))
	for i, m := range rows {
		if i < top {
			continue
		}
		l := "  " + padTo(m.Name, nw) + "   " + styleDim.Render(m.Where)
		if i == a.mcur {
			l = styleSel.Render(padTo("› "+padTo(m.Name, nw)+"   "+m.Where, min(a.width, nw+ansi.StringWidth(m.Where)+8)))
		}
		lines = append(lines, l)
	}
	if a.entering {
		lines = append(lines, "", "  Compare with: "+a.custom+"▏", styleDim.Render("  ssh destination (me@host, a Host from ~/.ssh/config), path to a snapshot, or NAME@last"))
	}
	if a.note != "" {
		lines = append(lines, "", styleBad.Render(a.note))
	}
	footer := "↑↓ choose · enter continue · q quit"
	if a.view != nil {
		footer = "↑↓ choose · enter continue · esc back to the comparison · q quit"
	}
	if a.entering {
		footer = "type a destination · enter continue · esc cancel"
	}
	return a.screenLines(lines, footer)
}

func (a *App) viewGroups() string {
	lines := []string{
		styleTitle.Render("What should be compared?") + styleDim.Render(fmt.Sprintf("  %s and %s", a.labels()[0], a.labels()[1])),
		"",
	}
	tw := 0
	for _, g := range a.groups {
		tw = max(tw, ansi.StringWidth(g.Title))
	}
	h := max(3, a.height-5)
	top := max(0, a.gcur-h+1)
	for i, g := range a.groups {
		if i < top || i >= top+h {
			continue
		}
		box := "[ ]"
		if a.gsel[g.Kind] {
			box = styleOK.Render("[x]")
		}
		os := map[string]string{"darwin": " (macOS)", "linux": " (Linux)"}[g.OS]
		l := box + " " + padTo(g.Title, tw) + "  " + styleDim.Render(g.Reads+os)
		if i == a.gcur {
			l = styleBold.Render("›") + l
		} else {
			l = " " + l
		}
		lines = append(lines, l)
	}
	n := 0
	for _, g := range a.groups {
		if a.gsel[g.Kind] {
			n++
		}
	}
	sum := "nothing selected: enter compares everything"
	if n > 0 {
		sum = fmt.Sprintf("%d selected", n)
	}
	lines = append(lines, "", styleDim.Render(sum))
	footer := "↑↓ move · space select · a all · enter scan · esc back to the machines · q quit"
	return a.screenLines(lines, footer)
}

func (a *App) labels() [2]string {
	if a.bName == "" {
		return [2]string{a.aName, "?"}
	}
	return a.b.Labels()
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (a *App) viewScan() string {
	labels := a.labels()
	what := "everything"
	if len(a.kinds) < len(a.groups) {
		var t []string
		for _, k := range a.kinds {
			t = append(t, a.title(k))
		}
		what = strings.Join(t, ", ")
	}
	lines := []string{
		styleTitle.Render("Scanning") + styleDim.Render(fmt.Sprintf("  %s · %.1fs", what, time.Since(a.started).Seconds())),
		"",
	}
	spin := spinner[a.frame%len(spinner)]
	column := func(side int) []string {
		style := styleA
		mark := "◀ "
		if side == 1 {
			style, mark = styleB, "▶ "
		}
		out := []string{style.Render(mark + labels[side])}
		switch {
		case a.errs[side] != nil:
			out = append(out, styleBad.Render("  ✗ failed"))
		case a.done[side]:
			out = append(out, styleOK.Render("  ✓ done"))
		case a.stage[side] != "":
			out = append(out, "  "+spin+" "+a.stage[side])
		default:
			out = append(out, "")
		}
		tw := 0
		for _, k := range a.kinds {
			tw = max(tw, ansi.StringWidth(a.title(k)))
		}
		kinds := append([]string{}, a.kinds...)
		sort.SliceStable(kinds, func(i, j int) bool { return a.order(kinds[i]) < a.order(kinds[j]) })
		for _, k := range kinds {
			st := a.state[side][k]
			var glyph, info string
			switch {
			case st == nil && a.done[side]:
				glyph, info = styleDim.Render("–"), styleDim.Render("not on this system")
			case st == nil:
				glyph = styleDim.Render("·")
			case st.running:
				glyph = spin
			case st.status == snapshot.Failed:
				glyph, info = styleBad.Render("✗"), styleBad.Render("failed")
			case st.status == snapshot.Absent || st.status == snapshot.Unavailable:
				glyph, info = styleDim.Render("–"), styleDim.Render(string(st.status))
			default:
				glyph, info = styleOK.Render("✓"), styleDim.Render(fmt.Sprint(st.items))
			}
			out = append(out, "  "+glyph+" "+padTo(a.title(k), tw)+"  "+info)
		}
		return out
	}
	left, right := column(0), column(1)
	if a.width >= 90 {
		cw := a.width / 2
		for i := 0; i < max(len(left), len(right)); i++ {
			l, r := "", ""
			if i < len(left) {
				l = left[i]
			}
			if i < len(right) {
				r = right[i]
			}
			lines = append(lines, padTo(truncate(l, cw-1), cw)+r)
		}
	} else {
		lines = append(append(append(lines, left...), ""), right...)
	}
	return a.screenLines(lines, "scanning both machines · q quit")
}

func (a *App) order(kind string) int {
	for i, g := range a.groups {
		if g.Kind == kind {
			return i
		}
	}
	return len(a.groups)
}

func (a *App) viewCheck() string {
	spin := spinner[a.frame%len(spinner)]
	lines := []string{
		styleTitle.Render("Connecting"),
		"",
		fmt.Sprintf("  %s %s (%s)", spin, a.bName, a.b.Side(1).Where),
		"",
		styleDim.Render("  Checking that ssh can reach it without asking anything."),
	}
	return a.screenLines(lines, "q quit")
}

func (a *App) viewConnect() string {
	var head, what string
	switch a.reach {
	case NeedsHostKey:
		head = a.bName + " has not been connected to from this machine yet"
		what = "ssh wants you to check its fingerprint before trusting it (or the key has changed)."
	case NeedsAuth:
		head = a.bName + " needs a password or a key"
		what = "ssh could not log in without asking: no key it accepts, or password login."
	default:
		head = a.bName + " could not be reached"
		what = "ssh could not connect at all: check the name, the network, or whether it is awake."
	}
	lines := []string{
		styleBad.Render(head),
		"",
		"  " + what,
		"",
	}
	for _, l := range strings.Split(a.why, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, styleDim.Render("  ssh: "+l))
		}
	}
	lines = append(lines, "")
	if a.reach == NeedsHostKey || a.reach == NeedsAuth {
		lines = append(lines,
			"  c   connect now: ssh takes over this terminal, so you can read the",
			"      fingerprint and type the password. hostdiff keeps that one",
			"      connection for the rest of this run, so it is asked once.",
			"")
		if a.reach == NeedsAuth {
			lines = append(lines, styleDim.Render("  To stop being asked at all: ssh-copy-id "+strings.TrimPrefix(a.b.Side(1).Where, "ssh ")), "")
		}
	}
	footer := "c connect · r try again · esc other machine · q quit"
	if a.reach == Unreachable {
		footer = "r try again · esc other machine · q quit"
	}
	return a.screenLines(lines, footer)
}

func (a *App) viewError() string {
	lines := []string{styleBad.Render("Could not compare")}
	for _, l := range strings.Split(a.err, "\n") {
		lines = append(lines, "  "+l)
	}
	return a.screenLines(lines, "r retry · m choose machines · q quit")
}
