// Package collect reads the state of the machine it runs on into a snapshot.
// Every collector only reads; nothing here writes outside hostdiff's own
// output. All collected text passes through one choke point (clean) that
// removes secrets and replaces the home directory with ~ before it enters the
// snapshot.
package collect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/morass/hostdiff/internal/redact"
	"github.com/morass/hostdiff/internal/snapshot"
)

// Env is the machine as collectors see it. Tests point Home, Root and Path at
// fixtures; the real binary uses the current user's values.
type Env struct {
	Home string
	OS   string
	// Root prefixes system paths (/Applications, /Library, /etc). Empty in
	// normal use; set by HOSTDIFF_SYSROOT in tests.
	Root string
	// Path is the PATH used to find and run tools.
	Path string
	User string
	// SSH is true when running inside an ssh session.
	SSH bool
	// CmdTimeout bounds each external command.
	CmdTimeout time.Duration
}

// Collector produces one section.
type Collector struct {
	Kind  string
	Title string
	// Reads says in plain words what the collector looks at.
	Reads string
	// OS limits the collector to "darwin" or "linux"; empty means both.
	OS  string
	Run func(e *Env, s *snapshot.Section)
}

// Current returns the environment of the running process.
func Current() *Env {
	home, _ := os.UserHomeDir()
	e := &Env{
		Home:       home,
		OS:         runtime.GOOS,
		Root:       os.Getenv("HOSTDIFF_SYSROOT"),
		Path:       os.Getenv("PATH"),
		SSH:        os.Getenv("SSH_CONNECTION") != "",
		CmdTimeout: 20 * time.Second,
	}
	if u, err := user.Current(); err == nil {
		e.User = u.Username
	}
	if e.Root != "" {
		// Test sandbox: only the PATH it was given.
		return e
	}
	// Non-interactive ssh sessions often start with a minimal PATH; add the
	// usual install folders so the same tools are found as in a terminal.
	for _, d := range []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin", filepath.Join(home, ".local", "bin"), filepath.Join(home, ".cargo", "bin"), filepath.Join(home, "go", "bin")} {
		if !pathHas(e.Path, d) {
			e.Path += string(os.PathListSeparator) + d
		}
	}
	return e
}

func pathHas(path, dir string) bool {
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return true
		}
	}
	return false
}

// Sys maps an absolute system path under Root.
func (e *Env) Sys(p string) string {
	if e.Root == "" {
		return p
	}
	return filepath.Join(e.Root, p)
}

// HomePath joins parts under the home directory.
func (e *Env) HomePath(parts ...string) string {
	return filepath.Join(append([]string{e.Home}, parts...)...)
}

// Look finds a program on Path.
func (e *Env) Look(name string) string {
	if strings.Contains(name, "/") {
		if isExec(name) {
			return name
		}
		return ""
	}
	for _, d := range filepath.SplitList(e.Path) {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if isExec(p) {
			return p
		}
	}
	return ""
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// errNotFound is returned by Out when the program is not installed.
var errNotFound = errors.New("not installed")

// Out runs a program and returns its standard output. Standard input is
// empty, so nothing can wait for a prompt.
func (e *Env) Out(name string, args ...string) (string, error) {
	out, _, err := e.run(false, name, args...)
	return out, err
}

// OutAll runs a program and returns standard output and error together
// (some tools print their version on stderr).
func (e *Env) OutAll(name string, args ...string) (string, error) {
	out, _, err := e.run(true, name, args...)
	return out, err
}

func (e *Env) run(both bool, name string, args ...string) (string, string, error) {
	bin := e.Look(name)
	if bin == "" {
		return "", "", errNotFound
	}
	timeout := e.CmdTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = e.childEnv()
	if st, err := os.Stat(e.Home); err == nil && st.IsDir() {
		cmd.Dir = e.Home
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if both {
		cmd.Stderr = &stdout
	} else {
		cmd.Stderr = &stderr
	}
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%s timed out after %s", name, timeout)
	}
	return stdout.String(), stderr.String(), err
}

func (e *Env) childEnv() []string {
	env := []string{
		"HOME=" + e.Home,
		"PATH=" + e.Path,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=dumb",
		"NO_COLOR=1",
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_ANALYTICS=1",
		"HOMEBREW_NO_ENV_HINTS=1",
	}
	for _, k := range []string{"USER", "LOGNAME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "SHELL"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// ReadFile reads a file up to limit bytes; ok is false if it is missing,
// unreadable or larger than the limit.
func ReadFile(p string, limit int64) ([]byte, bool) {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Size() > limit {
		return nil, false
	}
	b, err := os.ReadFile(p)
	return b, err == nil
}

// Lines splits output into trimmed, non-empty lines.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Options select what to collect.
type Options struct {
	Only []string
	Skip []string
	Tool string
}

// Snapshot runs the collectors that apply to this OS, in parallel, and returns
// the cleaned result.
func Snapshot(e *Env, opt Options) *snapshot.Snapshot {
	host, _ := os.Hostname()
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i]
	}
	snap := &snapshot.Snapshot{
		Format:  snapshot.Format,
		Tool:    opt.Tool,
		Created: time.Now().UTC().Truncate(time.Second),
		Host:    snapshot.Host{Name: host, OS: e.OS, Arch: runtime.GOARCH},
	}
	var cols []Collector
	for _, c := range All() {
		if c.OS != "" && c.OS != e.OS {
			continue
		}
		if len(opt.Only) > 0 && !contains(opt.Only, c.Kind) {
			continue
		}
		if contains(opt.Skip, c.Kind) {
			continue
		}
		cols = append(cols, c)
	}
	snap.Sections = make([]snapshot.Section, len(cols))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, c := range cols {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			snap.Sections[i] = runOne(e, c)
		}()
	}
	wg.Wait()
	return snap
}

func runOne(e *Env, c Collector) (sec snapshot.Section) {
	sec = snapshot.Section{Kind: c.Kind, Title: c.Title, Status: snapshot.OK}
	start := time.Now()
	defer func() {
		if os.Getenv("HOSTDIFF_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "hostdiff: %-10s %6s %d items\n", c.Kind, time.Since(start).Round(10*time.Millisecond), len(sec.Items))
		}
		if r := recover(); r != nil {
			sec.Status, sec.Note, sec.Items = snapshot.Failed, fmt.Sprintf("collector crashed: %v", r), nil
		}
		clean(e, &sec)
		sec.Sort()
	}()
	c.Run(e, &sec)
	return sec
}

// clean is the single place collected text passes before it is stored:
// secrets are redacted and the home directory becomes ~, so two machines
// with different user names compare equal.
func clean(e *Env, sec *snapshot.Section) {
	var homeRe *regexp.Regexp
	if e.Home != "" && e.Home != "/" {
		// Case-insensitive on macOS, where /Users/Name and /Users/name are
		// the same folder; never inside a longer name (/Users/name2).
		flags := ""
		if e.OS == "darwin" {
			flags = "(?i)"
		}
		homeRe = regexp.MustCompile(flags + regexp.QuoteMeta(e.Home) + `(/|\b|$)`)
	}
	norm := func(s string) string {
		if homeRe != nil {
			s = homeRe.ReplaceAllString(s, "~$1")
		}
		return strings.ToValidUTF8(s, "?")
	}
	text := func(s string) string {
		s, _ = redact.Secrets(s)
		return norm(s)
	}
	// Keys are names (git config keys, paths, labels): only unmistakable
	// credential shapes are removed there. Collectors that use free text as a
	// key apply redact.Secrets themselves.
	name := func(s string) string {
		s, _ = redact.Shapes(s)
		return norm(s)
	}
	sec.Note = text(sec.Note)
	for i := range sec.Items {
		it := &sec.Items[i]
		it.Key, it.Value, it.Detail, it.Tag = name(it.Key), text(it.Value), text(it.Detail), text(it.Tag)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// All returns every collector in display order.
func All() []Collector {
	return []Collector{
		{Kind: "system", Title: "System", Reads: "OS version, architecture, Rosetta, Xcode and command line tools, login shell, time zone, printers", Run: system},
		{Kind: "brew", Title: "Homebrew", Reads: "brew list --versions (formulae, casks), brew leaves, brew tap", Run: brew},
		{Kind: "mas", Title: "App Store", Reads: "mas list", OS: "darwin", Run: mas},
		{Kind: "apps", Title: "Applications", Reads: "/Applications and ~/Applications bundles with versions (Info.plist)", OS: "darwin", Run: apps},
		{Kind: "syspkg", Title: "System packages", Reads: "apt-mark showmanual, dnf, pacman -Qe, snap list, flatpak list", OS: "linux", Run: sysPackages},
		{Kind: "shortcuts", Title: "Shortcuts", Reads: "the shortcuts command (names and folders)", OS: "darwin", Run: shortcuts},
		{Kind: "keyboard", Title: "Keyboard shortcuts", Reads: "system hotkeys (com.apple.symbolichotkeys), app menu shortcuts (NSUserKeyEquivalents in ~/Library/Preferences), Services, input sources", OS: "darwin", Run: keyboard},
		{Kind: "defaults", Title: "macOS settings", Reads: "a curated list of Dock, Finder, keyboard, trackpad, screenshot and window settings via defaults export", OS: "darwin", Run: macDefaults},
		{Kind: "launchd", Title: "Launch agents and daemons", Reads: "plists in ~/Library/LaunchAgents, /Library/LaunchAgents, /Library/LaunchDaemons; launchctl print-disabled", OS: "darwin", Run: launchd},
		{Kind: "systemd", Title: "systemd units", Reads: "enabled system and user unit files", OS: "linux", Run: systemd},
		{Kind: "shell", Title: "Shell", Reads: "shell versions and the PATH a login shell builds", Run: shell},
		{Kind: "dotfiles", Title: "Dotfiles", Reads: "shell, git, editor and terminal config files (content, secrets redacted)", Run: dotfiles},
		{Kind: "git", Title: "Git config", Reads: "git config --global --list (secrets redacted)", Run: gitConfig},
		{Kind: "ssh", Title: "SSH", Reads: "Host entries in ~/.ssh/config, public key fingerprints, which private key files exist (never their content)", Run: sshConfig},
		{Kind: "runtimes", Title: "Runtimes and tools", Reads: "versions and locations of language runtimes, compilers and common CLI tools", Run: runtimes},
		{Kind: "packages", Title: "Global packages", Reads: "npm -g, pipx, uv tool, cargo install, go bin, gh extensions", Run: packages},
		{Kind: "editors", Title: "Editors", Reads: "VS Code / Cursor / VSCodium extensions, Vim and Neovim plugins, tmux plugins", Run: editors},
		{Kind: "fonts", Title: "Fonts", Reads: "user and system font files", Run: fonts},
		{Kind: "cron", Title: "Crontab", Reads: "crontab -l (secrets redacted)", Run: cron},
		{Kind: "etc", Title: "System files", Reads: "/etc/hosts, /etc/paths, /etc/paths.d, /etc/resolver, /etc/shells", Run: etcFiles},
		{Kind: "power", Title: "Power settings", Reads: "pmset -g custom", OS: "darwin", Run: power},
	}
}

// Kinds lists all section kinds, sorted.
func Kinds() []string {
	var out []string
	for _, c := range All() {
		out = append(out, c.Kind)
	}
	sort.Strings(out)
	return out
}

// withDeadline runs f and gives up waiting after d. A filesystem call that
// macOS holds for a privacy decision cannot be cancelled, so the goroutine is
// left behind; it ends with the process.
func withDeadline(d time.Duration, f func() error) (err error, done bool) {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	select {
	case err = <-ch:
		return err, true
	case <-time.After(d):
		return nil, false
	}
}
