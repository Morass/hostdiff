// Package tui is the interactive view of a comparison: sections on the left,
// what differs on the right, the content diff of any item on Enter, and
// installing marked items on either machine.
package tui

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/fix"
	"github.com/morass/hostdiff/internal/render"
	"github.com/morass/hostdiff/internal/snapshot"
)

var (
	styleA     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleB     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleCh    = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleBold  = lipgloss.NewStyle().Bold(true)
	styleSel   = lipgloss.NewStyle().Reverse(true)
	styleTitle = lipgloss.NewStyle().Bold(true).Underline(true)
)

type row struct {
	mark   string
	key    string
	value  string
	change *diff.Change
	item   *snapshot.Item
}

// Side says whether and how hostdiff can install on one machine of the
// comparison.
type Side struct {
	// Where describes how the machine is reached: "this machine", "ssh laptop".
	Where string
	// NoInstall says why nothing can be installed there (a snapshot file).
	NoInstall string
	// Prepare turns an installer script into the command that runs it on that
	// machine in this terminal, and returns what to clean up afterwards.
	Prepare func(script string) (*exec.Cmd, func(), error)
}

// Options connect the view to the machines. The zero value is read-only.
type Options struct {
	Sides [2]Side
	// Refresh collects the given sections of one side (0 is A) again and
	// returns the new comparison.
	Refresh func(side int, kinds []string) (*diff.Result, error)
}

type markID struct {
	side      int
	kind, key string
}

type preparedMsg struct {
	side    int
	cmd     *exec.Cmd
	cleanup func()
	err     error
	marked  []markID
}

type ranMsg struct {
	preparedMsg
	err error
}

type refreshedMsg struct {
	side   int
	res    *diff.Result
	err    error
	marked []markID
}

// Model is the bubbletea model.
type Model struct {
	opt        Options
	marks      map[markID]bool
	plan       bool
	busy       bool
	queue      []int
	status     string
	res        *diff.Result
	secs       []int
	sec, row   int
	top        int
	right      bool
	showSame   bool
	showDeps   bool
	filter     string
	filtering  bool
	detail     []string
	detailName string
	detailTop  int
	width      int
	height     int
}

// New builds the model. Sections where nothing could be collected on either
// side are left out.
func New(res *diff.Result, opt Options) Model {
	m := Model{opt: opt, marks: map[markID]bool{}, width: 100, height: 30}
	m.setResult(res)
	// Start on the first section with differences.
	for i, si := range m.secs {
		if res.Sections[si].Differences() > 0 {
			m.sec = i
			break
		}
	}
	return m
}

// setResult shows a new comparison, staying on the same section.
func (m *Model) setResult(res *diff.Result) {
	kind := ""
	if s := m.section(); s != nil {
		kind = s.Kind
	}
	m.res, m.secs, m.sec = res, nil, 0
	for i := range res.Sections {
		s := &res.Sections[i]
		if !s.Comparable && s.StatusA == snapshot.Absent && s.StatusB == snapshot.Absent {
			continue
		}
		if s.Comparable && len(s.OnlyA)+len(s.OnlyB)+len(s.Changed)+len(s.Same) == 0 {
			continue
		}
		if s.Kind == kind {
			m.sec = len(m.secs)
		}
		m.secs = append(m.secs, i)
	}
}

// Run shows the comparison until the user quits.
func Run(res *diff.Result, opt Options) error {
	_, err := tea.NewProgram(New(res, opt), tea.WithAltScreen()).Run()
	return err
}

func (m *Model) label(side int) string {
	if side == 0 {
		return m.res.A.Label
	}
	return m.res.B.Label
}

// actions returns what hostdiff could run for a row on each side (0 is A).
func (m *Model) actions(kind string, r row) [2]*fix.Action {
	var out [2]*fix.Action
	set := func(side int, a fix.Action, ok bool) {
		if ok {
			out[side] = &a
		}
	}
	switch {
	case r.change != nil:
		c := r.change
		a, ok := fix.ForChange(kind, c.Key, c.B, c.TagB, c.A, c.TagA)
		set(0, a, ok)
		a, ok = fix.ForChange(kind, c.Key, c.A, c.TagA, c.B, c.TagB)
		set(1, a, ok)
	case r.mark == "▶":
		a, ok := fix.ForMissing(kind, *r.item)
		set(0, a, ok)
	case r.mark == "◀":
		a, ok := fix.ForMissing(kind, *r.item)
		set(1, a, ok)
	}
	return out
}

// installable returns the sides a row can be installed on, and otherwise
// the reason it cannot.
func (m *Model) installable(kind string, r row) ([]int, string) {
	var sides []int
	why := "hostdiff has no install command for " + r.key
	for side, a := range m.actions(kind, r) {
		switch {
		case a == nil:
		case !a.Runnable():
			why = r.key + ": " + a.Note
		case m.opt.Sides[side].NoInstall != "":
			why = "cannot install on " + m.label(side) + ": " + m.opt.Sides[side].NoInstall
		case m.opt.Sides[side].Prepare == nil:
			why = "installing is not available in this view"
		default:
			sides = append(sides, side)
		}
	}
	return sides, why
}

// canInstall reports whether either side accepts installs.
func (m *Model) canInstall() bool {
	for _, s := range m.opt.Sides {
		if s.Prepare != nil && s.NoInstall == "" {
			return true
		}
	}
	return false
}

// markedSide returns the side a row is marked for, or -1.
func (m *Model) markedSide(kind, key string) int {
	for side := 0; side < 2; side++ {
		if m.marks[markID{side, kind, key}] {
			return side
		}
	}
	return -1
}

// toggle cycles a row through "not marked" and each side it can go to.
func (m *Model) toggle(kind string, r row) {
	sides, why := m.installable(kind, r)
	if len(sides) == 0 {
		m.status = why
		return
	}
	cur := m.markedSide(kind, r.key)
	for side := 0; side < 2; side++ {
		delete(m.marks, markID{side, kind, r.key})
	}
	for i, side := range sides {
		if cur == -1 || (side == cur && i+1 < len(sides)) {
			next := sides[0]
			if cur != -1 {
				next = sides[i+1]
			}
			m.marks[markID{next, kind, r.key}] = true
			m.status = "marked " + r.key + " to install on " + m.label(next)
			return
		}
	}
	m.status = "unmarked " + r.key
}

// toggleSection marks every installable row shown in the section, or clears
// them when all are marked already.
func (m *Model) toggleSection() {
	s := m.section()
	if s == nil {
		return
	}
	type pick struct {
		key  string
		side int
	}
	var picks []pick
	allMarked := true
	for _, r := range m.rows() {
		if sides, _ := m.installable(s.Kind, r); len(sides) > 0 {
			picks = append(picks, pick{r.key, sides[0]})
			allMarked = allMarked && m.markedSide(s.Kind, r.key) != -1
		}
	}
	if len(picks) == 0 {
		m.status = "nothing in " + s.Title + " that hostdiff can install"
		return
	}
	for _, p := range picks {
		for side := 0; side < 2; side++ {
			delete(m.marks, markID{side, s.Kind, p.key})
		}
		if !allMarked {
			m.marks[markID{p.side, s.Kind, p.key}] = true
		}
	}
	if allMarked {
		m.status = fmt.Sprintf("unmarked %d items in %s", len(picks), s.Title)
	} else {
		m.status = fmt.Sprintf("marked %d items in %s", len(picks), s.Title)
	}
}

// markedPlan returns the marked actions for one side, in install order.
func (m *Model) markedPlan(side int) []fix.Action {
	var out []fix.Action
	for _, a := range fix.Plan(m.res, side == 0) {
		if a.Runnable() && m.marks[markID{side, a.Kind, a.Key}] {
			out = append(out, a)
		}
	}
	return out
}

func (m *Model) markSummary() string {
	var parts []string
	for side := 0; side < 2; side++ {
		n := 0
		for id := range m.marks {
			if id.side == side {
				n++
			}
		}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d → %s", n, m.label(side)))
		}
	}
	return strings.Join(parts, ", ")
}

func (m *Model) openPlan() {
	var lines []string
	for side := 0; side < 2; side++ {
		acts := m.markedPlan(side)
		if len(acts) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("On %s (%s), %d commands:", m.label(side), m.opt.Sides[side].Where, len(acts)))
		for _, a := range acts {
			lines = append(lines, "  "+a.Command())
		}
		lines = append(lines, "")
	}
	if len(lines) == 0 {
		m.status = "mark items with space first (○ marks what hostdiff can install)"
		return
	}
	lines = append(lines,
		"# The commands run one by one in this terminal; you can answer password prompts.",
		"# A failed step does not stop the rest, Ctrl-C stops after the current step.",
		"# Afterwards those sections are collected again to show what now matches.")
	m.detail, m.detailName, m.detailTop, m.plan = lines, "Install marked items: y runs them, esc goes back", 0, true
}

// next starts the install on the next queued side.
func (m Model) next() (tea.Model, tea.Cmd) {
	for len(m.queue) > 0 {
		side := m.queue[0]
		m.queue = m.queue[1:]
		acts := m.markedPlan(side)
		if len(acts) == 0 {
			continue
		}
		var marked []markID
		for _, a := range acts {
			marked = append(marked, markID{side, a.Kind, a.Key})
		}
		m.busy = true
		m.status = "starting the install on " + m.label(side) + "…"
		script := fix.Installer(m.label(side), acts, true)
		prepare := m.opt.Sides[side].Prepare
		return m, func() tea.Msg {
			cmd, cleanup, err := prepare(script)
			return preparedMsg{side: side, cmd: cmd, cleanup: cleanup, err: err, marked: marked}
		}
	}
	m.busy = false
	return m, nil
}

func kindsOf(ids []markID) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id.kind] {
			seen[id.kind] = true
			out = append(out, id.kind)
		}
	}
	sort.Strings(out)
	return out
}

// stillDifferent counts marked items that the new comparison still lists as
// missing on their side. An installed package at another version than the
// other machine's counts as installed; a setting must match.
func stillDifferent(res *diff.Result, ids []markID) int {
	n := 0
	for _, id := range ids {
		for _, s := range res.Sections {
			if s.Kind != id.kind {
				continue
			}
			missing := s.OnlyA
			if id.side == 0 {
				missing = s.OnlyB
			}
			for _, it := range missing {
				if it.Key == id.key {
					n++
				}
			}
			for _, c := range s.Changed {
				if c.Key == id.key && id.kind == "defaults" {
					n++
				}
			}
		}
	}
	return n
}

func (m Model) Init() tea.Cmd { return nil }

func (m *Model) section() *diff.Section {
	if len(m.secs) == 0 {
		return nil
	}
	return &m.res.Sections[m.secs[m.sec]]
}

func (m *Model) rows() []row {
	s := m.section()
	if s == nil || !s.Comparable {
		return nil
	}
	f := strings.ToLower(m.filter)
	keep := func(parts ...string) bool {
		if f == "" {
			return true
		}
		for _, p := range parts {
			if strings.Contains(strings.ToLower(p), f) {
				return true
			}
		}
		return false
	}
	var out []row
	for i := range s.OnlyA {
		it := &s.OnlyA[i]
		if (it.Tag != "dependency" || m.showDeps) && keep(it.Key, it.Value) {
			out = append(out, row{mark: "◀", key: it.Key, value: it.Value, item: it})
		}
	}
	for i := range s.OnlyB {
		it := &s.OnlyB[i]
		if (it.Tag != "dependency" || m.showDeps) && keep(it.Key, it.Value) {
			out = append(out, row{mark: "▶", key: it.Key, value: it.Value, item: it})
		}
	}
	for i := range s.Changed {
		c := &s.Changed[i]
		if (c.TagA != "dependency" || c.TagB != "dependency" || m.showDeps) && keep(c.Key, c.A, c.B) {
			v := c.A + " │ " + c.B
			if c.A == c.B {
				v = "content differs"
			}
			out = append(out, row{mark: "≠", key: c.Key, value: v, change: c})
		}
	}
	if m.showSame {
		for i := range s.Same {
			it := &s.Same[i]
			if keep(it.Key, it.Value) {
				out = append(out, row{mark: "=", key: it.Key, value: it.Value, item: it})
			}
		}
	}
	return out
}

func (m *Model) bodyHeight() int { return max(3, m.height-4) }

func (m *Model) clamp() {
	n := len(m.rows())
	if m.row >= n {
		m.row = n - 1
	}
	if m.row < 0 {
		m.row = 0
	}
	h := m.bodyHeight()
	if m.row < m.top {
		m.top = m.row
	}
	if m.row >= m.top+h {
		m.top = m.row - h + 1
	}
	if m.top < 0 {
		m.top = 0
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case preparedMsg:
		if msg.err != nil {
			m.status = fmt.Sprintf("could not start the install on %s: %v", m.label(msg.side), msg.err)
			return m.next()
		}
		m.status = "installing on " + m.label(msg.side) + "…"
		return m, tea.ExecProcess(msg.cmd, func(err error) tea.Msg { return ranMsg{preparedMsg: msg, err: err} })
	case ranMsg:
		if msg.cleanup != nil {
			msg.cleanup()
		}
		if m.opt.Refresh == nil {
			for _, id := range msg.marked {
				delete(m.marks, id)
			}
			m.status = "install on " + m.label(msg.side) + " finished"
			return m.next()
		}
		kinds := kindsOf(msg.marked)
		m.status = "collecting " + strings.Join(kinds, ", ") + " on " + m.label(msg.side) + " again…"
		refresh, side, marked := m.opt.Refresh, msg.side, msg.marked
		return m, func() tea.Msg {
			res, err := refresh(side, kinds)
			return refreshedMsg{side: side, res: res, err: err, marked: marked}
		}
	case refreshedMsg:
		for _, id := range msg.marked {
			delete(m.marks, id)
		}
		if msg.err != nil {
			m.status = "collecting again failed: " + msg.err.Error()
			return m.next()
		}
		left := stillDifferent(msg.res, msg.marked)
		m.setResult(msg.res)
		m.clamp()
		m.status = fmt.Sprintf("%s: %d of %d installed items now match", m.label(msg.side), len(msg.marked)-left, len(msg.marked))
		if left > 0 {
			m.status += fmt.Sprintf("; %d still differ (see the install output, or mark them again)", left)
		}
		return m.next()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clamp()
		return m, nil
	case tea.KeyMsg:
		// A fast typist or a terminal multiplexer can deliver several keys as
		// one message; handle them one by one.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !m.filtering {
			var cmd tea.Cmd
			for _, r := range msg.Runes {
				var next tea.Model
				next, cmd = m.key(string(r), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				m = next.(Model)
				if cmd != nil {
					return m, cmd
				}
			}
			return m, nil
		}
		return m.key(msg.String(), msg)
	}
	return m, nil
}

func (m Model) key(k string, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.busy {
		if k == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.filtering {
		switch msg.Type {
		case tea.KeyEnter:
			m.filtering = false
		case tea.KeyEsc:
			m.filtering, m.filter = false, ""
		case tea.KeyBackspace:
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(msg.Runes)
		case tea.KeyCtrlC:
			return m, tea.Quit
		}
		m.row, m.top = 0, 0
		return m, nil
	}
	if m.detail != nil {
		h := m.bodyHeight()
		switch k {
		case "y":
			if m.plan {
				m.detail, m.plan = nil, false
				m.queue = []int{0, 1}
				return m.next()
			}
		case "q", "esc", "enter", "left", "h":
			m.detail, m.plan = nil, false
		case "ctrl+c":
			return m, tea.Quit
		case "down", "j":
			m.detailTop++
		case "up", "k":
			m.detailTop--
		case "pgdown", " ", "f":
			m.detailTop += h
		case "pgup", "b":
			m.detailTop -= h
		case "g", "home":
			m.detailTop = 0
		case "G", "end":
			m.detailTop = len(m.detail) - h
		}
		m.detailTop = max(0, min(m.detailTop, len(m.detail)-h))
		return m, nil
	}
	m.status = ""
	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case " ":
		if s := m.section(); s != nil && s.Comparable {
			if !m.right {
				m.toggleSection()
			} else if rows := m.rows(); m.row >= 0 && m.row < len(rows) {
				m.toggle(s.Kind, rows[m.row])
			}
		}
	case "x":
		m.marks = map[markID]bool{}
		m.status = "all marks cleared"
	case "i":
		m.openPlan()
	case "tab", "right", "l":
		if !m.right {
			m.right = true
		} else if k == "tab" {
			m.right = false
		}
	case "shift+tab", "left", "h", "esc":
		m.right = false
	case "down", "j":
		if m.right {
			m.row++
		} else if m.sec < len(m.secs)-1 {
			m.sec++
			m.row, m.top = 0, 0
		}
	case "up", "k":
		if m.right {
			m.row--
		} else if m.sec > 0 {
			m.sec--
			m.row, m.top = 0, 0
		}
	case "J", "]":
		if m.sec < len(m.secs)-1 {
			m.sec++
			m.row, m.top = 0, 0
		}
	case "K", "[":
		if m.sec > 0 {
			m.sec--
			m.row, m.top = 0, 0
		}
	case "pgdown":
		m.row += m.bodyHeight()
	case "pgup":
		m.row -= m.bodyHeight()
	case "g", "home":
		m.row = 0
	case "G", "end":
		m.row = len(m.rows()) - 1
	case "enter":
		if !m.right {
			m.right = true
			break
		}
		m.openDetail()
	case "/":
		m.filtering, m.filter, m.right = true, "", true
	case "a":
		m.showSame = !m.showSame
	case "d":
		m.showDeps = !m.showDeps
	case "s":
		m.detail = strings.Split(strings.TrimRight(fix.Script(m.res), "\n"), "\n")
		m.detailName = fmt.Sprintf("Script to make %s more like %s (review it, then copy what you need)", m.res.A.Label, m.res.B.Label)
		m.detailTop = 0
	}
	m.clamp()
	return m, nil
}

func (m *Model) openDetail() {
	rows := m.rows()
	if m.row < 0 || m.row >= len(rows) {
		return
	}
	r := rows[m.row]
	var lines []string
	switch {
	case r.change != nil && r.change.DetailA != r.change.DetailB:
		lines = strings.Split(strings.TrimRight(render.Unified(m.res, *r.change), "\n"), "\n")
		lines = append([]string{m.res.A.Label + ": " + r.change.A, m.res.B.Label + ": " + r.change.B, ""}, lines...)
	case r.change != nil:
		lines = []string{m.res.A.Label + ": " + r.change.A, m.res.B.Label + ": " + r.change.B}
	case r.item != nil:
		side := map[string]string{"◀": "only on " + m.res.A.Label, "▶": "only on " + m.res.B.Label, "=": "same on both"}[r.mark]
		lines = []string{side, "value: " + r.item.Value}
		if r.item.Detail != "" {
			lines = append(lines, "")
			lines = append(lines, strings.Split(strings.TrimRight(r.item.Detail, "\n"), "\n")...)
		}
	}
	m.detail, m.detailName, m.detailTop = lines, r.key, 0
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

func describe(s diff.Side) string {
	h := s.Snap.Host
	os := h.OS
	if os == "darwin" {
		os = "macOS"
	}
	return fmt.Sprintf("%s (%s, %s/%s)", s.Label, h.Name, os, h.Arch)
}

func (m Model) View() string {
	w := m.width
	var b strings.Builder
	head := styleA.Render("◀ "+describe(m.res.A)) + "   " + styleB.Render("▶ "+describe(m.res.B))
	b.WriteString(truncate(head, w) + "\n")
	h := m.bodyHeight()

	if m.detail != nil {
		b.WriteString(styleTitle.Render(truncate(m.detailName, w)) + "\n")
		end := min(len(m.detail), m.detailTop+h)
		for i := m.detailTop; i < end; i++ {
			l := m.detail[i]
			switch {
			case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
				l = styleBold.Render(l)
			case strings.HasPrefix(l, "-"):
				l = styleA.Render(l)
			case strings.HasPrefix(l, "+"):
				l = styleB.Render(l)
			case strings.HasPrefix(l, "@@"), strings.HasPrefix(l, "#"):
				l = styleDim.Render(l)
			}
			b.WriteString(truncate(l, w) + "\n")
		}
		for i := end - m.detailTop; i < h; i++ {
			b.WriteString("\n")
		}
		b.WriteString(styleDim.Render(truncate(fmt.Sprintf("line %d/%d · ↑↓ scroll · space page · esc back · q back", min(m.detailTop+1, len(m.detail)), len(m.detail)), w)))
		return b.String()
	}

	lw := min(30, max(18, w/3))
	rw := max(10, w-lw-3)
	info := fmt.Sprintf("%d differences", m.res.Differences())
	if sum := m.markSummary(); sum != "" {
		info += " · marked " + sum + " · i to install"
	}
	if m.status != "" {
		b.WriteString(styleBold.Render(truncate(m.status, w)) + "\n")
	} else {
		b.WriteString(styleDim.Render(truncate(info, w)) + "\n")
	}

	left := make([]string, 0, h)
	for i, si := range m.secs {
		s := &m.res.Sections[si]
		count := fmt.Sprint(s.Differences())
		if !s.Comparable {
			count = "–"
		}
		label := padTo(truncate(s.Title, lw-5), lw-4) + fmt.Sprintf("%4s", count)
		switch {
		case i == m.sec && !m.right:
			label = styleSel.Render(label)
		case i == m.sec:
			label = styleBold.Render(label)
		case s.Differences() == 0:
			label = styleDim.Render(label)
		}
		left = append(left, label)
	}
	// Keep the selected section visible in a long list.
	if off := m.sec - h + 1; off > 0 {
		left = left[off:]
	}

	right := make([]string, 0, h)
	s := m.section()
	rows := m.rows()
	switch {
	case s == nil:
		right = append(right, "Nothing to compare.")
	case !s.Comparable:
		right = append(right, styleBold.Render(s.Title)+" could not be compared:")
		for _, n := range []struct{ label, status, note string }{{m.res.A.Label, string(s.StatusA), s.NoteA}, {m.res.B.Label, string(s.StatusB), s.NoteB}} {
			right = append(right, fmt.Sprintf("  %s: %s %s", n.label, n.status, n.note))
		}
		right = append(right, "", styleDim.Render("Run `hostdiff snap -o FILE` in a Terminal on that machine and compare the files."))
	case len(rows) == 0:
		msg := "No differences."
		if m.filter != "" {
			msg = "Nothing matches the filter."
		}
		right = append(right, styleDim.Render(msg))
	default:
		kw := 0
		for _, r := range rows {
			kw = max(kw, ansi.StringWidth(r.key))
		}
		kw = min(kw, rw/2)
		end := min(len(rows), m.top+h)
		for i := m.top; i < end; i++ {
			r := rows[i]
			mark := r.mark
			switch r.mark {
			case "◀":
				mark = styleA.Render(mark)
			case "▶":
				mark = styleB.Render(mark)
			case "≠":
				mark = styleCh.Render(mark)
			default:
				mark = styleDim.Render(mark)
			}
			// The mark column only appears when a side can be installed on.
			pick, pw := "", 2
			if m.canInstall() {
				pick, pw = "  ", 4
				switch side := m.markedSide(s.Kind, r.key); {
				case side == 0:
					pick = styleA.Render("●") + " "
				case side == 1:
					pick = styleB.Render("●") + " "
				default:
					if sides, _ := m.installable(s.Kind, r); len(sides) > 0 {
						pick = styleDim.Render("○") + " "
					}
				}
			}
			line := padTo(truncate(r.key, kw), kw) + "  " + r.value
			line = truncate(line, rw-pw)
			if i == m.row && m.right {
				line = styleSel.Render(padTo(line, rw-pw))
			}
			right = append(right, mark+" "+pick+line)
		}
	}

	for i := 0; i < h; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		b.WriteString(padTo(l, lw) + " │ " + r + "\n")
	}
	footer := "↑↓ move · tab/enter open · / filter · a same · d dependencies · s script · q quit"
	if m.canInstall() {
		footer = "↑↓ move · enter open · space mark · i install · / filter · a same · d deps · s script · q quit"
	}
	if m.busy {
		footer = "installing… (ctrl+c quits hostdiff)"
	}
	if m.filtering || m.filter != "" {
		footer = "filter: " + m.filter
		if m.filtering {
			footer += "▏ (enter keep · esc clear)"
		}
	}
	var toggles []string
	if m.showSame {
		toggles = append(toggles, "showing same")
	}
	if m.showDeps {
		toggles = append(toggles, "showing dependencies")
	}
	if len(toggles) > 0 {
		footer += " · " + strings.Join(toggles, ", ")
	}
	b.WriteString(styleDim.Render(truncate(footer, w)))
	return b.String()
}
