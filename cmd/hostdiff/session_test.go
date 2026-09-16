package main

import (
	"sync"
	"testing"

	"github.com/morass/hostdiff/internal/config"
	"github.com/morass/hostdiff/internal/snapshot"
)

// A scan that was abandoned (another machine chosen, or a newer scan
// started) must not write its result over what came after it.
func TestAbandonedCollectIsDiscarded(t *testing.T) {
	cfg := &config.Config{Machines: map[string]*config.Machine{
		"x": {Name: "x", SSH: "x.invalid", Upload: "auto"},
		"y": {Name: "y", SSH: "y.invalid", Upload: "auto"},
	}}
	s := newSession(cfg, nil)
	release := make(chan struct{})
	var mu sync.Mutex
	s.collectFn = func(t *target, kinds []string, _ func(snapshot.Progress)) (*snapshot.Snapshot, error) {
		if t.label == "x" {
			<-release // the old scan finishes last
		}
		mu.Lock()
		defer mu.Unlock()
		return &snapshot.Snapshot{Format: 1, Host: snapshot.Host{Name: t.label}, Sections: []snapshot.Section{{Kind: "brew", Status: snapshot.OK, Items: []snapshot.Item{{Key: "formula › from-" + t.label}}}}}, nil
	}
	if err := s.Select("localhost", "x"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Collect(1, []string{"brew"}, nil) }()
	// Meanwhile the user picks y and scans it.
	for {
		s.mu.Lock()
		started := s.seq[1] >= 2
		s.mu.Unlock()
		if started {
			break
		}
	}
	if err := s.Select("localhost", "y"); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(1, []string{"brew"}, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Error("the abandoned scan was not told it was discarded")
	}
	res := s.Result([]string{"brew"})
	if res.B.Label != "y" || len(res.Sections) != 1 {
		t.Fatalf("result: %+v", res)
	}
	for _, it := range res.Sections[0].OnlyB {
		if it.Key != "formula › from-y" {
			t.Fatalf("x's scan overwrote y's: %v", res.Sections[0].OnlyB)
		}
	}
}
