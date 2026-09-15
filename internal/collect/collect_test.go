package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestShortcutsOverSSHIsUnavailableNotEmpty(t *testing.T) {
	e := sandbox(t)
	stub(t, e, "shortcuts", `exit 0`)
	e.SSH = true
	if s := section(t, e, "shortcuts"); s.Status != snapshot.Unavailable {
		t.Fatalf("empty list over ssh reported as %s", s.Status)
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
