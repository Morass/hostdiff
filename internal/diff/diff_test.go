package diff

import (
	"testing"

	"github.com/morass/hostdiff/internal/snapshot"
)

func snap(sections ...snapshot.Section) *snapshot.Snapshot {
	return &snapshot.Snapshot{Format: snapshot.Format, Sections: sections}
}

func items(kv ...string) []snapshot.Item {
	var out []snapshot.Item
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, snapshot.Item{Key: kv[i], Value: kv[i+1]})
	}
	return out
}

func TestCompare(t *testing.T) {
	a := snap(
		snapshot.Section{Kind: "brew", Title: "Homebrew", Status: snapshot.OK, Items: items("formula › jq", "1.7", "formula › node", "26", "formula › git", "2.50")},
		snapshot.Section{Kind: "dotfiles", Title: "Dotfiles", Status: snapshot.OK, Items: []snapshot.Item{{Key: "~/.zshrc", Value: "content x", Detail: "a\n"}}},
		snapshot.Section{Kind: "shortcuts", Title: "Shortcuts", Status: snapshot.OK, Items: items("Timer", "-")},
		snapshot.Section{Kind: "mas", Title: "App Store", Status: snapshot.OK, Items: items("Keynote", "14")},
	)
	b := snap(
		snapshot.Section{Kind: "brew", Title: "Homebrew", Status: snapshot.OK, Items: items("formula › node", "25", "formula › git", "2.50", "formula › wget", "1.2")},
		snapshot.Section{Kind: "dotfiles", Title: "Dotfiles", Status: snapshot.OK, Items: []snapshot.Item{{Key: "~/.zshrc", Value: "content x", Detail: "b\n"}}},
		snapshot.Section{Kind: "shortcuts", Title: "Shortcuts", Status: snapshot.Unavailable, Note: "privacy"},
		snapshot.Section{Kind: "mas", Title: "App Store", Status: snapshot.Absent},
	)
	r := Compare(Side{"a", a}, Side{"b", b}, Options{Ignore: []string{"brew:formula › git"}})
	brew := r.Sections[0]
	if len(brew.OnlyA) != 1 || brew.OnlyA[0].Key != "formula › jq" || len(brew.OnlyB) != 1 || len(brew.Changed) != 1 || brew.Ignored != 1 || len(brew.Same) != 0 {
		t.Fatalf("brew: %+v", brew)
	}
	if dot := r.Sections[1]; len(dot.Changed) != 1 {
		t.Fatalf("detail-only change not reported: %+v", dot)
	}
	if sc := r.Sections[2]; sc.Comparable || sc.Differences() != 0 {
		t.Fatalf("an unreadable side must not report items as missing: %+v", sc)
	}
	if m := r.Sections[3]; !m.Comparable || len(m.OnlyA) != 1 {
		t.Fatalf("not installed on one side should list the other's items: %+v", m)
	}
	if r.Differences() != 5 {
		t.Fatalf("differences %d", r.Differences())
	}
	only := Compare(Side{"a", a}, Side{"b", b}, Options{Only: []string{"mas"}})
	if len(only.Sections) != 1 || only.Sections[0].Kind != "mas" {
		t.Fatalf("only: %+v", only.Sections)
	}
}
