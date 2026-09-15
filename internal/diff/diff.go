// Package diff compares two snapshots section by section.
package diff

import (
	"github.com/morass/hostdiff/internal/config"
	"github.com/morass/hostdiff/internal/snapshot"
)

// Side names one of the two compared snapshots.
type Side struct {
	Label string             `json:"label"`
	Snap  *snapshot.Snapshot `json:"-"`
}

// Change is an item present on both sides with different content.
type Change struct {
	Key     string `json:"key"`
	A       string `json:"a"`
	B       string `json:"b"`
	DetailA string `json:"detail_a,omitempty"`
	DetailB string `json:"detail_b,omitempty"`
	TagA    string `json:"tag_a,omitempty"`
	TagB    string `json:"tag_b,omitempty"`
}

// Section is the comparison of one section.
type Section struct {
	Kind    string          `json:"kind"`
	Title   string          `json:"title"`
	StatusA snapshot.Status `json:"status_a"`
	StatusB snapshot.Status `json:"status_b"`
	NoteA   string          `json:"note_a,omitempty"`
	NoteB   string          `json:"note_b,omitempty"`
	// Comparable is false when one side could not be read (unavailable,
	// failed, or not collected); its items are then not reported as missing.
	Comparable bool            `json:"comparable"`
	OnlyA      []snapshot.Item `json:"only_a,omitempty"`
	OnlyB      []snapshot.Item `json:"only_b,omitempty"`
	Changed    []Change        `json:"changed,omitempty"`
	Same       []snapshot.Item `json:"same,omitempty"`
	Ignored    int             `json:"ignored,omitempty"`
}

// Differences counts reported differences.
func (s *Section) Differences() int { return len(s.OnlyA) + len(s.OnlyB) + len(s.Changed) }

// Result is a whole comparison.
type Result struct {
	A        Side      `json:"a"`
	B        Side      `json:"b"`
	Sections []Section `json:"sections"`
}

// Reversed returns the same comparison seen from the other side.
func (r *Result) Reversed() *Result {
	out := &Result{A: r.B, B: r.A, Sections: make([]Section, len(r.Sections))}
	for i, s := range r.Sections {
		s.StatusA, s.StatusB = s.StatusB, s.StatusA
		s.NoteA, s.NoteB = s.NoteB, s.NoteA
		s.OnlyA, s.OnlyB = s.OnlyB, s.OnlyA
		changed := make([]Change, len(s.Changed))
		for j, c := range s.Changed {
			changed[j] = Change{Key: c.Key, A: c.B, B: c.A, DetailA: c.DetailB, DetailB: c.DetailA, TagA: c.TagB, TagB: c.TagA}
		}
		s.Changed = changed
		out.Sections[i] = s
	}
	return out
}

// Differences counts differences across all sections.
func (r *Result) Differences() int {
	n := 0
	for i := range r.Sections {
		n += r.Sections[i].Differences()
	}
	return n
}

// Options narrow a comparison.
type Options struct {
	Only   []string
	Ignore []string
}

// Compare compares a with b.
func Compare(a, b Side, opt Options) *Result {
	r := &Result{A: a, B: b}
	var kinds []string
	seen := map[string]bool{}
	for _, snap := range []*snapshot.Snapshot{a.Snap, b.Snap} {
		for _, s := range snap.Sections {
			if !seen[s.Kind] {
				seen[s.Kind] = true
				kinds = append(kinds, s.Kind)
			}
		}
	}
	for _, kind := range kinds {
		if len(opt.Only) > 0 && !has(opt.Only, kind) {
			continue
		}
		sa, sb := a.Snap.Section(kind), b.Snap.Section(kind)
		r.Sections = append(r.Sections, compareSection(kind, sa, sb, opt.Ignore))
	}
	return r
}

func compareSection(kind string, sa, sb *snapshot.Section, ignore []string) Section {
	out := Section{Kind: kind}
	var itemsA, itemsB []snapshot.Item
	for _, p := range []struct {
		src    *snapshot.Section
		status *snapshot.Status
		note   *string
		items  *[]snapshot.Item
	}{{sa, &out.StatusA, &out.NoteA, &itemsA}, {sb, &out.StatusB, &out.NoteB, &itemsB}} {
		if p.src == nil {
			*p.status, *p.note = snapshot.Unavailable, "not collected"
			continue
		}
		if out.Title == "" {
			out.Title = p.src.Title
		}
		*p.status, *p.note, *p.items = p.src.Status, p.src.Note, p.src.Items
	}
	readable := func(s snapshot.Status) bool { return s == snapshot.OK || s == snapshot.Absent }
	out.Comparable = readable(out.StatusA) && readable(out.StatusB)
	if !out.Comparable {
		return out
	}
	mb := map[string]snapshot.Item{}
	for _, it := range itemsB {
		mb[it.Key] = it
	}
	inA := map[string]bool{}
	for _, ia := range itemsA {
		inA[ia.Key] = true
		if config.MatchIgnore(ignore, kind, ia.Key) {
			out.Ignored++
			continue
		}
		ib, ok := mb[ia.Key]
		switch {
		case !ok:
			out.OnlyA = append(out.OnlyA, ia)
		case ia.Value != ib.Value || ia.Detail != ib.Detail:
			out.Changed = append(out.Changed, Change{Key: ia.Key, A: ia.Value, B: ib.Value, DetailA: ia.Detail, DetailB: ib.Detail, TagA: ia.Tag, TagB: ib.Tag})
		default:
			out.Same = append(out.Same, ia)
		}
	}
	for _, ib := range itemsB {
		if inA[ib.Key] {
			continue
		}
		if config.MatchIgnore(ignore, kind, ib.Key) {
			out.Ignored++
			continue
		}
		out.OnlyB = append(out.OnlyB, ib)
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
