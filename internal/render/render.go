// Package render prints a comparison as text, Markdown or JSON.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	udiff "github.com/aymanbagabas/go-udiff"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/snapshot"
)

// Options control text output.
type Options struct {
	Color   bool
	All     bool // also list identical items
	Details bool // print content diffs for changed items
}

type palette struct{ bold, dim, a, b, ch, reset string }

func colors(on bool) palette {
	if !on {
		return palette{}
	}
	return palette{bold: "\x1b[1m", dim: "\x1b[2m", a: "\x1b[33m", b: "\x1b[36m", ch: "\x1b[35m", reset: "\x1b[0m"}
}

func describe(s diff.Side) string {
	h := s.Snap.Host
	os := h.OS
	if os == "darwin" {
		os = "macOS"
	}
	return fmt.Sprintf("%s (%s, %s/%s, %s)", s.Label, h.Name, os, h.Arch, s.Snap.Created.Local().Format("2006-01-02 15:04"))
}

// Text writes the human-readable comparison.
func Text(w io.Writer, r *diff.Result, opt Options) {
	p := colors(opt.Color)
	fmt.Fprintf(w, "%s%s◀%s %s\n", p.bold, p.a, p.reset, describe(r.A))
	fmt.Fprintf(w, "%s%s▶%s %s\n", p.bold, p.b, p.reset, describe(r.B))
	var same, blocked []string
	for i := range r.Sections {
		s := &r.Sections[i]
		if !s.Comparable {
			if s.StatusA == snapshot.Absent && s.StatusB == snapshot.Absent {
				continue
			}
			blocked = append(blocked, fmt.Sprintf("%s: %s", s.Title, whyBlocked(r, s)))
			continue
		}
		if s.Differences() == 0 && !opt.All {
			if len(s.Same) > 0 || s.StatusA == snapshot.OK {
				same = append(same, fmt.Sprintf("%s (%d)", s.Title, len(s.Same)))
			}
			continue
		}
		width := 0
		for _, k := range keys(s) {
			width = max(width, len([]rune(k)))
		}
		width = min(width, 56)
		fmt.Fprintf(w, "\n%s%s%s  %s%s%s\n", p.bold, s.Title, p.reset, p.dim, counts(r, s), p.reset)
		for _, n := range []struct {
			side  snapshot.Status
			label string
		}{{s.StatusA, r.A.Label}, {s.StatusB, r.B.Label}} {
			if n.side == snapshot.Absent {
				fmt.Fprintf(w, "  %snot installed on %s%s\n", p.dim, n.label, p.reset)
			}
		}
		// Packages only pulled in by other packages are counted, not listed,
		// unless --all: they follow from the requested ones.
		hidden := 0
		for _, it := range s.OnlyA {
			if it.Tag == "dependency" && !opt.All {
				hidden++
				continue
			}
			fmt.Fprintf(w, "  %s◀%s %s  %s\n", p.a, p.reset, pad(it.Key, width), it.Value)
		}
		for _, it := range s.OnlyB {
			if it.Tag == "dependency" && !opt.All {
				hidden++
				continue
			}
			fmt.Fprintf(w, "  %s▶%s %s  %s\n", p.b, p.reset, pad(it.Key, width), it.Value)
		}
		for _, c := range s.Changed {
			if c.TagA == "dependency" && c.TagB == "dependency" && !opt.All {
				hidden++
				continue
			}
			if c.A == c.B {
				fmt.Fprintf(w, "  %s≠%s %s  %scontent differs%s\n", p.ch, p.reset, pad(c.Key, width), p.dim, p.reset)
			} else {
				fmt.Fprintf(w, "  %s≠%s %s  %s%s%s │ %s%s%s\n", p.ch, p.reset, pad(c.Key, width), p.a, c.A, p.reset, p.b, c.B, p.reset)
			}
			if opt.Details && c.DetailA != c.DetailB {
				writeDetail(w, p, r, c)
			}
		}
		if hidden > 0 {
			fmt.Fprintf(w, "  %s… and %d differences in dependencies (installed only because other packages need them; --all lists them)%s\n", p.dim, hidden, p.reset)
		}
		if opt.All {
			for _, it := range s.Same {
				fmt.Fprintf(w, "  %s= %s  %s%s\n", p.dim, pad(it.Key, width), it.Value, p.reset)
			}
		}
	}
	fmt.Fprintln(w)
	n := r.Differences()
	switch n {
	case 0:
		fmt.Fprintf(w, "%sNo differences.%s\n", p.bold, p.reset)
	default:
		fmt.Fprintf(w, "%s%d differences.%s", p.bold, n, p.reset)
		if !opt.Details && hasContentChanges(r) {
			fmt.Fprintf(w, " Add --details to see changed file contents.")
		}
		fmt.Fprintln(w)
	}
	if len(same) > 0 && !opt.All {
		fmt.Fprintf(w, "%sSame: %s%s\n", p.dim, strings.Join(same, ", "), p.reset)
	}
	for _, b := range blocked {
		fmt.Fprintf(w, "%sNot compared: %s%s\n", p.dim, b, p.reset)
	}
	if len(blocked) > 0 {
		fmt.Fprintf(w, "%sTip: some sections can only be read in a Terminal on that machine: run `hostdiff snap -o FILE` there, then compare the files.%s\n", p.dim, p.reset)
	}
}

func hasContentChanges(r *diff.Result) bool {
	for i := range r.Sections {
		for _, c := range r.Sections[i].Changed {
			if c.DetailA != c.DetailB && (strings.Contains(c.DetailA, "\n") || strings.Contains(c.DetailB, "\n")) {
				return true
			}
		}
	}
	return false
}

func whyBlocked(r *diff.Result, s *diff.Section) string {
	var parts []string
	if s.StatusA != snapshot.OK && s.StatusA != snapshot.Absent {
		parts = append(parts, r.A.Label+" "+string(s.StatusA)+noteSuffix(s.NoteA))
	}
	if s.StatusB != snapshot.OK && s.StatusB != snapshot.Absent {
		parts = append(parts, r.B.Label+" "+string(s.StatusB)+noteSuffix(s.NoteB))
	}
	return strings.Join(parts, "; ")
}

func noteSuffix(n string) string {
	if n == "" {
		return ""
	}
	return " (" + n + ")"
}

func counts(r *diff.Result, s *diff.Section) string {
	var parts []string
	onlyA, onlyB, changed, deps := 0, 0, 0, 0
	for _, it := range s.OnlyA {
		if it.Tag == "dependency" {
			deps++
		} else {
			onlyA++
		}
	}
	for _, it := range s.OnlyB {
		if it.Tag == "dependency" {
			deps++
		} else {
			onlyB++
		}
	}
	for _, c := range s.Changed {
		if c.TagA == "dependency" && c.TagB == "dependency" {
			deps++
		} else {
			changed++
		}
	}
	if onlyA > 0 {
		parts = append(parts, fmt.Sprintf("%d only on %s", onlyA, r.A.Label))
	}
	if onlyB > 0 {
		parts = append(parts, fmt.Sprintf("%d only on %s", onlyB, r.B.Label))
	}
	if changed > 0 {
		parts = append(parts, fmt.Sprintf("%d differ", changed))
	}
	if deps > 0 {
		parts = append(parts, fmt.Sprintf("%d in dependencies", deps))
	}
	parts = append(parts, fmt.Sprintf("%d same", len(s.Same)))
	if s.Ignored > 0 {
		parts = append(parts, fmt.Sprintf("%d ignored", s.Ignored))
	}
	return strings.Join(parts, " · ")
}

func keys(s *diff.Section) []string {
	var out []string
	for _, it := range s.OnlyA {
		out = append(out, it.Key)
	}
	for _, it := range s.OnlyB {
		out = append(out, it.Key)
	}
	for _, c := range s.Changed {
		out = append(out, c.Key)
	}
	return out
}

func pad(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// Unified returns a unified diff of two detail texts.
func Unified(r *diff.Result, c diff.Change) string {
	return udiff.Unified(r.A.Label+": "+c.Key, r.B.Label+": "+c.Key, c.DetailA, c.DetailB)
}

func writeDetail(w io.Writer, p palette, r *diff.Result, c diff.Change) {
	for _, l := range strings.Split(strings.TrimRight(Unified(r, c), "\n"), "\n") {
		color := ""
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			color = p.bold
		case strings.HasPrefix(l, "-"):
			color = p.a
		case strings.HasPrefix(l, "+"):
			color = p.b
		case strings.HasPrefix(l, "@@"):
			color = p.dim
		}
		fmt.Fprintf(w, "      %s%s%s\n", color, l, p.reset)
	}
}

// Markdown writes the comparison as Markdown tables.
func Markdown(w io.Writer, r *diff.Result, opt Options) {
	fmt.Fprintf(w, "# %s vs %s\n\n", md(r.A.Label), md(r.B.Label))
	fmt.Fprintf(w, "- ◀ %s\n- ▶ %s\n\n", md(describe(r.A)), md(describe(r.B)))
	fmt.Fprintf(w, "%d differences.\n", r.Differences())
	for i := range r.Sections {
		s := &r.Sections[i]
		if !s.Comparable {
			if s.StatusA == snapshot.Absent && s.StatusB == snapshot.Absent {
				continue
			}
			fmt.Fprintf(w, "\n## %s\n\nNot compared: %s\n", md(s.Title), md(whyBlocked(r, s)))
			continue
		}
		if s.Differences() == 0 && !opt.All {
			continue
		}
		fmt.Fprintf(w, "\n## %s\n\n%s\n\n| Item | %s | %s |\n|---|---|---|\n", md(s.Title), md(counts(r, s)), md(r.A.Label), md(r.B.Label))
		for _, it := range s.OnlyA {
			fmt.Fprintf(w, "| %s | %s | — |\n", md(it.Key), md(it.Value))
		}
		for _, it := range s.OnlyB {
			fmt.Fprintf(w, "| %s | — | %s |\n", md(it.Key), md(it.Value))
		}
		for _, c := range s.Changed {
			a, b := c.A, c.B
			if a == b {
				a, b = a+" (content differs)", b+" (content differs)"
			}
			fmt.Fprintf(w, "| %s | %s | %s |\n", md(c.Key), md(a), md(b))
		}
		if opt.All {
			for _, it := range s.Same {
				fmt.Fprintf(w, "| %s | %s | %s |\n", md(it.Key), md(it.Value), md(it.Value))
			}
		}
		if opt.Details {
			for _, c := range s.Changed {
				if c.DetailA != c.DetailB {
					fmt.Fprintf(w, "\n```diff\n%s```\n", strings.ReplaceAll(Unified(r, c), "```", "ʼʼʼ"))
				}
			}
		}
	}
}

func md(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

type jsonSide struct {
	Label   string        `json:"label"`
	Host    snapshot.Host `json:"host"`
	Created time.Time     `json:"created"`
}

// JSON writes the comparison as JSON.
func JSON(w io.Writer, r *diff.Result) error {
	out := struct {
		A           jsonSide       `json:"a"`
		B           jsonSide       `json:"b"`
		Differences int            `json:"differences"`
		Sections    []diff.Section `json:"sections"`
	}{
		A:           jsonSide{r.A.Label, r.A.Snap.Host, r.A.Snap.Created},
		B:           jsonSide{r.B.Label, r.B.Snap.Host, r.B.Snap.Created},
		Differences: r.Differences(),
		Sections:    r.Sections,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

// Summary writes one line per section of a single snapshot.
func Summary(w io.Writer, s *snapshot.Snapshot, label string) {
	fmt.Fprintf(w, "%s: %s/%s, %s\n\n", label, s.Host.OS, s.Host.Arch, s.Created.Local().Format("2006-01-02 15:04"))
	for _, sec := range s.Sections {
		state := fmt.Sprintf("%d items", len(sec.Items))
		if sec.Status != snapshot.OK {
			state = string(sec.Status)
			if sec.Note != "" {
				state += ": " + sec.Note
			}
		}
		fmt.Fprintf(w, "  %-12s %-28s %s\n", sec.Kind, sec.Title, state)
	}
}
