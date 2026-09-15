// End-to-end tests: the real binary, a sandboxed HOME and system root, stub
// tools, and a fake ssh that runs the "remote" side locally with another
// sandbox. Nothing here reads the real machine's configuration.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var bin string

// Token-shaped fixtures are assembled at run time so the source never holds
// a literal that secret scanners flag.
var token = "ghp" + "_" + strings.Repeat("Zx9", 12)

func TestMain(m *testing.M) {
	if p := os.Getenv("HOSTDIFF_BIN"); p != "" {
		bin = p
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "hostdiff-e2e-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "hostdiff")
	build := exec.Command("go", "build", "-o", bin, "../cmd/hostdiff")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type machine struct {
	home, stubs string
}

type world struct {
	t       *testing.T
	root    string // sandbox root
	sysroot string
	local   machine
	sshRoot string
	config  string
}

func write(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// newMachine creates a home with stub tools whose output comes from files in
// the home, so each machine can have different "installed" software.
func newMachine(t *testing.T, home string, formulae, shortcuts string) machine {
	m := machine{home: home, stubs: filepath.Join(home, ".stubs")}
	write(t, filepath.Join(home, ".fixture", "formulae"), formulae, 0o644)
	write(t, filepath.Join(home, ".fixture", "shortcuts"), shortcuts, 0o644)
	stub := func(name, body string) { write(t, filepath.Join(m.stubs, name), "#!/bin/sh\n"+body+"\n", 0o755) }
	stub("brew", `case "$1 $2" in
"list --formula") cat "$HOME/.fixture/formulae";;
"list --cask") ;;
"leaves --installed-on-request") cut -d' ' -f1 "$HOME/.fixture/formulae";;
esac`)
	stub("shortcuts", `[ "$2" = "--folders" ] && exit 0; cat "$HOME/.fixture/shortcuts"`)
	stub("crontab", `exit 0`)
	stub("mas", `exit 0`)
	stub("lpstat", `exit 0`)
	stub("defaults", `exit 1`)
	stub("launchctl", `exit 1`)
	write(t, filepath.Join(home, ".zshrc"), "export PATH=\"$HOME/bin:$PATH\"\nexport GITHUB_TOKEN="+token+"\nalias ll='ls -l'\n", 0o644)
	write(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = Someone\n[url \"https://x-access-token:"+token+"@github.com/\"]\n\tinsteadOf = https://github.com/\n", 0o644)
	return m
}

func newWorld(t *testing.T) *world {
	root := t.TempDir()
	w := &world{t: t, root: root, sysroot: filepath.Join(root, "sys"), sshRoot: filepath.Join(root, "remotes"), config: filepath.Join(root, "config", "hostdiff", "config.toml")}
	w.local = newMachine(t, filepath.Join(root, "home", "local-user"), "jq 1.7.1\nnode 26.0.0\n", "Start timer\n")
	if err := os.MkdirAll(w.sysroot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fake ssh: `ssh [options] -- DEST COMMAND` runs COMMAND with the home
	// $FAKE_SSH_ROOT/DEST, the way sshd hands it to the login shell.
	// `ssh -G -- DEST` prints where DEST leads: "self" and "other@self" come
	// back to this machine, anything else to a documentation address.
	write(t, filepath.Join(root, "bin", "ssh"), `#!/bin/sh
if [ "$1" = "-G" ]; then
  case "$3" in
  self) printf 'user %s\nhostname localhost\nport 22\n' "$(id -un)";;
  other@self) printf 'user other\nhostname localhost\nport 22\n';;
  *) printf 'user %s\nhostname 2001:db8::10\nport 22\n' "$(id -un)";;
  esac
  exit 0
fi
while [ $# -gt 0 ]; do if [ "$1" = "--" ]; then shift; break; fi; shift; done
dest=$1; shift
home="$FAKE_SSH_ROOT/$dest"
[ -d "$home" ] || { echo "ssh: Could not resolve hostname $dest" >&2; exit 255; }
exec env -i HOME="$home" PATH="$home/.stubs:/usr/bin:/bin" SHELL=/bin/sh HOSTDIFF_SYSROOT="$FAKE_SYSROOT" SSH_CONNECTION="sandbox 1 sandbox 22" /bin/sh -c "$*"
`, 0o755)
	return w
}

func (w *world) remote(name, formulae, shortcuts string, installed bool) machine {
	m := newMachine(w.t, filepath.Join(w.sshRoot, name), formulae, shortcuts)
	if installed {
		b, err := os.ReadFile(bin)
		if err != nil {
			w.t.Fatal(err)
		}
		write(w.t, filepath.Join(m.home, ".local", "bin", "hostdiff"), string(b), 0o755)
	}
	return m
}

func (w *world) writeConfig(body string) {
	write(w.t, w.config, body, 0o600)
}

type result struct {
	stdout, stderr string
	code           int
}

func (w *world) run(args ...string) result {
	w.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = w.root
	cmd.Env = []string{
		"HOME=" + w.local.home,
		"PATH=" + w.local.stubs + ":/usr/bin:/bin",
		"SHELL=/bin/sh",
		"HOSTDIFF_SYSROOT=" + w.sysroot,
		"HOSTDIFF_CONFIG=" + w.config,
		"HOSTDIFF_SSH=" + filepath.Join(w.root, "bin", "ssh"),
		"XDG_STATE_HOME=" + filepath.Join(w.root, "state"),
		"FAKE_SSH_ROOT=" + w.sshRoot,
		"FAKE_SYSROOT=" + w.sysroot,
		"TMPDIR=" + w.root,
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		w.t.Fatal(err)
	}
	return result{out.String(), errb.String(), code}
}

func noLeak(t *testing.T, what, s string, w *world) {
	t.Helper()
	if strings.Contains(s, token[4:16]) {
		t.Errorf("%s contains the token", what)
	}
	if strings.Contains(s, w.root) {
		t.Errorf("%s contains a sandbox path (home not normalised)", what)
	}
}

func TestSnapshotHasNoSecrets(t *testing.T) {
	w := newWorld(t)
	r := w.run("snap", "--json")
	if r.code != 0 {
		t.Fatalf("snap failed: %+v", r)
	}
	noLeak(t, "snapshot", r.stdout, w)
	var s struct {
		Format   int `json:"format"`
		Sections []struct {
			Kind  string `json:"kind"`
			Items []struct{ Key, Value, Detail string }
		} `json:"sections"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &s); err != nil || s.Format != 1 {
		t.Fatalf("bad json: %v", err)
	}
	kinds := map[string]bool{}
	for _, sec := range s.Sections {
		kinds[sec.Kind] = true
	}
	for _, k := range []string{"brew", "dotfiles", "git", "shell"} {
		if !kinds[k] {
			t.Errorf("section %s missing", k)
		}
	}
	if !strings.Contains(r.stdout, "[REDACTED]") {
		t.Error("nothing was redacted")
	}
}

func TestSnapFileIsPrivateAndReplacesSymlinks(t *testing.T) {
	w := newWorld(t)
	victim := filepath.Join(w.root, "victim.txt")
	write(t, victim, "keep me", 0o644)
	out := filepath.Join(w.root, "snap.json")
	if err := os.Symlink(victim, out); err != nil {
		t.Fatal(err)
	}
	if r := w.run("snap", "-o", out, "--only", "brew"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep me" {
		t.Fatal("wrote through a symlink")
	}
	st, err := os.Lstat(out)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("output not a private regular file: %v %v", st.Mode(), err)
	}
}

func TestDiffFiles(t *testing.T) {
	w := newWorld(t)
	a := filepath.Join(w.root, "a.json")
	b := filepath.Join(w.root, "b.json")
	if r := w.run("snap", "-o", a); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := w.run("diff", a, a); r.code != 0 || !strings.Contains(r.stdout, "No differences.") {
		t.Fatalf("identical snapshots: %+v", r)
	}
	write(t, filepath.Join(w.local.home, ".fixture", "formulae"), "node 25.0.0\nwget 1.25\n", 0o644)
	write(t, filepath.Join(w.local.home, ".zshrc"), "export PATH=\"$HOME/bin:$PATH\"\nexport GITHUB_TOKEN="+token+"\nalias la='ls -la'\n", 0o644)
	if r := w.run("snap", "-o", b); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	r := w.run("diff", a, b, "--details", "--no-color")
	if r.code != 1 {
		t.Fatalf("expected exit 1 for differences: %+v", r)
	}
	for _, want := range []string{"◀ formula › jq", "▶ formula › wget", "≠ formula › node", "26.0.0 │ 25.0.0", "-alias ll='ls -l'", "+alias la='ls -la'"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
	noLeak(t, "diff", r.stdout, w)

	md := w.run("diff", a, b, "--format", "markdown")
	if !strings.Contains(md.stdout, "| formula › jq | 1.7.1 | — |") {
		t.Errorf("markdown:\n%s", md.stdout)
	}
	js := w.run("diff", a, b, "--format", "json")
	var parsed struct{ Differences int }
	if json.Unmarshal([]byte(js.stdout), &parsed) != nil || parsed.Differences == 0 {
		t.Errorf("json: %s", js.stdout)
	}
	sc := w.run("diff", a, b, "--script")
	if !strings.Contains(sc.stdout, "brew install 'wget'") || !strings.Contains(sc.stdout, "# only on a: brew uninstall 'jq'") {
		t.Errorf("script:\n%s", sc.stdout)
	}
	only := w.run("diff", a, b, "--only", "dotfiles", "--no-color")
	if strings.Contains(only.stdout, "formula") {
		t.Errorf("--only leaked other sections:\n%s", only.stdout)
	}
}

func TestRemoteInstalledAndUploaded(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("needs a POSIX shell")
	}
	w := newWorld(t)
	w.remote("laptop", "jq 1.7.1\nripgrep 14.1.0\n", "Resize image\n", true)
	w.remote("fresh", "jq 1.7.1\n", "Start timer\n", false)
	w.writeConfig(`[machines.laptop]
ssh = "laptop"
[machines.fresh]
ssh = "fresh"
[machines.locked]
ssh = "fresh"
upload = "never"
[machines.gone]
ssh = "nowhere"
[machines.here]
local = true
[machines.self]
ssh = "self"
[machines.other]
ssh = "other@self"
`)
	r := w.run("diff", "laptop", "--no-color")
	if r.code != 1 {
		t.Fatalf("laptop diff: %+v", r)
	}
	for _, want := range []string{"◀ here", "▶ laptop", "▶ formula › ripgrep", "◀ formula › node"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
	// Shortcuts come back empty-handed over ssh only when the tool returns
	// nothing; here the stub answers, so they are compared.
	if runtime.GOOS == "darwin" && !strings.Contains(r.stdout, "▶ Resize image") {
		t.Errorf("shortcuts not compared:\n%s", r.stdout)
	}
	noLeak(t, "remote diff", r.stdout+r.stderr, w)

	up := w.run("diff", "fresh", "here", "--only", "brew", "--no-color")
	if up.code != 1 || !strings.Contains(up.stdout, "▶ formula › node") {
		t.Fatalf("upload path: %+v", up)
	}
	left, _ := filepath.Glob(filepath.Join(w.root, "hostdiff.*"))
	if len(left) != 0 {
		t.Errorf("uploaded binary left behind: %v", left)
	}

	// Diffing a machine with itself is refused before anything is collected.
	for _, args := range [][]string{{"here"}, {"localhost", "here"}, {"self"}, {"self", "localhost"}} {
		r := w.run(append([]string{"diff"}, args...)...)
		if r.code != 2 || !strings.Contains(r.stderr, "same machine") || strings.Contains(r.stderr, "collecting") {
			t.Errorf("diff %v with itself: %+v", args, r)
		}
	}
	if r := w.run("diff", "self"); !strings.Contains(r.stderr, "ssh self leads back to this machine") {
		t.Errorf("reason missing: %+v", r)
	}
	// Another account on the same machine is a real comparison.
	if r := w.run("diff", "other"); strings.Contains(r.stderr, "same machine") {
		t.Errorf("other account refused: %+v", r)
	}

	never := w.run("diff", "locked")
	if never.code != 2 || !strings.Contains(never.stderr, "not installed there") {
		t.Errorf("upload = never: %+v", never)
	}
	gone := w.run("diff", "gone")
	if gone.code != 2 || !strings.Contains(gone.stderr, "ssh could not connect") {
		t.Errorf("unreachable: %+v", gone)
	}
	check := w.run("machines", "--check")
	if !strings.Contains(check.stdout, "✓ reachable") || !strings.Contains(check.stdout, "✗") {
		t.Errorf("machines --check:\n%s", check.stdout)
	}
}

func TestSavedSnapshots(t *testing.T) {
	w := newWorld(t)
	w.writeConfig("[machines.here]\nlocal = true\n")
	if r := w.run("snap", "here", "--save", "--only", "brew"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	write(t, filepath.Join(w.local.home, ".fixture", "formulae"), "jq 1.8.0\nnode 26.0.0\n", 0o644)
	r := w.run("diff", "here@last", "here", "--only", "brew", "--no-color")
	if r.code != 1 || !strings.Contains(r.stdout, "1.7.1 │ 1.8.0") {
		t.Fatalf("@last: %+v", r)
	}
	if r := w.run("diff", "nobody@last"); r.code != 2 {
		t.Errorf("missing saved snapshot: %+v", r)
	}
}

func TestConfigSafety(t *testing.T) {
	w := newWorld(t)
	if r := w.run("config", "init"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	st, _ := os.Stat(w.config)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config mode %o", st.Mode().Perm())
	}
	marker := filepath.Join(w.root, "pwned")
	w.writeConfig(fmt.Sprintf("[machines.evil]\nssh = \"-oProxyCommand=touch %s\"\n", marker))
	if r := w.run("diff", "evil"); r.code != 2 || !strings.Contains(r.stderr, "not a plain destination") {
		t.Errorf("option-shaped destination accepted: %+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ssh option injection ran a command")
	}
	w.writeConfig("[machines.x]\nlocal = true\n")
	if err := os.Chmod(w.config, 0o666); err != nil {
		t.Fatal(err)
	}
	if r := w.run("machines"); r.code != 2 || !strings.Contains(r.stderr, "writable by other users") {
		t.Errorf("world-writable config accepted: %+v", r)
	}
}

func TestHelp(t *testing.T) {
	w := newWorld(t)
	for _, args := range [][]string{{}, {"--help"}, {"help", "diff"}, {"diff", "--help"}, {"snap", "-h"}, {"sections"}, {"version"}} {
		if r := w.run(args...); r.code != 0 || r.stdout == "" {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if r := w.run("frobnicate"); r.code != 2 {
		t.Errorf("unknown command: %+v", r)
	}
	if r := w.run("diff", "x", "--only", "nosuch"); r.code != 2 || !strings.Contains(r.stderr, "unknown section") {
		t.Errorf("unknown section: %+v", r)
	}
}
