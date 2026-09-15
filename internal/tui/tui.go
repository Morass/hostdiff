// Package tui is the interactive view of a comparison: sections on the left,
// what differs on the right, and the content diff of any item on Enter.
package tui

import (
	"fmt"
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

// Model is the bubbletea model.
type Model struct {
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
func New(res *diff.Result) Model {
	m := Model{res: res, width: 100, height: 30}
	for i := range res.Sections {
		s := &res.Sections[i]
		if !s.Comparable && s.StatusA == snapshot.Absent && s.StatusB == snapshot.Absent {
			continue
		}
		if s.Comparable && len(s.OnlyA)+len(s.OnlyB)+len(s.Changed)+len(s.Same) == 0 {
			continue
		}
		m.secs = append(m.secs, i)
	}
	// Start on the first section with differences.
	for i, si := range m.secs {
		if res.Sections[si].Differences() > 0 {
			m.sec = i
			break
		}
	}
	return m
}

// Run shows the comparison until the user quits.
func Run(res *diff.Result) error {
	_, err := tea.NewProgram(New(res), tea.WithAltScreen()).Run()
	return err
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
		case "q", "esc", "enter", "left", "h":
			m.detail = nil
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
	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
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
		m.detailName = fmt.Sprintf("Script to make %s more like %s (review it, then copy what you need)", m.res.B.Label, m.res.A.Label)
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
	b.WriteString(styleDim.Render(truncate(fmt.Sprintf("%d differences", m.res.Differences()), w)) + "\n")

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
			line := padTo(truncate(r.key, kw), kw) + "  " + r.value
			line = truncate(line, rw-2)
			if i == m.row && m.right {
				line = styleSel.Render(padTo(line, rw-2))
			}
			right = append(right, mark+" "+line)
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
