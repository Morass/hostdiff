package fix

import (
	"fmt"
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
	for _, want := range []string{"==> [1/3] hdtest-fail", "✗ failed (exit 3)", "==> [2/3] hdtest-missing-tool x", "✗ hdtest-missing-tool is not installed on this machine", "==> [3/3] hdtest-ok", "✓ done", "1 of 3 done, 2 failed", "Failed:"} {
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

func TestRemoveAndUpdateCommands(t *testing.T) {
	cases := []struct {
		act  Action
		ok   bool
		verb string
		want string
	}{}
	add := func(a Action, ok bool, verb, want string) {
		cases = append(cases, struct {
			act  Action
			ok   bool
			verb string
			want string
		}{a, ok, verb, want})
	}
	a, ok := ForRemove("brew", snapshot.Item{Key: "cask › alt-tab"})
	add(a, ok, Remove, "brew uninstall --cask alt-tab")
	a, ok = ForRemove("defaults", snapshot.Item{Key: "dock › autohide", Tag: "defaults com.apple.dock\ntype bool"})
	add(a, ok, Reset, "defaults delete com.apple.dock autohide")
	a, ok = ForRemove("toolchains", snapshot.Item{Key: "pyenv › 3.11.9"})
	add(a, ok, Remove, "pyenv uninstall --force 3.11.9")
	a, ok = ForRemove("libraries", snapshot.Item{Key: "gem › rake"})
	add(a, ok, Remove, "gem uninstall --all --executables rake")
	a, ok = ForUpdate("packages", "npm › typescript", "5.6.2", "", "5.4.0", "")
	add(a, ok, Update, "npm install -g typescript@5.6.2")
	a, ok = ForUpdate("packages", "cargo › ripgrep", "v14.1.0", "", "v13.0.0", "")
	add(a, ok, Update, "cargo install --force --version 14.1.0 ripgrep")
	a, ok = ForUpdate("brew", "formula › node", "26", "", "25", "")
	add(a, ok, Update, "brew upgrade node")
	a, ok = ForUpdate("libraries", "python3.12 › requests", "2.32.3", "user", "2.31.0", "user")
	add(a, ok, Update, "python3.12 -m pip install --user requests==2.32.3")
	a, ok = ForUpdate("defaults", "dock › tilesize", "48", "defaults com.apple.dock\ntype int", "36", "")
	add(a, ok, Set, "defaults write com.apple.dock tilesize -int 48")
	for _, c := range cases {
		if !c.ok || c.act.Verb != c.verb || c.act.Command() != c.want {
			t.Errorf("got %v %q %q, want %q %q", c.ok, c.act.Verb, c.act.Command(), c.verb, c.want)
		}
	}
	for _, bad := range []Action{
		func() Action { a, _ := ForUpdate("packages", "npm › x", "1.0; rm -rf ~", "", "0.9", ""); return a }(),
		func() Action {
			a, _ := ForRemove("libraries", snapshot.Item{Key: "python3.12 › numpy", Tag: "system"})
			return a
		}(),
		func() Action {
			a, _ := ForRemove("libraries", snapshot.Item{Key: "gem › json", Tag: "default"})
			return a
		}(),
	} {
		if bad.Runnable() || bad.Note == "" {
			t.Errorf("should have no command, only a note: %+v", bad)
		}
	}
}

// Clone installs first, updates next, removes last (formulae before their
// tap), and leaves dependencies to the package manager.
func TestCloneOrder(t *testing.T) {
	s := &snapshot.Snapshot{}
	r := &diff.Result{A: diff.Side{Label: "a", Snap: s}, B: diff.Side{Label: "b", Snap: s}, Sections: []diff.Section{
		{Kind: "brew", Comparable: true,
			OnlyA: []snapshot.Item{{Key: "tap › owner/tools"}, {Key: "formula › jq", Tag: "requested"}, {Key: "formula › lib", Tag: "dependency"}},
			OnlyB: []snapshot.Item{{Key: "formula › wget", Tag: "requested"}}},
		{Kind: "packages", Comparable: true, Changed: []diff.Change{{Key: "npm › x", A: "1.0.0", B: "2.0.0"}}},
	}}
	var got []string
	for _, a := range Clone(r, true) {
		if a.Runnable() {
			got = append(got, a.Command())
		}
	}
	want := []string{"brew install wget", "npm install -g x@2.0.0", "brew uninstall jq", "brew untap owner/tools"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("clone:\n got %q\nwant %q", got, want)
	}
}

// The script contains, for every step, the exact command line a person
// confirmed, and nothing that runs besides those lines.
func TestInstallerRunsTheShownCommands(t *testing.T) {
	acts := []Action{
		{Kind: "brew", Key: "formula › jq", Verb: Remove, Argv: []string{"brew", "uninstall", "jq"}},
		{Kind: "defaults", Key: "screenshots › location", Verb: Set, Argv: []string{"defaults", "write", "com.apple.screencapture", "location", "-string", "~/Pictures/Shot's"}},
	}
	script := Installer("desk", acts, true)
	for i, a := range acts {
		line := fmt.Sprintf("step %d/2 %s %s", i+1, Quote(a.Command()), a.Command())
		if !strings.Contains(script, "\n"+line+"\n") {
			t.Errorf("missing step line %q in:\n%s", line, script)
		}
	}
	commands := 0
	for _, l := range strings.Split(script, "\n") {
		if strings.HasPrefix(l, "step ") {
			commands++
		}
	}
	if commands != 2 {
		t.Errorf("%d step lines, want 2", commands)
	}
}
