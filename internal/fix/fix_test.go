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
	// Written from laptop's side for readability; the script runs on desk,
	// the first side, to bring over what laptop has.
	r := &diff.Result{A: diff.Side{Label: "laptop", Snap: s}, B: diff.Side{Label: "desk", Snap: s}, Sections: sections}
	return r.Reversed()
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
		"brew install jq",
		"brew install --cask alt-tab",
		"brew tap owner/tools",
		"mas install 409183694",
		"defaults write com.apple.dock autohide -bool true",
		"defaults write com.apple.screencapture location -string '~/Pictures/Shots'",
		"# only on desk: brew uninstall wget",
		"# skipped defaults dock › persistent-apps: not a simple value",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "brew tap") > strings.Index(got, "brew install jq") {
		t.Errorf("tap must come before the formulae:\n%s", got)
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

func TestLibraryAndToolchainCommands(t *testing.T) {
	r := result(
		diff.Section{Kind: "libraries", Title: "Language libraries", Comparable: true, OnlyA: []snapshot.Item{
			{Key: "python3.12 › requests", Value: "2.32", Tag: "user"},
			{Key: "python3.12 › numpy", Value: "2.0", Tag: "system"},
			{Key: "gem › rake", Value: "13"},
			{Key: "gem › json", Value: "2.7", Tag: "default"},
			{Key: "perl › Moose::Util", Value: "2"},
			{Key: "R › data.table", Value: "1.15"},
			{Key: "julia v1.10 › Plots", Value: "added"},
			{Key: "R › x'); system('id", Value: "1"},
		}},
		diff.Section{Kind: "toolchains", Title: "Toolchains", Comparable: true, OnlyA: []snapshot.Item{
			{Key: "pyenv › 3.12.4", Value: "installed"},
			{Key: "rustup › nightly-aarch64-apple-darwin", Value: "installed"},
			{Key: "asdf › nodejs 22.1.0", Value: "installed"},
			{Key: "mise › go 1.23", Value: "active"},
			{Key: "pyenv › 3.12; rm -rf ~", Value: "installed"},
		}},
	)
	got := Script(r)
	for _, want := range []string{
		"python3.12 -m pip install --user requests",
		"# python3.12 › numpy: installed system-wide there",
		"gem install rake",
		"cpanm Moose::Util",
		`Rscript -e 'install.packages('\''data.table'\''`,
		`julia -e 'using Pkg; Pkg.add("Plots")'`,
		"pyenv install --skip-existing 3.12.4",
		"rustup toolchain install nightly-aarch64-apple-darwin",
		"asdf install nodejs 22.1.0",
		"mise install go@1.23",
		"# skipped libraries R › x'); system('id: unusual characters",
		"# skipped toolchains pyenv › 3.12; rm -rf ~: unusual characters",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "gem install json") {
		t.Error("default gem installed explicitly")
	}
}

// The installer runs every step, keeps going after a failure or a missing
// tool, passes hostile names as plain arguments, and reports.
func TestInstallerRunsEveryStep(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "pwned")
	record := filepath.Join(t.TempDir(), "args")
	for name, body := range map[string]string{
		"hdtest-ok":   "printf '%s\\n' \"$@\" >> " + Quote(record),
		"hdtest-fail": "exit 3",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	evil := "$(touch " + marker + ")"
	acts := []Action{
		{Kind: "k", Key: "one", Argv: []string{"hdtest-fail"}},
		{Kind: "k", Key: "two", Argv: []string{"hdtest-missing-tool", "x"}},
		{Kind: "k", Key: "three", Argv: []string{"hdtest-ok", evil}},
		{Kind: "k", Key: "note only", Note: "nothing to run"},
	}
	cmd := exec.Command("/bin/sh", "-c", Installer("box\ntouch "+marker, acts, false))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "HOSTDIFF_SYSROOT=/nonexistent"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("failed steps must fail the installer:\n%s", out)
	}
	for _, want := range []string{"==> hdtest-fail", "hdtest-missing-tool is not installed on this machine", "1 of 3 done, 2 failed", "Failed:"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if got, _ := os.ReadFile(record); string(got) != evil+"\n" {
		t.Errorf("argument not passed literally: %q", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("injected command ran:\n%s", out)
	}
}

// Every runnable action of a hostile snapshot renders to a command the shell
// reads as exactly its argument list.
func TestCommandRoundTrip(t *testing.T) {
	for _, argv := range [][]string{
		{"brew", "install", "jq"},
		{"defaults", "write", "com.apple.x", "a key", "-string", "it's $(x) `y` ~ *"},
		{"Rscript", "-e", "install.packages('a.b', repos = 'https://cloud.r-project.org')"},
	} {
		line := Action{Argv: append([]string{"printf", "%s\\n"}, argv...)}.Command()
		out, err := exec.Command("/bin/sh", "-c", line).Output()
		if err != nil || string(out) != strings.Join(argv, "\n")+"\n" {
			t.Errorf("%q -> %q (%v)", line, out, err)
		}
	}
}
