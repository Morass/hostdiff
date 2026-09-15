package tui

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/snapshot"
)

func sample() *diff.Result {
	snap := func(name string) *snapshot.Snapshot {
		return &snapshot.Snapshot{Format: 1, Created: time.Unix(0, 0), Host: snapshot.Host{Name: name, OS: "darwin", Arch: "arm64"}}
	}
	return &diff.Result{
		A: diff.Side{Label: "laptop", Snap: snap("lap")},
		B: diff.Side{Label: "desk", Snap: snap("dsk")},
		Sections: []diff.Section{
			{Kind: "fonts", Title: "Fonts", Comparable: true, Same: []snapshot.Item{{Key: "Inter.ttf", Value: "user"}}},
			{Kind: "brew", Title: "Homebrew", Comparable: true,
				OnlyA:   []snapshot.Item{{Key: "formula › jq", Value: "1.7.1", Tag: "requested"}, {Key: "formula › oniguruma", Value: "6.9", Tag: "dependency"}},
				OnlyB:   []snapshot.Item{{Key: "formula › wget", Value: "1.25", Tag: "requested"}},
				Changed: []diff.Change{{Key: "formula › node", A: "26", B: "25", TagA: "requested", TagB: "requested"}},
				Same:    []snapshot.Item{{Key: "formula › git", Value: "2.50"}}},
			{Kind: "dotfiles", Title: "Dotfiles", Comparable: true,
				Changed: []diff.Change{{Key: "~/.zshrc", A: "content 1", B: "content 2", DetailA: "alias ll='ls -l'\n", DetailB: "alias la='ls -la'\n"}}},
			{Kind: "shortcuts", Title: "Shortcuts", StatusA: snapshot.Unavailable, NoteA: "over ssh", StatusB: snapshot.OK},
			{Kind: "mas", Title: "App Store", StatusA: snapshot.Absent, StatusB: snapshot.Absent},
		},
	}
}

func send(m tea.Model, keys ...string) tea.Model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m, _ = m.Update(msg)
	}
	return m
}

func start() tea.Model {
	m, _ := New(sample(), Options{}).Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	return m
}

func startWith(opt Options) tea.Model {
	m, _ := New(sample(), opt).Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	return m
}

func TestOpensOnFirstDifferenceAndHidesAbsent(t *testing.T) {
	v := start().View()
	if !strings.Contains(v, "◀ formula › jq") || !strings.Contains(v, "▶ formula › wget") || !strings.Contains(v, "26 │ 25") {
		t.Fatalf("brew not shown first:\n%s", v)
	}
	if strings.Contains(v, "oniguruma") {
		t.Errorf("dependency shown by default:\n%s", v)
	}
	if strings.Contains(v, "App Store") {
		t.Errorf("section absent on both sides listed:\n%s", v)
	}
	if strings.Contains(v, "formula › git") {
		t.Errorf("same items shown by default")
	}
}

func TestTogglesAndFilter(t *testing.T) {
	m := send(start(), "d", "a")
	v := m.View()
	if !strings.Contains(v, "oniguruma") || !strings.Contains(v, "formula › git") {
		t.Fatalf("toggles:\n%s", v)
	}
	m = send(start(), "/", "w", "g", "enter")
	v = m.View()
	if strings.Contains(v, "formula › jq") || !strings.Contains(v, "formula › wget") || !strings.Contains(v, "filter: wg") {
		t.Fatalf("filter:\n%s", v)
	}
	m = send(m, "/", "esc")
	if !strings.Contains(m.View(), "formula › jq") {
		t.Fatal("esc did not clear the filter")
	}
}

func TestDetailShowsContentDiff(t *testing.T) {
	// Down to Dotfiles, into the list, open the item.
	m := send(start(), "down", "enter", "enter")
	v := m.View()
	if !strings.Contains(v, "-alias ll='ls -l'") || !strings.Contains(v, "+alias la='ls -la'") {
		t.Fatalf("content diff missing:\n%s", v)
	}
	m = send(m, "esc")
	if strings.Contains(m.View(), "+alias") {
		t.Fatal("esc did not close the detail")
	}
}

func TestKeyBurstIsSplit(t *testing.T) {
	// "jj" arriving as one message must move twice, like two presses.
	one := send(start(), "tab", "j", "j")
	burst := send(start(), "tab", "jj")
	if one.(Model).row != burst.(Model).row || burst.(Model).row != 2 {
		t.Fatalf("burst row %d, separate row %d", burst.(Model).row, one.(Model).row)
	}
}

func TestUnavailableSectionExplains(t *testing.T) {
	m := send(start(), "down", "down")
	v := m.View()
	if !strings.Contains(v, "could not be compared") || !strings.Contains(v, "hostdiff snap -o FILE") {
		t.Fatalf("unavailable section:\n%s", v)
	}
}

func TestScriptView(t *testing.T) {
	v := send(start(), "s").View()
	if !strings.Contains(v, "brew install wget") || !strings.Contains(v, "Script to make laptop more like desk") {
		t.Fatalf("script view:\n%s", v)
	}
}

func TestSmallWindowDoesNotPanic(t *testing.T) {
	m, _ := New(sample(), Options{}).Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	m = send(m, "tab", "G", "enter", "j", "esc", "/", "x", "enter")
	_ = m.View()
}

func installable() (Options, *[]string) {
	var scripts []string
	prep := func(script string) (*exec.Cmd, func(), error) {
		scripts = append(scripts, script)
		return exec.Command("true"), func() {}, nil
	}
	return Options{Sides: [2]Side{{Where: "this machine", Prepare: prep}, {Where: "ssh desk", Prepare: prep}}}, &scripts
}

// Rows in brew: ◀ jq (only laptop, goes to desk), ▶ wget (goes to laptop),
// ≠ node (a version difference: nothing to install).
func TestMarkAndPlanBothSides(t *testing.T) {
	opt, _ := installable()
	m := send(startWith(opt), "tab", "space", "j", "space")
	v := m.View()
	if !strings.Contains(v, "marked") || !strings.Contains(v, "●") {
		t.Fatalf("marks not shown:\n%s", v)
	}
	m = send(m, "j", "space")
	if !strings.Contains(m.View(), "version differs") {
		t.Errorf("unmarkable row not explained:\n%s", m.View())
	}
	v = send(m, "i").View()
	for _, want := range []string{"On laptop (this machine), 1 commands:", "brew install wget", "On desk (ssh desk), 1 commands:", "brew install jq", "y runs them"} {
		if !strings.Contains(v, want) {
			t.Errorf("plan missing %q:\n%s", want, v)
		}
	}
	if v := send(m, "x", "i").View(); !strings.Contains(v, "mark items with space first") {
		t.Errorf("x did not clear:\n%s", v)
	}
}

func TestSectionMarkAndSnapshotSide(t *testing.T) {
	opt, _ := installable()
	opt.Sides[1] = Side{Where: "snapshot file", NoInstall: "it is a snapshot file"}
	m := send(startWith(opt), "space")
	if v := m.View(); !strings.Contains(v, "marked 1 items in Homebrew") {
		t.Errorf("section mark (only wget can go to the live side):\n%s", v)
	}
	m = send(startWith(opt), "tab", "space")
	if v := m.View(); !strings.Contains(v, "cannot install on desk: it is a snapshot file") {
		t.Errorf("snapshot side not refused:\n%s", v)
	}
}

// y prepares the installer, runs it, collects again and reports what now
// matches; the marks go away.
func TestInstallRunsAndRefreshes(t *testing.T) {
	opt, scripts := installable()
	var refreshed []string
	opt.Refresh = func(side int, kinds []string) (*diff.Result, error) {
		refreshed = append(refreshed, fmt.Sprint(side, kinds))
		r := sample()
		r.Sections[1].OnlyB = nil // wget is now on laptop too
		return r, nil
	}
	m := send(startWith(opt), "tab", "j", "space", "i")
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil || !strings.Contains(m.View(), "installing") {
		t.Fatalf("y did not start:\n%s", m.View())
	}
	prepared := cmd().(preparedMsg)
	if len(*scripts) != 1 || !strings.Contains((*scripts)[0], "'brew' 'install' 'wget'") {
		t.Fatalf("installer script: %q", *scripts)
	}
	m, _ = m.Update(prepared)
	m, cmd = m.Update(ranMsg{preparedMsg: prepared})
	m, _ = m.Update(cmd())
	v := m.View()
	if len(refreshed) != 1 || refreshed[0] != "0 [brew]" || !strings.Contains(v, "laptop: 1 of 1 installed items now match") {
		t.Fatalf("refresh %v:\n%s", refreshed, v)
	}
	if strings.Contains(v, "formula › wget") || strings.Contains(v, "●") {
		t.Errorf("installed item or mark still shown:\n%s", v)
	}
}

// A package installed at another version than the other machine's has been
// installed; a setting that still differs has not been applied.
func TestStillDifferent(t *testing.T) {
	res := &diff.Result{Sections: []diff.Section{
		{Kind: "brew", Changed: []diff.Change{{Key: "formula › jq", A: "1.8", B: "1.7"}}},
		{Kind: "defaults", Changed: []diff.Change{{Key: "dock › autohide", A: "true", B: "false"}}},
		{Kind: "packages", OnlyB: []snapshot.Item{{Key: "npm › x"}}},
	}}
	ids := []markID{{0, "brew", "formula › jq"}, {0, "defaults", "dock › autohide"}, {0, "packages", "npm › x"}}
	if n := stillDifferent(res, ids); n != 2 {
		t.Fatalf("still different = %d, want 2 (the setting and the missing npm package)", n)
	}
}
