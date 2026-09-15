package tui

import (
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
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m, _ = m.Update(msg)
	}
	return m
}

func start() tea.Model {
	m, _ := New(sample()).Update(tea.WindowSizeMsg{Width: 120, Height: 24})
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
	if !strings.Contains(v, "brew install 'jq'") || !strings.Contains(v, "Script to make desk more like laptop") {
		t.Fatalf("script view:\n%s", v)
	}
}

func TestSmallWindowDoesNotPanic(t *testing.T) {
	m, _ := New(sample()).Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	m = send(m, "tab", "G", "enter", "j", "esc", "/", "x", "enter")
	_ = m.View()
}
