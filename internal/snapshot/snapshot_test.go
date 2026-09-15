package snapshot

import (
	"strings"
	"testing"
)

// A snapshot from a file or another machine is hostile input: escape
// sequences in it must not reach the terminal.
func TestReadStripsControlCharacters(t *testing.T) {
	in := `{"format": 1, "host": {"name": "a\u001b[2J\u001b[Hb"}, "sections": [{"kind": "brew", "title": "B\u009b31m", "status": "ok",
	"items": [{"key": "k\u0007", "value": "v\r1", "detail": "line1\n\tline2\u001b]0;x\u0007"}]}]}`
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	it := s.Sections[0].Items[0]
	if s.Host.Name != "a[2J[Hb" || s.Sections[0].Title != "B31m" || it.Key != "k" || it.Value != "v1" || it.Detail != "line1\n\tline2]0;x" {
		t.Fatalf("controls kept: %q %q %+v", s.Host.Name, s.Sections[0].Title, it)
	}
}
