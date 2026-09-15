package collect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/morass/hostdiff/internal/snapshot"
)

// Token-shaped fixtures are assembled at run time so the source never holds
// a literal that secret scanners flag.
var token = "ghp" + "_" + strings.Repeat("Ab1", 12)

func writeFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func sandbox(t *testing.T) *Env {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home", "someone")
	stubs := filepath.Join(root, "stubs")
	for _, d := range []string{stubs, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Env{Home: home, OS: "darwin", Root: filepath.Join(root, "sys"), Path: stubs, User: "someone"}
}

func stub(t *testing.T, e *Env, name, script string) {
	writeFile(t, filepath.Join(e.Path, name), "#!/bin/sh\n"+script+"\n", 0o755)
}

func section(t *testing.T, e *Env, kind string) snapshot.Section {
	t.Helper()
	s := Snapshot(e, Options{Only: []string{kind}})
	if len(s.Sections) != 1 {
		t.Fatalf("%s: %d sections", kind, len(s.Sections))
	}
	return s.Sections[0]
}

func find(s snapshot.Section, key string) *snapshot.Item {
	for i := range s.Items {
		if s.Items[i].Key == key {
			return &s.Items[i]
		}
	}
	return nil
}

func TestDotfilesRedactedAndNormalised(t *testing.T) {
	e := sandbox(t)
	writeFile(t, e.HomePath(".zshrc"), "export PATH="+e.Home+"/bin:$PATH\nexport GITHUB_TOKEN="+token+"\n", 0o644)
	// A dotfile linked into Documents must not be opened (privacy prompt).
	docs := e.HomePath("Documents", "dotfiles")
	writeFile(t, filepath.Join(docs, "bashrc"), "echo hi\n", 0o644)
	if err := os.Symlink(filepath.Join(docs, "bashrc"), e.HomePath(".bashrc")); err != nil {
		t.Fatal(err)
	}
	// Relative link to a normal place is followed.
	writeFile(t, e.HomePath("dots", "tmux.conf"), "set -g mouse on\n", 0o644)
	if err := os.Symlink("dots/tmux.conf", e.HomePath(".tmux.conf")); err != nil {
		t.Fatal(err)
	}

	s := section(t, e, "dotfiles")
	z := find(s, "~/.zshrc")
	if z == nil {
		t.Fatalf("no .zshrc: %+v", s.Items)
	}
	if strings.Contains(z.Detail, token[4:16]) || !strings.Contains(z.Detail, "[REDACTED]") {
		t.Errorf("token not redacted: %q", z.Detail)
	}
	if strings.Contains(z.Detail, e.Home) || !strings.Contains(z.Detail, "~/bin") {
		t.Errorf("home not normalised: %q", z.Detail)
	}
	var bash, tmux *snapshot.Item
	for i := range s.Items {
		switch {
		case strings.HasPrefix(s.Items[i].Key, "~/.bashrc"):
			bash = &s.Items[i]
		case strings.HasPrefix(s.Items[i].Key, "~/.tmux.conf"):
			tmux = &s.Items[i]
		}
	}
	if bash == nil || bash.Detail != "" || !strings.Contains(bash.Value, "not read") {
		t.Errorf("protected link was read: %+v", bash)
	}
	if tmux == nil || !strings.Contains(tmux.Detail, "mouse on") {
		t.Errorf("relative link not followed: %+v", tmux)
	}
}

func TestSSHNeverReadsPrivateKeys(t *testing.T) {
	e := sandbox(t)
	writeFile(t, e.HomePath(".ssh", "config"), "Host box\n  HostName box.example.org\n  User me\n  IdentityFile ~/.ssh/id_ed25519\n", 0o600)
	// Unreadable, so reading it would fail loudly rather than silently.
	writeFile(t, e.HomePath(".ssh", "id_ed25519"), "not a key", 0o000)
	writeFile(t, e.HomePath(".ssh", "id_ed25519.pub"), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBIF3EjoB8kpLi6d3mzCJBc07SmUkQUA9bBQ8x3rRnkU someone@example.org\n", 0o644)
	s := section(t, e, "ssh")
	if it := find(s, "private key › id_ed25519"); it == nil || it.Value != "present" {
		t.Errorf("private key presence: %+v", s.Items)
	}
	pub := find(s, "public key › id_ed25519.pub")
	if pub == nil || !strings.HasPrefix(pub.Value, "ssh-ed25519 SHA256:") || strings.Contains(pub.Value, "example.org") {
		t.Errorf("public key fingerprint: %+v", pub)
	}
	if h := find(s, "host › box"); h == nil || !strings.Contains(h.Value, "HostName box.example.org") {
		t.Errorf("host entry: %+v", s.Items)
	}
}

func TestKeyboard(t *testing.T) {
	e := sandbox(t)
	plist := func(domain, body string) {
		writeFile(t, filepath.Join(e.Root, "defaults", domain+".plist"), `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>`+body+`</dict></plist>`, 0o644)
	}
	plist("com.apple.symbolichotkeys", `<key>AppleSymbolicHotKeys</key><dict>
		<key>64</key><dict><key>enabled</key><true/><key>value</key><dict><key>parameters</key><array><integer>32</integer><integer>49</integer><integer>1048576</integer></array><key>type</key><string>standard</string></dict></dict>
		<key>60</key><dict><key>enabled</key><false/></dict>
		<key>999</key><dict><key>enabled</key><integer>1</integer></dict>
	</dict>`)
	plist("com.apple.universalaccess", `<key>com.apple.custommenu.apps</key><array><string>com.example.editor</string><string>bad domain; rm</string></array>`)
	plist("com.example.editor", `<key>NSUserKeyEquivalents</key><dict><key>Save As…</key><string>@$s</string></dict>`)
	plist("NSGlobalDomain", `<key>NSUserKeyEquivalents</key><dict><key>Zoom</key><string>^~z</string></dict>`)
	plist("com.apple.HIToolbox", `<key>AppleEnabledInputSources</key><array><dict><key>KeyboardLayout Name</key><string>Czech</string></dict></array>`)
	s := section(t, e, "keyboard")
	for key, want := range map[string]string{
		"system › Show Spotlight search":            "on cmd+space",
		"system › Select the previous input source": "off",
		"system › hotkey 999":                       "on",
		"app › com.example.editor › Save As…":       "cmd+shift+s",
		"app › all apps › Zoom":                     "ctrl+opt+z",
		"input source › Czech":                      "enabled",
	} {
		if it := find(s, key); it == nil || it.Value != want {
			t.Errorf("%s = %+v, want %q", key, it, want)
		}
	}
	if got := menuCombo("@$s"); got != "cmd+shift+s" {
		// order follows the stored string
		t.Logf("menuCombo(@$s) = %s", got)
	}
}

func TestBrewAndMas(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "brew", `case "$1 $2" in
"list --formula") printf 'jq 1.7.1\noniguruma 6.9.9\n';;
"list --cask") printf 'alt-tab 7.2\n';;
"leaves --installed-on-request") echo jq;;
"tap ") echo owner/tools;;
esac`)
	stub(t, e, "mas", `printf '409183694  Keynote  (14.2)\n'`)
	s := section(t, e, "brew")
	if it := find(s, "formula › jq"); it == nil || it.Value != "1.7.1" || it.Tag != "requested" {
		t.Errorf("jq: %+v", it)
	}
	if it := find(s, "formula › oniguruma"); it == nil || it.Tag != "dependency" {
		t.Errorf("oniguruma: %+v", it)
	}
	if find(s, "cask › alt-tab") == nil || find(s, "tap › owner/tools") == nil {
		t.Errorf("casks/taps: %+v", s.Items)
	}
	m := section(t, e, "mas")
	if it := find(m, "Keynote"); it == nil || it.Value != "14.2" || it.Tag != "id 409183694" {
		t.Errorf("mas: %+v", m.Items)
	}
	e.Path = t.TempDir()
	if s := section(t, e, "brew"); s.Status != snapshot.Absent {
		t.Errorf("no brew should be absent: %+v", s)
	}
}

func TestShortcutsEmptyListIsUnavailableNotEmpty(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "shortcuts", `exit 0`)
	for _, ssh := range []bool{true, false} {
		e.SSH = ssh
		if s := section(t, e, "shortcuts"); s.Status != snapshot.Unavailable {
			t.Fatalf("empty list (ssh %v) reported as %s", ssh, s.Status)
		}
	}
	stub(t, e, "shortcuts", `[ "$2" = "--folders" ] && { echo Work; exit 0; }
[ "$2" = "--folder-name" ] && { echo "Start timer"; exit 0; }
printf 'Start timer\nResize image\n'`)
	s := section(t, e, "shortcuts")
	if s.Status != snapshot.OK || find(s, "Start timer").Value != "Work" || find(s, "Resize image").Value != "(no folder)" {
		t.Fatalf("shortcuts: %+v", s)
	}
}

func TestStubsThatWouldOpenDialogsAreNotRun(t *testing.T) {
	e := sandbox(t)
	// A link on PATH to a /usr/bin stub is the same stub.
	if _, err := os.Stat("/usr/bin/java"); err == nil && runtime.GOOS == "darwin" {
		link := filepath.Join(e.Path, "java")
		if err := os.Symlink("/usr/bin/java", link); err != nil {
			t.Fatal(err)
		}
		if !e.stubWouldPrompt("java", link) {
			t.Error("link to the java stub would be run")
		}
		if _, err := e.Out("java", "-version"); err == nil {
			t.Error("Out ran the java stub")
		}
		os.Remove(link)
	}
	if !e.stubWouldPrompt("java", "/usr/bin/java") {
		t.Error("java stub without a JDK would be run")
	}
	writeFile(t, filepath.Join(e.Root, "Library", "Java", "JavaVirtualMachines", "jdk", "x"), "", 0o644)
	if e.stubWouldPrompt("java", "/usr/bin/java") {
		t.Error("java with a JDK present treated as a stub")
	}
	if e.stubWouldPrompt("node", "/opt/homebrew/bin/node") {
		t.Error("non-stub treated as stub")
	}
	e.OS = "linux"
	if e.stubWouldPrompt("java", "/usr/bin/java") {
		t.Error("linux has no stubs")
	}
}

func TestApps(t *testing.T) {
	e := sandbox(t)
	info := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>2.1</string><key>CFBundleVersion</key><string>210</string><key>CFBundleIdentifier</key><string>com.example.foo</string></dict></plist>`
	writeFile(t, filepath.Join(e.Root, "Applications", "Foo.app", "Contents", "Info.plist"), info, 0o644)
	writeFile(t, filepath.Join(e.Root, "Applications", "Utilities", "Bar.app", "Contents", "Info.plist"), info, 0o644)
	writeFile(t, e.HomePath("Applications", "Baz.app", "Contents", "Info.plist"), info, 0o644)
	s := section(t, e, "apps")
	for _, k := range []string{"Foo.app", "Utilities/Bar.app", "~/Applications/Baz.app"} {
		if it := find(s, k); it == nil || it.Value != "2.1" || !strings.Contains(it.Detail, "build 210") {
			t.Errorf("%s: %+v", k, s.Items)
		}
	}
}

func TestCrashingCollectorIsContained(t *testing.T) {
	e := sandbox(t)
	sec := runOne(e, Collector{Kind: "x", Title: "X", Run: func(*Env, *snapshot.Section) { panic("boom") }})
	if sec.Status != snapshot.Failed || !strings.Contains(sec.Note, "boom") {
		t.Fatalf("%+v", sec)
	}
}

// On macOS a link can spell the home folder with different capitals; it is
// still the protected Documents folder and still the home to normalise.
func TestProtectedFolderCaseInsensitive(t *testing.T) {
	e := sandbox(t)
	upper := strings.Replace(e.Home, "someone", "SomeOne", 1)
	docs := e.HomePath("Documents")
	writeFile(t, filepath.Join(docs, "zshrc"), "echo hi\n", 0o644)
	if err := os.Symlink(filepath.Join(upper, "Documents", "zshrc"), e.HomePath(".zshrc")); err != nil {
		t.Fatal(err)
	}
	s := section(t, e, "dotfiles")
	z := find(s, "~/.zshrc")
	if z == nil || z.Detail != "" || !strings.HasPrefix(z.Value, "not read") {
		t.Fatalf("differently-cased protected link was read: %+v", s.Items)
	}
	if strings.Contains(z.Value, "SomeOne") || !strings.Contains(z.Value, "~/Documents/zshrc") {
		t.Errorf("home not normalised case-insensitively: %q", z.Value)
	}
}

// The Shortcuts folder must not be touched at all, locally or over ssh
// (privacy prompt, or a read that blocks forever): here it is a folder the
// test cannot read, which must not matter.
func TestShortcutsNeverReadsTheFolder(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "shortcuts", `printf 'Timer\n'`)
	dir := e.HomePath("Library", "Shortcuts")
	writeFile(t, filepath.Join(dir, "x"), "", 0o644)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	for _, ssh := range []bool{true, false} {
		e.SSH = ssh
		s := section(t, e, "shortcuts")
		if s.Status != snapshot.OK || find(s, "Timer") == nil {
			t.Fatalf("ssh %v: the folder permission must not matter: %+v", ssh, s)
		}
	}
}

// Values split from their names (git config --list) and plist key/string
// pairs lose the key=value shape the text redaction looks for.
func TestSplitAndStructuredSecretsRedacted(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "git", `printf 'user.name=Someone\ngithub.token=opaque-value-12345\ncredential.https://example.com.password=s3cretpass\ncredential.helper=osxkeychain\n'`)
	g := section(t, e, "git")
	if it := find(g, "github.token"); it == nil || it.Value != "[REDACTED]" {
		t.Errorf("git token: %+v", g.Items)
	}
	if it := find(g, "credential.https://example.com.password"); it == nil || it.Value != "[REDACTED]" {
		t.Errorf("git password: %+v", g.Items)
	}
	if it := find(g, "credential.helper"); it == nil || it.Value != "osxkeychain" {
		t.Errorf("harmless git value redacted: %+v", g.Items)
	}

	writeFile(t, e.HomePath("Library", "LaunchAgents", "x.test.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>x.test</string><key>ProgramArguments</key><array><string>/bin/echo</string><string>--password</string><string>opaque-arg-999</string></array><key>EnvironmentVariables</key><dict><key>API_TOKEN</key><string>opaque-env-555</string></dict></dict></plist>`, 0o644)
	l := section(t, e, "launchd")
	it := find(l, "user agent › x.test")
	if it == nil || strings.Contains(it.Detail+it.Value, "opaque-") || !strings.Contains(it.Detail, "API_TOKEN") {
		t.Errorf("launchd plist secrets: %+v", l.Items)
	}

	e2 := sandbox(t)
	stub(t, e2, "shortcuts", `printf 'Resize image\nTOKEN=opaque-name-4242\n'`)
	if s := section(t, e2, "shortcuts"); find(s, "Resize image") == nil || strings.Contains(fmt.Sprint(s.Items), "opaque-name") {
		t.Errorf("shortcut names: %+v", s.Items)
	}
}

// Terminal control characters collected from a machine never reach a
// snapshot, where a terminal would later interpret them.
func TestControlCharactersStripped(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "brew", `case "$1 $2" in
"list --formula") printf 'evil\033[2J\r 1.0\n';;
esac`)
	writeFile(t, e.HomePath(".zshrc"), "alias x='echo \033]0;title\007'\n", 0o644)
	s := Snapshot(e, Options{Only: []string{"brew", "dotfiles"}})
	for _, sec := range s.Sections {
		for _, it := range sec.Items {
			if strings.ContainsAny(it.Key+it.Value+it.Detail+it.Tag, "\x1b\r\x07") {
				t.Errorf("%s: control character kept in %q", sec.Kind, it)
			}
		}
	}
}

// A FIFO where a dotfile belongs would block the open forever.
func TestReadFileSkipsSpecialFiles(t *testing.T) {
	e := sandbox(t)
	p := e.HomePath(".hushlogin")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan snapshot.Section, 1)
	go func() { done <- section(t, e, "dotfiles") }()
	select {
	case s := <-done:
		if it := find(s, "~/.hushlogin"); it == nil || !strings.HasPrefix(it.Value, "unreadable") {
			t.Errorf("fifo: %+v", s.Items)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dotfiles blocked on a FIFO")
	}
	if _, ok := ReadFile(p, 10); ok {
		t.Error("ReadFile accepted a FIFO")
	}
	big := e.HomePath("big")
	writeFile(t, big, strings.Repeat("x", 100), 0o644)
	if _, ok := ReadFile(big, 10); ok {
		t.Error("ReadFile ignored the limit")
	}
}

// A folder on the way that links into Documents is as protected as a link
// on the file itself: even stat'ing a path through it can raise a prompt.
func TestParentFolderLinkedIntoProtectedFolder(t *testing.T) {
	e := sandbox(t)
	writeFile(t, e.HomePath("Documents", "cfg", "fish", "config.fish"), "set -gx EDITOR vim\n", 0o644)
	if err := os.Symlink(e.HomePath("Documents", "cfg"), e.HomePath(".config")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.HomePath("Documents", "ssh", "config"), "Host x\n", 0o644)
	if err := os.Symlink(e.HomePath("Documents", "ssh"), e.HomePath(".ssh")); err != nil {
		t.Fatal(err)
	}
	s := section(t, e, "dotfiles")
	it := find(s, "~/.config/fish/config.fish")
	if it == nil || it.Detail != "" || !strings.HasPrefix(it.Value, "not read") {
		t.Errorf("file under a protected parent link was read: %+v", s.Items)
	}
	if sh := section(t, e, "ssh"); sh.Status != snapshot.Unavailable || len(sh.Items) != 0 {
		t.Errorf("ssh folder linked into Documents was read: %+v", sh)
	}
}

// python -c puts the current folder (the home folder) first on the import
// path; a json.py there must not be imported, which would run it.
func TestPythonProbeIgnoresModulesInHome(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil || filepath.Dir(py) == "/usr/bin" {
		t.Skip("no python3 outside /usr/bin")
	}
	e := sandbox(t)
	e.OS = "linux" // no stub checks for the real interpreter
	stub(t, e, "python3", `exec `+py+` "$@"`)
	marker := filepath.Join(t.TempDir(), "ran")
	writeFile(t, e.HomePath("json.py"), "open("+strconv.Quote(marker)+", 'w').write('x')\n", 0o644)
	out, err := e.Out("python3", "-E", "-c", pythonProbe)
	if err != nil || !strings.Contains(out, `"pkgs"`) {
		t.Fatalf("probe failed: %v %s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("json.py from the home folder was imported")
	}
}

func TestLibraries(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "python3", `echo '{"v": "3.12", "pkgs": [["requests", "2.32.3", "user"], ["pip", "24.0", "system"]]}'`)
	stub(t, e, "gem", `printf 'rake (13.2.1)\njson (default: 2.7.1)\nminitest (5.20.0, default: 5.16.3)\n'`)
	stub(t, e, "perl", `printf 'Moose\t2.2206\n'`)
	stub(t, e, "composer", `echo '{"installed":[{"name":"laravel/installer","version":"v5.8.0"}]}'`)
	stub(t, e, "Rscript", `printf 'dplyr\t1.1.4\n'`)
	stub(t, e, "luarocks", `printf 'lpeg\t1.1.0-1\tinstalled\t/usr/local/lib/luarocks/rocks\n'`)
	writeFile(t, e.HomePath(".julia", "environments", "v1.10", "Project.toml"), "[deps]\nPlots = \"91a5bcdd-55d7-5caf-9e0b-520d859cae80\"\n\n[compat]\nPlots = \"1\"\n", 0o644)
	s := section(t, e, "libraries")
	for key, want := range map[string]string{
		"python3.12 › requests":        "2.32.3/user",
		"python3.12 › pip":             "24.0/system",
		"gem › rake":                   "13.2.1/",
		"gem › json":                   "2.7.1/default",
		"gem › minitest":               "5.20.0, 5.16.3/",
		"perl › Moose":                 "2.2206/",
		"composer › laravel/installer": "v5.8.0/",
		"R › dplyr":                    "1.1.4/",
		"julia v1.10 › Plots":          "added/",
		"luarocks › lpeg":              "1.1.0-1/",
	} {
		it := find(s, key)
		if it == nil || it.Value+"/"+it.Tag != want {
			t.Errorf("%s = %+v, want %s", key, it, want)
		}
	}
	if find(s, "julia v1.10 › Plots\" = \"1") != nil || len(s.Items) != 10 {
		t.Errorf("compat section leaked or extra items: %+v", s.Items)
	}
}

func TestToolchains(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "pyenv", `printf '3.11.9\n3.12.4\n'`)
	stub(t, e, "rustup", `case "$1" in
toolchain) printf 'stable-aarch64-apple-darwin (default)\nnightly-aarch64-apple-darwin\n';;
component) printf 'clippy-aarch64-apple-darwin\n';;
esac`)
	stub(t, e, "asdf", `printf 'nodejs\n  20.11.0\n *22.1.0\nruby\n  No versions installed\n'`)
	writeFile(t, e.HomePath(".nvm", "versions", "node", "v20.11.0", "bin", "node"), "", 0o755)
	writeFile(t, filepath.Join(e.Root, "Library", "Java", "JavaVirtualMachines", "temurin-21.jdk", "x"), "", 0o644)
	writeFile(t, e.HomePath(".sdkman", "candidates", "java", "21.0.2-tem", "x"), "", 0o644)
	if err := os.Symlink("21.0.2-tem", e.HomePath(".sdkman", "candidates", "java", "current")); err != nil {
		t.Fatal(err)
	}
	s := section(t, e, "toolchains")
	for key, want := range map[string]string{
		"pyenv › 3.12.4":                                 "installed",
		"rustup › stable-aarch64-apple-darwin":           "default",
		"rustup › nightly-aarch64-apple-darwin":          "installed",
		"rustup component › clippy-aarch64-apple-darwin": "installed",
		"asdf › nodejs 22.1.0":                           "installed",
		"nvm › v20.11.0":                                 "installed",
		"jdk › temurin-21.jdk":                           "installed",
		"sdkman java › 21.0.2-tem":                       "installed",
	} {
		if it := find(s, key); it == nil || it.Value != want {
			t.Errorf("%s = %+v, want %s", key, it, want)
		}
	}
	for _, it := range s.Items {
		if strings.Contains(it.Key, "No versions") || strings.Contains(it.Key, "current") {
			t.Errorf("unexpected %q", it.Key)
		}
	}
}
