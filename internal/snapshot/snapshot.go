// Package snapshot defines what hostdiff records about one machine: a list of
// sections (Homebrew, apps, keyboard shortcuts, ...), each a sorted list of
// key/value items. Snapshots are plain JSON so they can be saved, copied
// between machines and compared later.
package snapshot

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// Format is bumped when the JSON layout changes incompatibly.
const Format = 1

// Item is one fact: "formula/jq" = "1.7.1". Detail holds longer text that is
// compared too and shown as a diff (a dotfile's content, a plist).
type Item struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`
	// Tag is shown and used by fix scripts but not compared: installed on
	// request or as a dependency, an App Store id, a setting's type.
	Tag string `json:"tag,omitempty"`
}

// Status says whether a section could be read.
type Status string

const (
	// OK means the section was read; an empty item list really is empty.
	OK Status = "ok"
	// Absent means the thing is not installed (no Homebrew, no mas).
	Absent Status = "absent"
	// Unavailable means it exists but could not be read from this session
	// (privacy protection, no GUI session over ssh).
	Unavailable Status = "unavailable"
	// Failed means reading it went wrong (timeout, parse error).
	Failed Status = "failed"
)

// Section is the result of one collector.
type Section struct {
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Note   string `json:"note,omitempty"`
	Items  []Item `json:"items,omitempty"`
}

// Host describes the machine a snapshot came from.
type Host struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Snapshot is everything collected from one machine at one moment.
type Snapshot struct {
	Format   int       `json:"format"`
	Tool     string    `json:"tool"`
	Created  time.Time `json:"created"`
	Host     Host      `json:"host"`
	Sections []Section `json:"sections"`
}

// Section returns the section of the given kind, or nil.
func (s *Snapshot) Section(kind string) *Section {
	for i := range s.Sections {
		if s.Sections[i].Kind == kind {
			return &s.Sections[i]
		}
	}
	return nil
}

// Add appends an item.
func (s *Section) Add(key, value, detail string) {
	s.Items = append(s.Items, Item{Key: key, Value: value, Detail: detail})
}

// AddTag appends an item with a tag.
func (s *Section) AddTag(key, value, tag string) {
	s.Items = append(s.Items, Item{Key: key, Value: value, Tag: tag})
}

// Sort orders items by key and drops exact duplicate keys (the last one wins),
// so two snapshots of the same machine compare equal.
func (s *Section) Sort() {
	sort.SliceStable(s.Items, func(i, j int) bool { return s.Items[i].Key < s.Items[j].Key })
	out := s.Items[:0]
	for i, it := range s.Items {
		if i+1 < len(s.Items) && s.Items[i+1].Key == it.Key {
			continue
		}
		out = append(out, it)
	}
	s.Items = out
}

// Read decodes a snapshot and checks its format.
func Read(r io.Reader) (*Snapshot, error) {
	var s Snapshot
	dec := json.NewDecoder(r)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("not a hostdiff snapshot: %w", err)
	}
	if s.Format == 0 {
		return nil, fmt.Errorf("not a hostdiff snapshot: no format field")
	}
	if s.Format != Format {
		return nil, fmt.Errorf("snapshot format %d is not supported by this hostdiff (format %d); use the same version on both machines", s.Format, Format)
	}
	return &s, nil
}

// Load reads a snapshot file.
func Load(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Write encodes a snapshot as indented JSON.
func Write(w io.Writer, s *Snapshot) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	enc.SetEscapeHTML(false)
	return enc.Encode(s)
}
