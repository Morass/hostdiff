package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || c.Found || len(c.Machines) != 0 {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestValidMachines(t *testing.T) {
	p := write(t, `
ignore = ["apps:Xcode*", "runtimes:docker"]
[machines.laptop]
ssh = "laptop"
[machines.box]
ssh = "me@box.example.org"
command = "~/.local/bin/hostdiff"
upload = "never"
[machines.desk]
local = true
`, 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Names(), ","); got != "box,desk,laptop" {
		t.Fatalf("names %s", got)
	}
	if c.Machines["laptop"].Upload != "auto" || c.Machines["box"].Upload != "never" {
		t.Fatalf("upload defaults wrong: %+v", c.Machines)
	}
}

func TestRejectsUnsafeValues(t *testing.T) {
	cases := map[string]string{
		"option as destination": `[machines.x]
ssh = "-oProxyCommand=touch /tmp/pwned"`,
		"spaces in destination": `[machines.x]
ssh = "host; rm -rf ~"`,
		"quote in command": `[machines.x]
ssh = "h"
command = "hostdiff'; id; '"`,
		"both local and ssh": `[machines.x]
ssh = "h"
local = true`,
		"neither": `[machines.x]
upload = "auto"`,
		"reserved name": `[machines.local]
local = true`,
		"bad upload": `[machines.x]
ssh = "h"
upload = "sometimes"`,
	}
	for name, body := range cases {
		if _, err := Load(write(t, body, 0o600)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRejectsWritableByOthers(t *testing.T) {
	p := write(t, "[machines.x]\nlocal = true\n", 0o666)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("group/world-writable config accepted: %v", err)
	}
}

func TestWriteExample(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.toml")
	if err := WriteExample(p); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("example does not load: %v", err)
	}
	if err := WriteExample(p); err == nil {
		t.Fatal("overwrote an existing config")
	}
}

func TestMatchIgnore(t *testing.T) {
	pats := []string{"apps:Xcode*", "*docker*"}
	for _, c := range []struct {
		kind, key string
		want      bool
	}{
		{"apps", "Xcode.app", true},
		{"brew", "Xcode.app", false},
		{"runtimes", "docker", true},
		{"brew", "formula › docker-compose", true},
		{"brew", "formula › podman", false},
	} {
		if got := MatchIgnore(pats, c.kind, c.key); got != c.want {
			t.Errorf("%s:%s = %v", c.kind, c.key, got)
		}
	}
}

// Settings below a [machines.x] table belong to that table in TOML; they must
// be refused, not silently ignored.
func TestRejectsUnknownOrMisplacedSettings(t *testing.T) {
	p := write(t, "[machines.laptop]\nssh = \"laptop\"\nskip = [\"fonts\"]\n", 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "machines.laptop.skip") {
		t.Fatalf("misplaced skip accepted: %v", err)
	}
}

// Uncommenting every setting in the example must give a working config with
// the global settings in effect.
func TestExampleUncommentedWorks(t *testing.T) {
	var lines []string
	for _, l := range strings.Split(Example, "\n") {
		if rest, ok := strings.CutPrefix(l, "# "); ok && (strings.HasPrefix(rest, "[") || strings.Contains(rest, " = ")) && !strings.Contains(rest, ". ") {
			l = rest
		}
		lines = append(lines, l)
	}
	c, err := Load(write(t, strings.Join(lines, "\n"), 0o600))
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(lines, "\n"))
	}
	if len(c.Skip) != 1 || len(c.Ignore) != 2 || len(c.Machines) != 2 {
		t.Fatalf("settings lost: %+v", c)
	}
}
