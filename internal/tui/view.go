package tui

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/fix"
	"github.com/morass/hostdiff/internal/render"
	"github.com/morass/hostdiff/internal/snapshot"
)

type nav int

const (
	navNone nav = iota
	navQuit
	navGroups
	navMachines
	navRescan
)

type row struct {
	kind   string
	mark   string // ◀ only A, ▶ only B, ≠ both but different, = same
	key    string
	a, b   string
	change *diff.Change
	item   *snapshot.Item
}

type sideAction struct {
	side int
	act  fix.Action
}

// choice is one line of the action menu.
type choice struct {
	verb     string
	side     int
	acts     []fix.Action // runnable
	notes    []fix.Action // without a command, with the reason
	details  bool
	clone    bool
	disabled string
}

type job struct {
	choice
	before map[string]string
	had    map[string]bool
}

type preparedMsg struct {
	job     job
	cmd     *exec.Cmd
	cleanup func()
	err     error
}

type ranMsg struct {
	preparedMsg
	err error
}

type refreshedMsg struct {
	job job
	res *diff.Result
	err error
}

// view is the comparison screen: groups on the left, a table of what
// differs on the right, and the action menu, confirmation and runs.
type view struct {
	b      Backend
	res    *diff.Result
	kinds  []string
	secs   []int
	sp, cp int // selected section, and child group (-1: the whole section)

	row, top  int
	right     bool
	showSame  bool
	showDeps  bool
	filter    string
	filtering bool
	sel       map[string]bool

	menu      []choice
	menuTitle string
	menuCur   int

	confirm   *choice
	confirmed []string // the confirmation text, while the full script is shown
	typed     string
	detail    []string
	detailFor string
	detailTop int

	busy          bool
	status        string
	width, height int
}

func newView(b Backend, res *diff.Result) *view {
	v := &view{b: b, sel: map[string]bool{}, cp: -1, width: 100, height: 30}
	v.setResult(res)
	for i, si := range v.secs {
		if res.Sections[si].Differences() > 0 {
			v.sp = i
			break
		}
	}
	return v
}

// setResult shows a new comparison, staying on the same group.
func (v *view) setResult(res *diff.Result) {
	kind, prefix := "", ""
	if s := v.section(); s != nil {
		kind, prefix = s.Kind, v.prefix()
	}
	v.res, v.secs, v.sp, v.cp = res, nil, 0, -1
	for i := range res.Sections {
		s := &res.Sections[i]
		if !s.Comparable && s.StatusA == snapshot.Absent && s.StatusB == snapshot.Absent {
			continue
		}
		if s.Comparable && len(s.OnlyA)+len(s.OnlyB)+len(s.Changed)+len(s.Same) == 0 {
			continue
		}
		if s.Kind == kind {
			v.sp = len(v.secs)
		}
		v.secs = append(v.secs, i)
	}
	if prefix != "" {
		for i, p := range v.children() {
			if p == prefix {
				v.cp = i
			}
		}
	}
	v.clamp()
}

func (v *view) labels() [2]string { return [2]string{v.res.A.Label, v.res.B.Label} }

func (v *view) section() *diff.Section {
	if v.res == nil || len(v.secs) == 0 || v.sp >= len(v.secs) {
		return nil
	}
	return &v.res.Sections[v.secs[v.sp]]
}

func prefixOf(key string) string {
	if p, _, ok := strings.Cut(key, " › "); ok {
		return p
	}
	return ""
}

// children lists the groups inside the selected section (formula, cask,
// tap; python3.12, gem, …), when there are at least two.
func (v *view) children() []string {
	s := v.section()
	if s == nil {
		return nil
	}
	seen := map[string]bool{}
	add := func(key string) {
		if p := prefixOf(key); p != "" {
			seen[p] = true
		}
	}
	for _, it := range s.OnlyA {
		add(it.Key)
	}
	for _, it := range s.OnlyB {
		add(it.Key)
	}
	for _, c := range s.Changed {
		add(c.Key)
	}
	for _, it := range s.Same {
		add(it.Key)
	}
	if len(seen) < 2 {
		return nil
	}
	var out []string
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (v *view) prefix() string {
	kids := v.children()
	if v.cp < 0 || v.cp >= len(kids) {
		return ""
	}
	return kids[v.cp]
}

// sectionRows lists a section's rows, limited to a group prefix when given.
func sectionRows(s *diff.Section, prefix string, same, deps bool) []row {
	if s == nil || !s.Comparable {
		return nil
	}
	in := func(key string) bool { return prefix == "" || strings.HasPrefix(key, prefix+" › ") }
	var out []row
	for i := range s.OnlyA {
		it := &s.OnlyA[i]
		if (it.Tag != "dependency" || deps) && in(it.Key) {
			out = append(out, row{kind: s.Kind, mark: "◀", key: it.Key, a: it.Value, item: it})
		}
	}
	for i := range s.OnlyB {
		it := &s.OnlyB[i]
		if (it.Tag != "dependency" || deps) && in(it.Key) {
			out = append(out, row{kind: s.Kind, mark: "▶", key: it.Key, b: it.Value, item: it})
		}
	}
	for i := range s.Changed {
		c := &s.Changed[i]
		if (c.TagA != "dependency" || c.TagB != "dependency" || deps) && in(c.Key) {
			a, b := c.A, c.B
			if a == b {
				a, b = "(content)", "(content)"
			}
			out = append(out, row{kind: s.Kind, mark: "≠", key: c.Key, a: a, b: b, change: c})
		}
	}
	if same {
		for i := range s.Same {
			it := &s.Same[i]
			if in(it.Key) {
				out = append(out, row{kind: s.Kind, mark: "=", key: it.Key, a: it.Value, b: it.Value, item: it})
			}
		}
	}
	return out
}

func (v *view) rows() []row {
	rows := sectionRows(v.section(), v.prefix(), v.showSame, v.showDeps)
	if v.filter == "" {
		return rows
	}
	f := strings.ToLower(v.filter)
	var out []row
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.key+" "+r.a+" "+r.b), f) {
			out = append(out, r)
		}
	}
	return out
}

func (v *view) bodyHeight() int { return max(3, v.height-4) }

func (v *view) clamp() {
	n := len(v.rows())
	v.row = max(0, min(v.row, n-1))
	h := v.bodyHeight() - 1 // the table header
	if v.row < v.top {
		v.top = v.row
	}
	if v.row >= v.top+h {
		v.top = v.row - h + 1
	}
	v.top = max(0, v.top)
}

func selKey(kind, key string) string { return kind + "\x00" + key }

// actions returns what hostdiff can do about a row, on each side.
func actions(r row) []sideAction {
	var out []sideAction
	add := func(side int, a fix.Action, ok bool) {
		if ok {
			out = append(out, sideAction{side, a})
		}
	}
	switch r.mark {
	case "◀":
		a, ok := fix.ForMissing(r.kind, *r.item)
		add(1, a, ok)
		a, ok = fix.ForRemove(r.kind, *r.item)
		add(0, a, ok)
	case "▶":
		a, ok := fix.ForMissing(r.kind, *r.item)
		add(0, a, ok)
		a, ok = fix.ForRemove(r.kind, *r.item)
		add(1, a, ok)
	case "≠":
		c := r.change
		a, ok := fix.ForUpdate(r.kind, c.Key, c.B, c.TagB, c.A, c.TagA)
		add(0, a, ok)
		a, ok = fix.ForUpdate(r.kind, c.Key, c.A, c.TagA, c.B, c.TagB)
		add(1, a, ok)
		a, ok = fix.ForRemove(r.kind, snapshot.Item{Key: c.Key, Value: c.A, Tag: c.TagA})
		add(0, a, ok)
		a, ok = fix.ForRemove(r.kind, snapshot.Item{Key: c.Key, Value: c.B, Tag: c.TagB})
		add(1, a, ok)
	case "=":
		a, ok := fix.ForRemove(r.kind, *r.item)
		add(0, a, ok)
		add(1, a, ok)
	}
	return out
}

func (v *view) canChange() bool {
	for side := 0; side < 2; side++ {
		if s := v.b.Side(side); s.Prepare != nil && s.NoInstall == "" {
			return true
		}
	}
	return false
}

// targets are the selected rows in every section, or the current row.
func (v *view) targets() []row {
	if len(v.sel) == 0 {
		rows := v.rows()
		if v.row >= 0 && v.row < len(rows) {
			return rows[v.row : v.row+1]
		}
		return nil
	}
	var out []row
	for i := range v.res.Sections {
		for _, r := range sectionRows(&v.res.Sections[i], "", true, true) {
			if v.sel[selKey(r.kind, r.key)] {
				out = append(out, r)
			}
		}
	}
	return out
}

var verbOrder = map[string]int{fix.Install: 0, fix.Update: 1, fix.Set: 2, fix.Remove: 3, fix.Reset: 4}

func (v *view) sideProblem(side int) string {
	s := v.b.Side(side)
	switch {
	case s.NoInstall != "":
		return "cannot change " + v.labels()[side] + ": " + s.NoInstall
	case s.Prepare == nil:
		return "changes are not available in this view"
	}
	return ""
}

func (v *view) openMenu() {
	rows := v.targets()
	if len(rows) == 0 {
		return
	}
	byKey := map[[2]int]*choice{}
	var list []*choice
	for _, r := range rows {
		for _, sa := range actions(r) {
			id := [2]int{verbOrder[sa.act.Verb], sa.side}
			c := byKey[id]
			if c == nil {
				c = &choice{verb: sa.act.Verb, side: sa.side}
				byKey[id] = c
				list = append(list, c)
			}
			if sa.act.Runnable() {
				c.acts = append(c.acts, sa.act)
			} else {
				c.notes = append(c.notes, sa.act)
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if verbOrder[list[i].verb] != verbOrder[list[j].verb] {
			return verbOrder[list[i].verb] < verbOrder[list[j].verb]
		}
		return list[i].side < list[j].side
	})
	v.menu = nil
	// Details only add something when there is content to show; plain
	// values are already in the table.
	if r := rows[0]; len(v.sel) == 0 && ((r.change != nil && r.change.DetailA+r.change.DetailB != "") || (r.item != nil && r.item.Detail != "")) {
		v.menu = append(v.menu, choice{details: true})
	}
	var reason string
	for _, c := range list {
		if len(c.acts) == 0 {
			if len(c.notes) > 0 && reason == "" {
				reason = c.notes[0].Key + ": " + c.notes[0].Note
			}
			continue
		}
		c.disabled = v.sideProblem(c.side)
		v.menu = append(v.menu, *c)
	}
	if len(v.menu) == 0 || (len(v.menu) == 1 && v.menu[0].details) {
		if reason == "" {
			reason = "hostdiff has no command for " + rows[0].key
		}
		if len(v.menu) == 1 {
			v.menu = nil
			v.openDetail()
		}
		v.status = reason
		v.menu = nil
		return
	}
	v.menuTitle = rows[0].key
	if len(rows) > 1 {
		v.menuTitle = fmt.Sprintf("%d selected items", len(rows))
	}
	v.menuCur = 0
}

func (v *view) openCloneMenu() {
	l := v.labels()
	v.menu = nil
	for side := 0; side < 2; side++ {
		c := choice{clone: true, side: side}
		for _, a := range fix.Clone(v.res, side == 0) {
			if a.Runnable() {
				c.acts = append(c.acts, a)
			} else {
				c.notes = append(c.notes, a)
			}
		}
		c.disabled = v.sideProblem(side)
		if len(c.acts) == 0 && c.disabled == "" {
			c.disabled = l[side] + " already matches " + l[1-side] + " as far as hostdiff can change it"
		}
		v.menu = append(v.menu, c)
	}
	v.menuTitle = "Clone: make one machine like the other (in the compared groups)"
	v.menuCur = 0
}

func (v *view) choiceLabel(c choice) string {
	l := v.labels()
	on, other := l[c.side], l[1-c.side]
	if c.details {
		return "Show details"
	}
	var s string
	switch {
	case c.clone:
		s = fmt.Sprintf("Make %s like %s", on, other)
		counts := map[string]int{}
		for _, a := range c.acts {
			counts[a.Verb]++
		}
		var parts []string
		for _, verb := range []string{fix.Install, fix.Update, fix.Set, fix.Remove, fix.Reset} {
			if counts[verb] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[verb], verb))
			}
		}
		if len(parts) > 0 {
			s += ": " + strings.Join(parts, ", ")
		}
		return s
	case c.verb == fix.Install:
		s = "Install on " + on
	case c.verb == fix.Update:
		s = fmt.Sprintf("Update on %s to %s's version", on, other)
	case c.verb == fix.Set:
		s = fmt.Sprintf("Set on %s as on %s", on, other)
	case c.verb == fix.Remove:
		s = "Remove from " + on
	case c.verb == fix.Reset:
		s = "Reset to the default on " + on
	}
	if total := len(c.acts) + len(c.notes); total > 1 {
		if len(c.notes) > 0 {
			s += fmt.Sprintf(" (%d of %d)", len(c.acts), total)
		} else {
			s += fmt.Sprintf(" (%d)", total)
		}
	}
	return s
}

func verbTitle(verb string) string {
	return map[string]string{fix.Install: "Install", fix.Update: "Update", fix.Set: "Set", fix.Remove: "Remove", fix.Reset: "Reset"}[verb]
}

func (v *view) openConfirm(c choice) {
	l := v.labels()
	side := v.b.Side(c.side)
	on := l[c.side]
	if side.Remote {
		on += " (" + side.Where + ")"
	} else {
		on += " (this machine)"
	}
	lines := []string{
		fmt.Sprintf("hostdiff will run these %d commands on %s, in this order, exactly as written:", len(c.acts), on),
		"",
	}
	removals := 0
	prev := ""
	for i, a := range c.acts {
		if a.Verb != prev {
			lines = append(lines, verbTitle(a.Verb))
			prev = a.Verb
		}
		lines = append(lines, fmt.Sprintf("  %3d  %s", i+1, a.Command()))
		if a.Verb == fix.Remove {
			removals++
		}
	}
	lines = append(lines, "")
	if removals > 0 {
		lines = append(lines, fmt.Sprintf("Remove: this deletes %d items from %s.", removals, l[c.side]), "")
	}
	if len(c.notes) > 0 {
		lines = append(lines, fmt.Sprintf("# Not included, no command (%d):", len(c.notes)))
		for _, n := range c.notes {
			lines = append(lines, "#   "+n.Key+": "+n.Note)
		}
		lines = append(lines, "")
	}
	how := "# How: the commands go into a temporary script (only readable by you), run with /bin/sh in this terminal, then deleted."
	if side.Remote {
		how = fmt.Sprintf("# How: the commands go into a temporary script, copied to %s over ssh, run there with /bin/sh in this terminal (ssh -t), then deleted.", l[c.side])
	}
	lines = append(lines,
		how,
		"# Before each command the script prints it with its number; you can answer password prompts.",
		"# A failed command does not stop the rest; Ctrl-C stops after the current one. Nothing else is run.",
		"# Afterwards the affected groups are scanned again (read only). Tab shows the full script.")
	v.confirm, v.typed, v.confirmed = &c, "", nil
	v.detail, v.detailTop = lines, 0
	v.detailFor = "Confirm: y runs these commands, esc cancels"
	if v.needTyped() {
		v.detailFor = "Confirm: type yes and press enter to run, esc cancels"
	}
	v.menu = nil
}

// toggleScript switches the confirmation between the command list and the
// full script that will run.
func (v *view) toggleScript() {
	if v.confirm == nil {
		return
	}
	if v.confirmed != nil {
		v.detail, v.confirmed, v.detailTop = v.confirmed, nil, 0
		return
	}
	v.confirmed = v.detail
	script := fix.Installer(v.labels()[v.confirm.side], v.confirm.acts, true)
	v.detail = append([]string{"# The full script, exactly as it will run. Tab goes back to the list.", ""}, strings.Split(strings.TrimRight(script, "\n"), "\n")...)
	v.detailTop = 0
}

// needTyped reports whether the open confirmation wants "yes" typed out: a
// clone that removes things.
func (v *view) needTyped() bool {
	if v.confirm == nil || !v.confirm.clone {
		return false
	}
	for _, a := range v.confirm.acts {
		if a.Verb == fix.Remove {
			return true
		}
	}
	return false
}

func lookup(s *snapshot.Snapshot, kind, key string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, sec := range s.Sections {
		if sec.Kind != kind {
			continue
		}
		for _, it := range sec.Items {
			if it.Key == key {
				return it.Value, true
			}
		}
	}
	return "", false
}

func (v *view) snapOf(res *diff.Result, side int) *snapshot.Snapshot {
	if side == 0 {
		return res.A.Snap
	}
	return res.B.Snap
}

// run starts the confirmed commands.
func (v *view) run() tea.Cmd {
	c := *v.confirm
	v.confirm, v.detail, v.typed, v.confirmed = nil, nil, "", nil
	j := job{choice: c, before: map[string]string{}, had: map[string]bool{}}
	snap := v.snapOf(v.res, c.side)
	for _, a := range c.acts {
		val, ok := lookup(snap, a.Kind, a.Key)
		j.before[selKey(a.Kind, a.Key)], j.had[selKey(a.Kind, a.Key)] = val, ok
	}
	label := v.labels()[c.side]
	prepare := v.b.Side(c.side).Prepare
	script := fix.Installer(label, c.acts, true)
	v.busy = true
	v.status = "starting on " + label + "…"
	return func() tea.Msg {
		cmd, cleanup, err := prepare(script)
		return preparedMsg{job: j, cmd: cmd, cleanup: cleanup, err: err}
	}
}

// handle processes the messages of a run.
func (v *view) handle(msg tea.Msg) tea.Cmd {
	l := v.labels()
	switch msg := msg.(type) {
	case preparedMsg:
		if msg.err != nil {
			v.busy = false
			v.status = fmt.Sprintf("could not start on %s: %v", l[msg.job.side], msg.err)
			return nil
		}
		v.status = "running on " + l[msg.job.side] + "…"
		return tea.ExecProcess(msg.cmd, func(err error) tea.Msg { return ranMsg{preparedMsg: msg, err: err} })
	case ranMsg:
		if msg.cleanup != nil {
			msg.cleanup()
		}
		seen := map[string]bool{}
		var kinds []string
		for _, a := range msg.job.acts {
			if !seen[a.Kind] {
				seen[a.Kind] = true
				kinds = append(kinds, a.Kind)
			}
		}
		v.status = "scanning " + strings.Join(kinds, ", ") + " on " + l[msg.job.side] + " again…"
		b, j, all := v.b, msg.job, v.kinds
		return func() tea.Msg {
			if err := b.Collect(j.side, kinds, nil); err != nil {
				return refreshedMsg{job: j, err: err}
			}
			return refreshedMsg{job: j, res: b.Result(all)}
		}
	case refreshedMsg:
		v.busy = false
		for _, a := range msg.job.acts {
			delete(v.sel, selKey(a.Kind, a.Key))
		}
		if msg.err != nil {
			v.status = "scanning again failed: " + msg.err.Error()
			return nil
		}
		snap := v.snapOf(msg.res, msg.job.side)
		worked := 0
		for _, a := range msg.job.acts {
			k := selKey(a.Kind, a.Key)
			val, has := lookup(snap, a.Kind, a.Key)
			switch a.Verb {
			case fix.Install:
				if has {
					worked++
				}
			case fix.Remove, fix.Reset:
				if !has {
					worked++
				}
			default:
				if has && (!msg.job.had[k] || val != msg.job.before[k]) {
					worked++
				}
			}
		}
		v.setResult(msg.res)
		done := map[string]string{fix.Install: "installed", fix.Update: "updated", fix.Set: "set", fix.Remove: "removed", fix.Reset: "reset"}[msg.job.verb]
		if msg.job.clone {
			done = "changes took effect"
		}
		v.status = fmt.Sprintf("%s: %d of %d %s", l[msg.job.side], worked, len(msg.job.acts), done)
		if worked < len(msg.job.acts) {
			v.status += " (the command output said why; the rest are still listed)"
		}
	}
	return nil
}

func (v *view) openDetail() {
	rows := v.rows()
	if v.row < 0 || v.row >= len(rows) {
		return
	}
	r := rows[v.row]
	l := v.labels()
	var lines []string
	switch {
	case r.change != nil && r.change.DetailA != r.change.DetailB:
		lines = strings.Split(strings.TrimRight(render.Unified(v.res, *r.change), "\n"), "\n")
		lines = append([]string{l[0] + ": " + r.change.A, l[1] + ": " + r.change.B, ""}, lines...)
	case r.change != nil:
		lines = []string{l[0] + ": " + r.change.A, l[1] + ": " + r.change.B}
	case r.item != nil:
		side := map[string]string{"◀": "only on " + l[0], "▶": "only on " + l[1], "=": "same on both"}[r.mark]
		lines = []string{side, "value: " + r.item.Value}
		if r.item.Detail != "" {
			lines = append(lines, "")
			lines = append(lines, strings.Split(strings.TrimRight(r.item.Detail, "\n"), "\n")...)
		}
	}
	v.detail, v.detailFor, v.detailTop = lines, r.key, 0
}

// key handles a key on the comparison screen.
func (v *view) key(k string, msg tea.KeyMsg) (tea.Cmd, nav) {
	if v.busy {
		return nil, navNone
	}
	if v.filtering {
		switch msg.Type {
		case tea.KeyEnter:
			v.filtering = false
		case tea.KeyEsc:
			v.filtering, v.filter = false, ""
		case tea.KeyBackspace:
			if r := []rune(v.filter); len(r) > 0 {
				v.filter = string(r[:len(r)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			v.filter += string(msg.Runes)
		}
		v.row, v.top = 0, 0
		return nil, navNone
	}
	if v.detail != nil {
		h := v.bodyHeight()
		if v.confirm != nil && msg.Type == tea.KeyTab {
			v.toggleScript()
			return nil, navNone
		}
		if v.needTyped() {
			switch msg.Type {
			case tea.KeyEnter:
				if v.typed == "yes" {
					return v.run(), navNone
				}
				v.typed = ""
			case tea.KeyEsc:
				v.confirm, v.detail, v.typed, v.confirmed = nil, nil, "", nil
			case tea.KeyBackspace:
				if r := []rune(v.typed); len(r) > 0 {
					v.typed = string(r[:len(r)-1])
				}
			case tea.KeyRunes:
				v.typed += string(msg.Runes)
			case tea.KeyDown:
				v.detailTop++
			case tea.KeyUp:
				v.detailTop--
			}
			v.detailTop = max(0, min(v.detailTop, len(v.detail)-h))
			return nil, navNone
		}
		switch k {
		case "y":
			if v.confirm != nil {
				return v.run(), navNone
			}
		case "q", "esc", "enter", "left", "h", "n":
			v.detail, v.confirm, v.confirmed = nil, nil, nil
		case "down", "j":
			v.detailTop++
		case "up", "k":
			v.detailTop--
		case "pgdown", " ", "f":
			v.detailTop += h
		case "pgup", "b":
			v.detailTop -= h
		case "g", "home":
			v.detailTop = 0
		case "G", "end":
			v.detailTop = len(v.detail) - h
		}
		v.detailTop = max(0, min(v.detailTop, len(v.detail)-h))
		return nil, navNone
	}
	if v.menu != nil {
		switch k {
		case "down", "j":
			v.menuCur = min(v.menuCur+1, len(v.menu)-1)
		case "up", "k":
			v.menuCur = max(v.menuCur-1, 0)
		case "esc", "q", "left", "h":
			v.menu = nil
		case "enter", "right", "l":
			c := v.menu[v.menuCur]
			switch {
			case c.details:
				v.menu = nil
				v.openDetail()
			case c.disabled != "":
				v.status = c.disabled
			default:
				v.openConfirm(c)
			}
		}
		return nil, navNone
	}

	v.status = ""
	switch k {
	case "q":
		return nil, navQuit
	case "c":
		return nil, navGroups
	case "m":
		return nil, navMachines
	case "r":
		return nil, navRescan
	case "tab", "right", "l":
		if !v.right {
			v.right = true
		} else if k == "tab" {
			v.right = false
		}
	case "shift+tab", "left", "h", "esc":
		v.right = false
	case "down", "j":
		if v.right {
			v.row++
		} else {
			v.down()
		}
	case "up", "k":
		if v.right {
			v.row--
		} else {
			v.up()
		}
	case "J", "]":
		if v.sp < len(v.secs)-1 {
			v.sp, v.cp, v.row, v.top = v.sp+1, -1, 0, 0
		}
	case "K", "[":
		if v.sp > 0 {
			v.sp, v.cp, v.row, v.top = v.sp-1, -1, 0, 0
		}
	case "pgdown":
		v.row += v.bodyHeight()
	case "pgup":
		v.row -= v.bodyHeight()
	case "g", "home":
		v.row = 0
	case "G", "end":
		v.row = len(v.rows()) - 1
	case "enter":
		if !v.right {
			v.right = true
			break
		}
		v.openMenu()
	case " ":
		v.toggle()
	case "x":
		v.sel = map[string]bool{}
		v.status = "selection cleared"
	case "v":
		v.openDetail()
	case "C":
		v.openCloneMenu()
	case "/":
		v.filtering, v.filter, v.right = true, "", true
	case "a":
		v.showSame = !v.showSame
	case "d":
		v.showDeps = !v.showDeps
	case "s":
		l := v.labels()
		v.detail = strings.Split(strings.TrimRight(fix.Script(v.res), "\n"), "\n")
		v.detailFor = fmt.Sprintf("Script to make %s more like %s (review it, then copy what you need)", l[0], l[1])
		v.detailTop = 0
	}
	v.clamp()
	return nil, navNone
}

func (v *view) down() {
	if v.cp+1 < len(v.children()) {
		v.cp++
	} else if v.sp < len(v.secs)-1 {
		v.sp, v.cp = v.sp+1, -1
	}
	v.row, v.top = 0, 0
}

func (v *view) up() {
	if v.cp >= 0 {
		v.cp--
	} else if v.sp > 0 {
		v.sp--
	}
	v.row, v.top = 0, 0
}

// toggle selects the current row, or on the left every row of the group.
func (v *view) toggle() {
	rows := v.rows()
	if !v.right {
		if len(rows) == 0 {
			return
		}
		all := true
		for _, r := range rows {
			all = all && v.sel[selKey(r.kind, r.key)]
		}
		for _, r := range rows {
			if all {
				delete(v.sel, selKey(r.kind, r.key))
			} else {
				v.sel[selKey(r.kind, r.key)] = true
			}
		}
		if all {
			v.status = fmt.Sprintf("unselected %d items", len(rows))
		} else {
			v.status = fmt.Sprintf("selected %d items · enter shows what can be done", len(rows))
		}
		return
	}
	if v.row < 0 || v.row >= len(rows) {
		return
	}
	k := selKey(rows[v.row].kind, rows[v.row].key)
	if v.sel[k] {
		delete(v.sel, k)
	} else {
		v.sel[k] = true
	}
	v.row++
}

func describe(s diff.Side) string {
	if s.Snap == nil || s.Snap.Host.Name == "" {
		return s.Label
	}
	h := s.Snap.Host
	os := h.OS
	if os == "darwin" {
		os = "macOS"
	}
	return fmt.Sprintf("%s (%s, %s/%s)", s.Label, h.Name, os, h.Arch)
}

func (v *view) countFor(s *diff.Section, prefix string) int {
	n := 0
	for _, r := range sectionRows(s, prefix, false, false) {
		if r.mark != "=" {
			n++
		}
	}
	return n
}

func (v *view) render() string {
	w := v.width
	var b strings.Builder
	head := styleA.Render("◀ "+describe(v.res.A)) + "   " + styleB.Render("▶ "+describe(v.res.B))
	b.WriteString(truncate(head, w) + "\n")
	h := v.bodyHeight()

	if v.detail != nil {
		b.WriteString(styleTitle.Render(truncate(v.detailFor, w)) + "\n")
		end := min(len(v.detail), v.detailTop+h)
		for i := v.detailTop; i < end; i++ {
			l := v.detail[i]
			switch {
			case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
				l = styleBold.Render(l)
			case strings.HasPrefix(l, "-"):
				l = styleA.Render(l)
			case strings.HasPrefix(l, "+"):
				l = styleB.Render(l)
			case strings.HasPrefix(l, "@@"), strings.HasPrefix(l, "#"):
				l = styleDim.Render(l)
			case strings.HasPrefix(l, "Remove"):
				l = styleBad.Render(l)
			case v.confirm != nil && v.confirmed == nil && (l == "Install" || l == "Update" || l == "Set" || l == "Reset"):
				l = styleBold.Render(l)
			}
			b.WriteString(truncate(l, w) + "\n")
		}
		for i := end - v.detailTop; i < h; i++ {
			b.WriteString("\n")
		}
		footer := fmt.Sprintf("line %d/%d · ↑↓ scroll · esc back", min(v.detailTop+1, len(v.detail)), len(v.detail))
		switch {
		case v.needTyped():
			footer = "type yes and press enter to run: " + v.typed + "▏ · tab full script · esc cancels"
		case v.confirm != nil:
			footer = "y run · tab full script · esc cancel · ↑↓ scroll"
		}
		b.WriteString(styleBold.Render(truncate(footer, w)))
		return b.String()
	}

	lw := min(30, max(18, w/4))
	rw := max(20, w-lw-3)
	info := fmt.Sprintf("%d differences", v.res.Differences())
	if n := len(v.sel); n > 0 {
		info += fmt.Sprintf(" · %d selected · enter: what to do with them", n)
	}
	switch {
	case v.status != "":
		b.WriteString(styleBold.Render(truncate(v.status, w)) + "\n")
	default:
		b.WriteString(styleDim.Render(truncate(info, w)) + "\n")
	}

	var left []string
	selLine := 0
	for i, si := range v.secs {
		s := &v.res.Sections[si]
		count := fmt.Sprint(v.countFor(s, ""))
		if !s.Comparable {
			count = "–"
		}
		label := padTo(truncate(s.Title, lw-5), lw-4) + fmt.Sprintf("%4s", count)
		switch {
		case i == v.sp && v.cp < 0 && !v.right:
			label = styleSel.Render(label)
			selLine = len(left)
		case i == v.sp:
			label = styleBold.Render(label)
		case count == "0":
			label = styleDim.Render(label)
		}
		left = append(left, label)
		if i != v.sp {
			continue
		}
		for j, p := range v.children() {
			c := v.countFor(s, p)
			l := "  " + padTo(truncate(p, lw-7), lw-6) + fmt.Sprintf("%4d", c)
			switch {
			case j == v.cp && !v.right:
				l = styleSel.Render(l)
				selLine = len(left)
			case j == v.cp:
				l = styleBold.Render(l)
			case c == 0:
				l = styleDim.Render(l)
			}
			left = append(left, l)
		}
	}
	if off := selLine - h + 1; off > 0 {
		left = left[off:]
	}

	right := v.renderRight(rw, h)

	for i := 0; i < h; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		b.WriteString(padTo(l, lw) + " │ " + truncate(r, rw) + "\n")
	}
	footer := "↑↓ move · tab/enter open · v details · / filter · a same · d deps · s script · c groups · m machines · r rescan · q quit"
	if v.canChange() {
		footer = "space select · enter actions · C clone · v details · / filter · a same · d deps · c groups · m machines · r rescan · q quit"
	}
	if v.busy {
		footer = "working… (ctrl+c quits hostdiff)"
	}
	if v.filtering || v.filter != "" {
		footer = "filter: " + v.filter
		if v.filtering {
			footer += "▏ (enter keep · esc clear)"
		}
	}
	b.WriteString(styleDim.Render(truncate(footer, w)))
	return b.String()
}

func (v *view) renderRight(rw, h int) []string {
	var out []string
	s := v.section()
	rows := v.rows()
	switch {
	case s == nil:
		return []string{"Nothing to compare."}
	case !s.Comparable:
		out = append(out, styleBold.Render(s.Title)+" could not be compared:")
		l := v.labels()
		for _, n := range []struct{ label, status, note string }{{l[0], string(s.StatusA), s.NoteA}, {l[1], string(s.StatusB), s.NoteB}} {
			out = append(out, fmt.Sprintf("  %s: %s %s", n.label, n.status, n.note))
		}
		return append(out, "", styleDim.Render("Run `hostdiff snap -o FILE` in a Terminal on that machine and compare the files."))
	}
	if v.menu != nil {
		out = append(out, styleTitle.Render(truncate(v.menuTitle, rw)), "")
		for i, c := range v.menu {
			l := v.choiceLabel(c)
			if c.disabled != "" {
				l = styleDim.Render(l + " · " + c.disabled)
			}
			if i == v.menuCur {
				l = styleSel.Render("› " + ansi.Strip(l))
			} else {
				l = "  " + l
			}
			out = append(out, l)
		}
		return append(out, "", styleDim.Render("↑↓ choose · enter continue · esc back"))
	}
	if len(rows) == 0 {
		msg := "No differences."
		if v.filter != "" {
			msg = "Nothing matches the filter."
		}
		return []string{styleDim.Render(msg)}
	}
	prefix := v.prefix()
	name := func(r row) string {
		if prefix != "" {
			return strings.TrimPrefix(r.key, prefix+" › ")
		}
		return r.key
	}
	kw := 4
	for _, r := range rows {
		kw = max(kw, ansi.StringWidth(name(r)))
	}
	pick := v.canChange() || len(v.sel) > 0
	fixed := 2 + 2 + 2 + 1 // mark, selection, gaps
	kw = min(kw, (rw-fixed)/2)
	vw := max(6, (rw-fixed-kw)/2)
	l := v.labels()
	head := "  "
	if pick {
		head += "  "
	}
	head += padTo("", kw) + "  " + padTo(truncate(l[0], vw-1), vw) + " " + truncate(l[1], vw)
	out = append(out, styleDim.Render(head))
	cell := func(s string, style func(...string) string) string {
		if s == "" {
			return padTo(styleDim.Render("—"), vw)
		}
		return padTo(style(truncate(s, vw-1)), vw)
	}
	end := min(len(rows), v.top+h-1)
	for i := v.top; i < end; i++ {
		r := rows[i]
		mark := styleDim.Render(r.mark)
		switch r.mark {
		case "◀":
			mark = styleA.Render(r.mark)
		case "▶":
			mark = styleB.Render(r.mark)
		case "≠":
			mark = styleCh.Render(r.mark)
		}
		line := mark + " "
		if pick {
			if v.sel[selKey(r.kind, r.key)] {
				line += styleBold.Render("●") + " "
			} else {
				line += "  "
			}
		}
		plain := func(s ...string) string { return strings.Join(s, "") }
		line += padTo(truncate(name(r), kw), kw) + "  " + cell(r.a, plain) + " " + cell(r.b, plain)
		if i == v.row && v.right {
			line = styleSel.Render(padTo(ansi.Strip(line), rw))
		}
		out = append(out, line)
	}
	return out
}
