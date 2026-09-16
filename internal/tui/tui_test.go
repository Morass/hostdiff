package tui

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/snapshot"
)

// fakeBackend serves fixed snapshots of "laptop" (this machine) and "desk".
type fakeBackend struct {
	full      [2]*snapshot.Snapshot
	have      [2]*snapshot.Snapshot
	labels    [2]string
	sides     [2]Side
	collected []string
	scripts   []string
	results   map[int]int
	onCollect func(side int, s *snapshot.Snapshot)
}

func sec(kind, title string, items ...snapshot.Item) snapshot.Section {
	s := snapshot.Section{Kind: kind, Title: title, Status: snapshot.OK, Items: items}
	s.Sort()
	return s
}

func newFake() *fakeBackend {
	host := func(n string) snapshot.Host { return snapshot.Host{Name: n, OS: "darwin", Arch: "arm64"} }
	laptop := &snapshot.Snapshot{Format: 1, Created: time.Unix(0, 0), Host: host("lap"), Sections: []snapshot.Section{
		sec("brew", "Homebrew",
			snapshot.Item{Key: "formula › jq", Value: "1.7.1", Tag: "requested"},
			snapshot.Item{Key: "formula › node", Value: "26", Tag: "requested"},
			snapshot.Item{Key: "formula › git", Value: "2.50", Tag: "requested"},
			snapshot.Item{Key: "cask › rectangle", Value: "0.87"}),
		sec("libraries", "Language libraries",
			snapshot.Item{Key: "python3.12 › requests", Value: "2.32.3", Tag: "user"},
			snapshot.Item{Key: "gem › rake", Value: "13.2.1"}),
		sec("dotfiles", "Dotfiles", snapshot.Item{Key: "~/.zshrc", Value: "content 1", Detail: "alias ll='ls -l'\n"}),
	}}
	desk := &snapshot.Snapshot{Format: 1, Created: time.Unix(0, 0), Host: host("dsk"), Sections: []snapshot.Section{
		sec("brew", "Homebrew",
			snapshot.Item{Key: "formula › wget", Value: "1.25", Tag: "requested"},
			snapshot.Item{Key: "formula › node", Value: "25", Tag: "requested"},
			snapshot.Item{Key: "formula › git", Value: "2.50", Tag: "requested"},
			snapshot.Item{Key: "formula › oniguruma", Value: "6.9", Tag: "dependency"}),
		sec("libraries", "Language libraries",
			snapshot.Item{Key: "python3.12 › requests", Value: "2.31.0", Tag: "user"},
			snapshot.Item{Key: "python3.12 › numpy", Value: "2.0", Tag: "system"},
			snapshot.Item{Key: "gem › rake", Value: "13.2.1"}),
		sec("dotfiles", "Dotfiles", snapshot.Item{Key: "~/.zshrc", Value: "content 2", Detail: "alias la='ls -la'\n"}),
	}}
	f := &fakeBackend{full: [2]*snapshot.Snapshot{laptop, desk}}
	prep := func(script string) (*Started, error) {
		f.scripts = append(f.scripts, script)
		return &Started{
			Cmd:     exec.Command("true"),
			Results: func() (map[int]int, error) { return f.results, nil },
			Cleanup: func() {},
		}, nil
	}
	f.sides = [2]Side{{Where: "this machine", Prepare: prep}, {Where: "ssh desk", Remote: true, Prepare: prep}}
	return f
}

func (f *fakeBackend) Here() Machine { return Machine{Name: "laptop", Where: "this machine"} }
func (f *fakeBackend) Others() []Machine {
	return []Machine{{Name: "desk", Where: "ssh desk"}, {Name: "box", Where: "ssh box"}}
}
func (f *fakeBackend) Groups() []Group {
	return []Group{
		{Kind: "brew", Title: "Homebrew", Reads: "brew list"},
		{Kind: "libraries", Title: "Language libraries", Reads: "python, gems"},
		{Kind: "dotfiles", Title: "Dotfiles", Reads: "shell files"},
	}
}
func (f *fakeBackend) Select(a, b string) error {
	if a == b {
		return errors.New(a + " and " + b + " are the same machine")
	}
	f.labels = [2]string{a, b}
	return nil
}
func (f *fakeBackend) Labels() [2]string { return f.labels }
func (f *fakeBackend) Collect(side int, kinds []string, progress func(snapshot.Progress)) error {
	f.collected = append(f.collected, fmt.Sprint(side, kinds))
	if f.onCollect != nil {
		f.onCollect(side, f.full[side])
	}
	out := &snapshot.Snapshot{Format: 1, Host: f.full[side].Host}
	for _, s := range f.full[side].Sections {
		if slices.Contains(kinds, s.Kind) {
			if progress != nil {
				progress(snapshot.Progress{Stage: "start", Kind: s.Kind})
				progress(snapshot.Progress{Stage: "done", Kind: s.Kind, Items: len(s.Items), Status: s.Status})
			}
			out.Sections = append(out.Sections, s)
		}
	}
	f.have[side] = out
	return nil
}
func (f *fakeBackend) Result(kinds []string) *diff.Result {
	return diff.Compare(diff.Side{Label: f.labels[0], Snap: f.have[0]}, diff.Side{Label: f.labels[1], Snap: f.have[1]}, diff.Options{Only: kinds})
}
func (f *fakeBackend) Side(side int) Side { return f.sides[side] }

func press(a *App, keys ...string) tea.Cmd {
	var last tea.Cmd
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, last = a.Update(msg)
	}
	return last
}

// scanned drains the collection started by the app until the comparison is
// shown.
func scanned(t *testing.T, a *App) {
	t.Helper()
	for a.screen == screenScan {
		select {
		case msg := <-a.ch:
			a.Update(msg)
		case <-time.After(5 * time.Second):
			t.Fatal("scan never finished")
		}
	}
}

func started(t *testing.T, f *fakeBackend, st Start) *App {
	t.Helper()
	a := newApp(f, st)
	a.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	a.Init()
	scanned(t, a)
	return a
}

func must(t *testing.T, view string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(view, w) {
			t.Errorf("missing %q in:\n%s", w, view)
		}
	}
}

func TestGuidedFlowPicksMachineAndGroups(t *testing.T) {
	f := newFake()
	a := newApp(f, Start{})
	a.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	must(t, a.View(), "◀ laptop", "Compare with:", "desk", "ssh box")
	press(a, "enter")
	must(t, a.View(), "What should be compared?", "laptop and desk", "Homebrew", "nothing selected")
	press(a, "space", "enter")
	if a.screen != screenScan || !strings.Contains(a.View(), "Scanning") {
		t.Fatalf("not scanning:\n%s", a.View())
	}
	scanned(t, a)
	if len(f.collected) != 2 || f.collected[0] != "0 [brew]" && f.collected[1] != "0 [brew]" {
		t.Errorf("collected %v, want brew on both sides", f.collected)
	}
	v := a.View()
	must(t, v, "Homebrew", "formula › jq", "1.7.1", "—", "formula › node", "26", "25")
	if strings.Contains(v, "Dotfiles") || strings.Contains(v, "oniguruma") {
		t.Errorf("ungrouped section or dependency shown:\n%s", v)
	}
}

func TestSameMachineIsExplained(t *testing.T) {
	a := newApp(newFake(), Start{B: "laptop"})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	must(t, a.View(), "Could not compare", "same machine")
}

func TestChildGroupsSplitASection(t *testing.T) {
	a := started(t, newFake(), Start{B: "desk", Kinds: []string{"libraries"}})
	press(a, "j") // gem
	must(t, a.View(), "gem", "python3.12", "No differences.")
	press(a, "j") // python3.12
	v := a.View()
	must(t, v, "requests", "2.32.3", "2.31.0", "numpy")
	if strings.Contains(v, "python3.12 › requests") {
		t.Errorf("group prefix not stripped in the table:\n%s", v)
	}
}

func run(t *testing.T, a *App, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatalf("nothing started:\n%s", a.View())
	}
	prepared := cmd().(preparedMsg)
	_, exec := a.Update(prepared)
	if exec == nil {
		t.Fatalf("not executed: %+v\n%s", prepared.err, a.View())
	}
	_, refresh := a.Update(ranMsg{preparedMsg: prepared})
	a.Update(refresh())
}

func TestRemoveOneItem(t *testing.T) {
	f := newFake()
	a := started(t, f, Start{B: "desk", Kinds: []string{"brew"}})
	// Rows: ◀ cask › rectangle, ◀ formula › jq, ▶ formula › wget, ≠ formula › node.
	press(a, "tab", "j", "enter")
	must(t, a.View(), "formula › jq", "Install on desk", "Remove from laptop")
	press(a, "j", "enter")
	must(t, a.View(), "hostdiff will run these 1 commands on laptop (this machine), in this order, exactly as written:", "Remove", "  1  brew uninstall jq", "deletes 1 items from laptop", "y run")
	press(a, "tab")
	must(t, a.View(), "The full script, exactly as it will run", "#!/bin/sh")
	press(a, "tab")
	must(t, a.View(), "  1  brew uninstall jq")
	f.results = map[int]int{1: 0}
	f.onCollect = func(side int, s *snapshot.Snapshot) {
		if side == 0 {
			items := s.Sections[0].Items[:0]
			for _, it := range s.Sections[0].Items {
				if it.Key != "formula › jq" {
					items = append(items, it)
				}
			}
			s.Sections[0].Items = items
		}
	}
	run(t, a, press(a, "y"))
	if !strings.Contains(f.scripts[0], "step 1 1 'brew uninstall jq' brew uninstall jq") || f.collected[len(f.collected)-1] != "0 [brew]" {
		t.Errorf("script %q, collected %v", f.scripts, f.collected)
	}
	v := a.View()
	must(t, v, "laptop: 1 of 1 removed")
	if strings.Contains(v, "formula › jq") {
		t.Errorf("removed item still listed:\n%s", v)
	}
}

func TestSelectionAggregatesActions(t *testing.T) {
	a := started(t, newFake(), Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "tab", "space", "space", "space", "enter")
	must(t, a.View(), "3 selected items", "Install on laptop", "Install on desk (2)", "Remove from laptop (2)", "Remove from desk")
}

func TestUpdateToOtherVersion(t *testing.T) {
	a := started(t, newFake(), Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "tab", "j", "j", "j", "enter")
	must(t, a.View(), "Update on laptop to desk's version", "Update on desk to laptop's version", "Remove from laptop", "Remove from desk")
	press(a, "enter")
	must(t, a.View(), "Update", "  1  brew upgrade node")
}

func TestCloneAsksToTypeYesWhenRemoving(t *testing.T) {
	f := newFake()
	f.results = map[int]int{1: 0, 2: 0, 3: 0, 4: 0}
	a := started(t, f, Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "C")
	must(t, a.View(), "Make laptop like desk: 1 install, 1 update, 2 remove", "Make desk like laptop")
	press(a, "enter")
	must(t, a.View(), "type yes", "4 commands on laptop", "1  brew install wget", "2  brew upgrade node", "brew uninstall jq", "brew uninstall --cask rectangle", "deletes 2 items from laptop")
	if cmd := press(a, "y", "enter"); cmd != nil || a.view.confirm == nil {
		t.Fatal("clone ran without yes typed out")
	}
	press(a, "y", "e", "s")
	run(t, a, press(a, "enter"))
	if len(f.scripts) != 1 {
		t.Fatalf("scripts: %v", f.scripts)
	}
	s := f.scripts[0]
	if strings.Index(s, "brew install wget") > strings.Index(s, "brew uninstall") {
		t.Errorf("removals must come after installs:\n%s", s)
	}
	must(t, a.View(), "laptop: ")
}

func TestSnapshotSideCannotChange(t *testing.T) {
	f := newFake()
	f.sides[1] = Side{Where: "snapshot file", NoInstall: "it is a snapshot file"}
	a := started(t, f, Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "tab", "enter")
	must(t, a.View(), "Install on desk · cannot change desk: it is a snapshot file")
	press(a, "enter")
	if a.view.confirm != nil {
		t.Fatal("confirmation opened for a snapshot side")
	}
	must(t, a.View(), "cannot change desk")
}

func TestReadOnlyView(t *testing.T) {
	f := newFake()
	f.sides = [2]Side{}
	a := started(t, f, Start{B: "desk", Kinds: []string{"brew", "dotfiles"}})
	if strings.Contains(a.View(), "space select") {
		t.Errorf("change keys offered in a read-only view:\n%s", a.View())
	}
	press(a, "tab", "enter")
	must(t, a.View(), "changes are not available")
}

func TestDetailsScriptAndBackToGroups(t *testing.T) {
	f := newFake()
	a := started(t, f, Start{B: "desk", Kinds: []string{"brew", "dotfiles"}})
	press(a, "J", "tab", "v")
	must(t, a.View(), "-alias ll='ls -l'", "+alias la='ls -la'")
	press(a, "esc", "s")
	must(t, a.View(), "brew install wget")
	press(a, "esc", "c")
	if a.screen != screenGroups || !a.gsel["brew"] || !a.gsel["dotfiles"] || a.gsel["libraries"] {
		t.Fatalf("groups screen with the current choice: %v\n%s", a.gsel, a.View())
	}
	press(a, "enter")
	scanned(t, a)
	press(a, "m")
	must(t, a.View(), "Compare with:")
}

func TestFilterAndSame(t *testing.T) {
	a := started(t, newFake(), Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "a")
	must(t, a.View(), "formula › git")
	press(a, "/", "w", "g", "enter")
	v := a.View()
	must(t, v, "formula › wget", "filter: wg")
	if strings.Contains(v, "formula › jq") {
		t.Errorf("filter:\n%s", v)
	}
}

func TestKeyBurstAndTinyWindow(t *testing.T) {
	a := started(t, newFake(), Start{B: "desk", Kinds: []string{"brew", "libraries", "dotfiles"}})
	one := started(t, newFake(), Start{B: "desk", Kinds: []string{"brew", "libraries", "dotfiles"}})
	press(a, "tab", "jj")
	press(one, "tab", "j", "j")
	if a.view.row != one.view.row || a.view.row != 2 {
		t.Fatalf("burst row %d, separate %d", a.view.row, one.view.row)
	}
	a.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	press(a, "G", "enter", "j", "enter", "esc", "esc", "C", "esc", "/", "x", "enter", "c")
	_ = a.View()
	press(a, "esc")
	_ = a.View()
}

func TestRemoteConfirmationSaysHow(t *testing.T) {
	a := started(t, newFake(), Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "tab", "enter", "enter")
	must(t, a.View(), "on desk (ssh desk)", "copied to desk over ssh, run there with /bin/sh", "1  brew install --cask rectangle")
}

// A command that fails is reported as failed, marked in the table, and
// explained by "o" — never mistaken for a change that went through.
func TestFailedCommandIsReported(t *testing.T) {
	f := newFake()
	f.results = map[int]int{1: 1}
	a := started(t, f, Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "tab", "j", "enter", "j", "enter") // jq: remove from laptop
	run(t, a, press(a, "y"))
	v := a.View()
	must(t, v, "What the last run did on laptop", "✗   1  brew uninstall jq  (exit 1)")
	press(a, "esc")
	v = a.View()
	must(t, v, "laptop: 0 of 1 removed, 1 failed", "✗")
	press(a, "j", "k") // the summary survives moving around
	must(t, a.View(), "last run · laptop: 0 of 1 removed, 1 failed")
	if !strings.Contains(v, "formula › jq") {
		t.Errorf("an item that was not removed must stay in the table:\n%s", v)
	}
	press(a, "o")
	must(t, a.View(), "What the last run did on laptop", "its output is in the terminal above")
}

// Commands that never ran (Ctrl-C) are not counted as done.
func TestStoppedRunSaysNotRun(t *testing.T) {
	f := newFake()
	f.results = map[int]int{}
	a := started(t, f, Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "tab", "j", "enter", "enter") // jq: install on desk
	run(t, a, press(a, "y"))
	must(t, a.View(), "What the last run did on desk", "never ran")
	press(a, "esc")
	must(t, a.View(), "desk: 0 of 1 installed, 1 not run")
}

// Escape steps back: table → groups → machines, and back to the table.
func TestEscapeGoesBack(t *testing.T) {
	a := started(t, newFake(), Start{B: "desk", Kinds: []string{"brew"}})
	press(a, "tab", "esc") // the right pane hands focus back to the groups list
	if a.screen != screenView {
		t.Fatalf("esc left the table too early")
	}
	press(a, "esc")
	if a.screen != screenGroups {
		t.Fatalf("esc did not go back to the groups: %v", a.screen)
	}
	press(a, "esc")
	if a.screen != screenMachines {
		t.Fatalf("esc did not go back to the machines: %v", a.screen)
	}
	press(a, "esc")
	if a.screen != screenView {
		t.Fatalf("esc did not return to the comparison: %v", a.screen)
	}
}

// Anything that is not in the config file can be typed in the machine
// picker: an ssh destination, a snapshot file, NAME@last.
func TestCustomMachineIsTypedIn(t *testing.T) {
	f := newFake()
	a := newApp(f, Start{})
	a.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	must(t, a.View(), "something else…", "an ssh destination, a snapshot file, or NAME@last")
	press(a, "j", "j", "enter")
	must(t, a.View(), "Compare with:", "type a destination")
	press(a, "m", "e", "@", "b", "o", "x", "enter")
	if a.bName != "me@box" || a.screen != screenGroups {
		t.Fatalf("typed destination not used: %q, screen %v\n%s", a.bName, a.screen, a.View())
	}
}

func TestCustomMachineRefusedIsExplained(t *testing.T) {
	f := newFake()
	a := newApp(f, Start{})
	a.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	press(a, "j", "j", "enter")
	press(a, "l", "a", "p", "t", "o", "p", "enter")
	must(t, a.View(), "same machine")
	if a.screen != screenMachines {
		t.Fatalf("moved on after a refused machine")
	}
}
