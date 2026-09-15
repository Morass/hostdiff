package fix

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/snapshot"
)

func result(sections ...diff.Section) *diff.Result {
	s := &snapshot.Snapshot{}
	return &diff.Result{A: diff.Side{Label: "laptop", Snap: s}, B: diff.Side{Label: "desk", Snap: s}, Sections: sections}
}

func TestInstallCommands(t *testing.T) {
	r := result(
		diff.Section{Kind: "brew", Title: "Homebrew", Comparable: true, OnlyA: []snapshot.Item{
			{Key: "formula › jq", Value: "1.7", Tag: "requested"},
			{Key: "formula › oniguruma", Value: "6.9", Tag: "dependency"},
			{Key: "cask › alt-tab", Value: "7.2"},
			{Key: "tap › owner/tools", Value: "tapped"},
		}, OnlyB: []snapshot.Item{{Key: "formula › wget", Value: "1.2", Tag: "requested"}}},
		diff.Section{Kind: "mas", Title: "App Store", Comparable: true, OnlyA: []snapshot.Item{{Key: "Keynote", Value: "14", Tag: "id 409183694"}}},
		diff.Section{Kind: "defaults", Title: "macOS settings", Comparable: true, Changed: []diff.Change{
			{Key: "dock › autohide", A: "true", B: "false", TagA: "defaults com.apple.dock\ntype bool"},
			{Key: "screenshots › location", A: "~/Pictures/Shots", B: "~/Desktop", TagA: "defaults com.apple.screencapture\ntype string"},
			{Key: "dock › persistent-apps", A: "Safari, Mail", B: "Safari", TagA: ""},
		}},
	)
	got := Script(r)
	for _, want := range []string{
		"brew install 'jq'",
		"brew install --cask 'alt-tab'",
		"brew tap 'owner/tools'",
		"mas install 409183694  # Keynote",
		"defaults write 'com.apple.dock' 'autohide' -bool true",
		"defaults write 'com.apple.screencapture' 'location' -string '~/Pictures/Shots'",
		"# only on desk: brew uninstall 'wget'",
		"# skipped defaults dock › persistent-apps: not a simple value",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "oniguruma") {
		t.Errorf("dependency installed explicitly:\n%s", got)
	}
}

// A snapshot can come from another machine, so its contents are hostile
// input. Nothing in it may reach a command position of the generated script.
func TestHostileNamesNeverExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	evil := []string{
		"x'; touch " + marker + "; '",
		"x\ntouch " + marker,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
	}
	var sections []diff.Section
	for _, e := range evil {
		sections = append(sections,
			diff.Section{Kind: "brew", Title: "B\ntouch " + marker, Comparable: true, OnlyA: []snapshot.Item{{Key: "formula › " + e, Value: "1", Tag: "requested"}}},
			diff.Section{Kind: "mas", Title: "M", Comparable: true, OnlyA: []snapshot.Item{{Key: e, Value: "1", Tag: "id 1; touch " + marker}}},
			diff.Section{Kind: "defaults", Title: "D", Comparable: true, Changed: []diff.Change{
				{Key: "dock › " + e, A: "1", TagA: "defaults com.apple.dock\ntype int"},
				{Key: "dock › k", A: e, TagA: "defaults com.apple.dock\ntype string"},
				{Key: "dock › k2", A: "1", TagA: "defaults " + e + "\ntype int"},
			}},
		)
	}
	r := result(sections...)
	r.A.Label = "a\ntouch " + marker
	script := Script(r)

	// Run the script with every command it could invoke replaced by a no-op,
	// so only an injected command would touch the marker.
	bin := t.TempDir()
	for _, name := range []string{"brew", "mas", "defaults", "npm", "pipx", "uv", "cargo", "code", "cursor", "codium", "gh"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s\n%s", err, out, script)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("injected command ran; script:\n%s", script)
	}
}

func TestQuote(t *testing.T) {
	out, err := exec.Command("/bin/sh", "-c", "printf %s "+Quote("it's $(x) `y`")).Output()
	if err != nil || string(out) != "it's $(x) `y`" {
		t.Fatalf("quote round trip: %q %v", out, err)
	}
}
