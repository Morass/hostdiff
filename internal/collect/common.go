package collect

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/morass/hostdiff/internal/redact"
	"github.com/morass/hostdiff/internal/snapshot"
)

func system(e *Env, s *snapshot.Section) {
	s.Add("architecture", runtime.GOARCH, "")
	switch e.OS {
	case "darwin":
		if v, err := e.Out("sw_vers", "-productVersion"); err == nil {
			s.Add("macOS", strings.TrimSpace(v), "")
		}
		if v, err := e.Out("sw_vers", "-buildVersion"); err == nil {
			s.Add("macOS build", strings.TrimSpace(v), "")
		}
		if runtime.GOARCH == "arm64" {
			_, err := os.Stat(e.Sys("/Library/Apple/usr/share/rosetta/rosetta"))
			s.Add("Rosetta", yesNo(err == nil, "installed", "not installed"), "")
		}
		if v, err := e.Out("xcode-select", "-p"); err == nil {
			s.Add("developer directory", strings.TrimSpace(v), "")
		}
		if b, ok := ReadFile(e.Sys("/Applications/Xcode.app/Contents/version.plist"), 1<<20); ok {
			if m, err := decodePlist(b); err == nil {
				s.Add("Xcode", str(m["CFBundleShortVersionString"]), "")
			}
		}
		if out, err := e.Out("pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables"); err == nil {
			for _, l := range Lines(out) {
				if v, ok := strings.CutPrefix(l, "version: "); ok {
					s.Add("command line tools", v, "")
				}
			}
		}
		if e.User != "" {
			if out, err := e.Out("dscl", ".", "-read", "/Users/"+e.User, "UserShell"); err == nil {
				if _, v, ok := strings.Cut(strings.TrimSpace(out), ": "); ok {
					s.Add("login shell", v, "")
				}
			}
		}
	case "linux":
		if b, ok := ReadFile(e.Sys("/etc/os-release"), 1<<16); ok {
			for _, l := range Lines(string(b)) {
				if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
					s.Add("distribution", strings.Trim(v, `"`), "")
				}
			}
		}
		if v, err := e.Out("uname", "-r"); err == nil {
			s.Add("kernel", strings.TrimSpace(v), "")
		}
		if e.User != "" {
			if out, err := e.Out("getent", "passwd", e.User); err == nil {
				if f := strings.Split(strings.TrimSpace(out), ":"); len(f) >= 7 {
					s.Add("login shell", f[6], "")
				}
			}
		}
	}
	if target, err := os.Readlink(e.Sys("/etc/localtime")); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			s.Add("time zone", target[i+len("zoneinfo/"):], "")
		}
	}
	if out, err := e.Out("lpstat", "-p"); err == nil {
		for _, l := range Lines(out) {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "printer" {
				s.Add("printer › "+f[1], "configured", "")
			}
		}
	}
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func brew(e *Env, s *snapshot.Section) {
	if e.Look("brew") == "" {
		s.Status, s.Note = snapshot.Absent, "Homebrew is not installed"
		return
	}
	leaves := map[string]bool{}
	if out, err := e.Out("brew", "leaves", "--installed-on-request"); err == nil {
		for _, l := range Lines(out) {
			leaves[l] = true
		}
	}
	formulae, err := e.Out("brew", "list", "--formula", "--versions")
	if err != nil {
		s.Status, s.Note = snapshot.Failed, "brew list: "+err.Error()
		return
	}
	for _, l := range Lines(formulae) {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		detail := "dependency"
		if leaves[f[0]] {
			detail = "requested"
		}
		s.AddTag("formula › "+f[0], strings.Join(f[1:], " "), detail)
	}
	if casks, err := e.Out("brew", "list", "--cask", "--versions"); err == nil {
		for _, l := range Lines(casks) {
			if f := strings.Fields(l); len(f) >= 2 {
				s.Add("cask › "+f[0], strings.Join(f[1:], " "), "")
			}
		}
	}
	if taps, err := e.Out("brew", "tap"); err == nil {
		for _, l := range Lines(taps) {
			s.Add("tap › "+l, "tapped", "")
		}
	}
}

func shell(e *Env, s *snapshot.Section) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		p := e.Look(sh)
		if p == "" {
			continue
		}
		out, err := e.OutAll(sh, "--version")
		if err != nil {
			continue
		}
		s.Add(sh, versionOf(out)+" · "+p, "")
	}
	login := os.Getenv("SHELL")
	if login == "" || e.Look(login) == "" {
		return
	}
	// The PATH a login shell builds from the user's startup files. Positions
	// matter (which node wins), so the value is the position.
	short := *e
	short.CmdTimeout = 8 * time.Second
	// Start from the minimal PATH a new login gets, so the result shows what
	// the system and the startup files add, not what hostdiff started with.
	short.Path = "/usr/bin:/bin:/usr/sbin:/sbin"
	out, err := short.Out(e.Look(login), "-lc", `printf '\n__HOSTDIFF_PATH__%s\n' "$PATH"`)
	if err != nil && out == "" {
		s.Note = "could not run a login shell to read PATH: " + err.Error()
		return
	}
	for _, l := range strings.Split(out, "\n") {
		v, ok := strings.CutPrefix(l, "__HOSTDIFF_PATH__")
		if !ok {
			continue
		}
		// Each folder is reported as present or missing, and the order as one
		// list, so a single inserted folder does not mark every later one as
		// changed.
		var order []string
		for _, dir := range strings.Split(v, ":") {
			if dir == "" {
				continue
			}
			order = append(order, dir)
			value := "present"
			if _, err := os.Stat(dir); err != nil {
				value = "missing"
			}
			s.Add("PATH › "+dir, value, "")
		}
		s.Add("PATH order", strconv.Itoa(len(order))+" folders", strings.Join(order, "\n")+"\n")
	}
}

// Files compared by content. Anything that holds credentials by design
// (.netrc, .npmrc, .pypirc, .env, cloud credential files) is deliberately not
// on this list.
var dotfileList = []string{
	".profile", ".bashrc", ".bash_profile", ".bash_login", ".bash_aliases", ".bash_logout",
	".zshrc", ".zprofile", ".zshenv", ".zlogin", ".zlogout",
	".config/fish/config.fish", ".inputrc", ".editrc",
	".tmux.conf", ".config/tmux/tmux.conf", ".screenrc",
	".vimrc", ".config/nvim/init.lua", ".config/nvim/init.vim", ".ideavimrc",
	".gitconfig", ".config/git/config", ".gitignore_global", ".config/git/ignore",
	".config/starship.toml", ".wezterm.lua", ".config/wezterm/wezterm.lua",
	".config/ghostty/config", ".config/alacritty/alacritty.toml", ".config/kitty/kitty.conf",
	".hushlogin", ".curlrc", ".wgetrc", ".editorconfig", ".ripgreprc", ".config/bat/config",
	".ssh/config", ".config/karabiner/karabiner.json", ".hammerspoon/init.lua", ".skhdrc", ".yabairc", ".aerospace.toml",
}

// Folders macOS guards with privacy prompts. Opening a file inside one from a
// command line tool can put a permission dialog on the screen, so hostdiff
// never follows a link into them.
var protectedDirs = []string{"Desktop", "Documents", "Downloads", "Pictures", "Movies", "Music", "Library/Mobile Documents", "Library/CloudStorage", "Library/Containers", "Library/Group Containers", "Library/Mail", "Library/Messages", "Library/Safari"}

// linkTarget follows a symlink chain by reading link text only, and reports
// whether it ends in a protected folder before anything there is touched.
func (e *Env) linkTarget(p string) (target string, protected bool) {
	cur := p
	for hop := 0; hop < 10; hop++ {
		st, err := os.Lstat(cur)
		if err != nil || st.Mode()&fs.ModeSymlink == 0 {
			return target, false
		}
		t, err := os.Readlink(cur)
		if err != nil {
			return target, false
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(cur), t)
		}
		t = filepath.Clean(t)
		if target == "" {
			target = t
		}
		if e.isProtected(t) {
			return target, true
		}
		cur = t
	}
	return target, true
}

// pathProtected reports whether reaching p would pass through a
// privacy-protected folder: p itself, or any folder on the way being one or
// linking into one. It reads link text only, never the folders themselves,
// so it has to run before anything under p is opened or even stat'ed.
func (e *Env) pathProtected(p string) bool {
	if e.OS != "darwin" {
		return false
	}
	p = filepath.Clean(p)
	if e.isProtected(p) {
		return true
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(p, cur), cur) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if _, protected := e.linkTarget(cur); protected {
			return true
		}
	}
	return false
}

func (e *Env) isProtected(p string) bool {
	if e.OS != "darwin" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(p), "/volumes/") {
		return true
	}
	// APFS is case-insensitive by default: a home or Documents folder spelled
	// with different capitals is still the same protected folder.
	lp := strings.ToLower(p)
	for _, d := range protectedDirs {
		dir := strings.ToLower(e.HomePath(d))
		if lp == dir || strings.HasPrefix(lp, dir+"/") {
			return true
		}
	}
	return false
}

func dotfiles(e *Env, s *snapshot.Section) {
	for _, rel := range dotfileList {
		p := e.HomePath(rel)
		key := "~/" + rel
		if e.pathProtected(filepath.Dir(p)) {
			s.Add(key, "not read: its folder links into a privacy-protected folder", "")
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		target, protected := e.linkTarget(p)
		link := ""
		if target != "" {
			link = " (→ " + target + ")"
		}
		if protected {
			s.Add(key, "not read: linked into a privacy-protected folder"+link, "")
			continue
		}
		b, ok := ReadFile(p, 512<<10)
		if !ok {
			s.Add(key, "unreadable or too large"+link, "")
			continue
		}
		// The hash is of the cleaned content, so a redacted token or a
		// different home folder does not make identical files look different.
		s.Add(key, "content "+shortHash(cleanText(e, string(b)))+link, string(b))
	}
}

func cleanText(e *Env, t string) string {
	sec := snapshot.Section{Items: []snapshot.Item{{Detail: t}}}
	clean(e, &sec)
	return sec.Items[0].Detail
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func gitConfig(e *Env, s *snapshot.Section) {
	if p := e.Look("git"); p == "" || e.stubWouldPrompt("git", p) {
		s.Status, s.Note = snapshot.Absent, "git is not installed"
		return
	}
	out, err := e.Out("git", "config", "--global", "--list")
	if err != nil && out == "" {
		// No global config at all is a normal state.
		return
	}
	seen := map[string]int{}
	for _, l := range Lines(out) {
		k, v, _ := strings.Cut(l, "=")
		// Split from its key, "github.token=abc" would no longer look like
		// a secret to the redaction every value goes through later.
		v = redact.Value(k[strings.LastIndex(k, ".")+1:], v)
		seen[k]++
		if seen[k] > 1 {
			k += " #" + itoa(seen[k])
		}
		s.Add(k, v, "")
	}
}

func sshConfig(e *Env, s *snapshot.Section) {
	dir := e.HomePath(".ssh")
	if e.pathProtected(dir) {
		s.Status, s.Note = snapshot.Unavailable, "~/.ssh links into a privacy-protected folder"
		return
	}
	if _, protected := e.linkTarget(filepath.Join(dir, "config")); protected {
		s.Add("config", "not read: linked into a privacy-protected folder", "")
	} else if f, err := os.Open(filepath.Join(dir, "config")); err == nil {
		var host string
		var opts []string
		flush := func() {
			if host != "" {
				s.Add("host › "+host, strings.Join(opts, " · "), "")
			}
			host, opts = "", nil
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			k, v := splitSSH(l)
			switch strings.ToLower(k) {
			case "host":
				flush()
				host = v
			case "match":
				flush()
				host = "match " + v
			case "include":
				s.Add("include › "+v, "included", "")
			default:
				opts = append(opts, k+" "+v)
			}
		}
		flush()
		f.Close()
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	names := map[string]bool{}
	for _, en := range entries {
		names[en.Name()] = true
	}
	for _, en := range entries {
		n := en.Name()
		switch {
		case strings.HasSuffix(n, ".pub"):
			if b, ok := ReadFile(filepath.Join(dir, n), 64<<10); ok {
				s.Add("public key › "+n, pubFingerprint(string(b)), "")
			}
		case n == "authorized_keys":
			if b, ok := ReadFile(filepath.Join(dir, n), 1<<20); ok {
				for _, l := range Lines(string(b)) {
					if !strings.HasPrefix(l, "#") {
						if fp := pubFingerprint(l); fp != "?" {
							s.Add("authorized › "+fp, "authorized", "")
						}
					}
				}
			}
		case strings.HasPrefix(n, "id_") || names[n+".pub"]:
			// A private key: record that it exists, never read it.
			s.Add("private key › "+n, "present", "")
		}
	}
}

func splitSSH(l string) (string, string) {
	if i := strings.IndexAny(l, " \t="); i > 0 {
		return l[:i], strings.TrimSpace(strings.TrimLeft(l[i:], " \t="))
	}
	return l, ""
}

// pubFingerprint returns "TYPE SHA256:..." for an OpenSSH public key line,
// the same fingerprint ssh-keygen -l prints. Options before the key type (in
// authorized_keys) are skipped; the comment is dropped.
func pubFingerprint(line string) string {
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		if strings.HasPrefix(f[i], "ssh-") || strings.HasPrefix(f[i], "ecdsa-") || strings.HasPrefix(f[i], "sk-") {
			blob, err := base64.StdEncoding.DecodeString(f[i+1])
			if err != nil {
				return "?"
			}
			sum := sha256.Sum256(blob)
			return f[i] + " SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
		}
	}
	return "?"
}

var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+(?:[-+~][0-9A-Za-z.]+)?`)

// versionOf picks the first version-looking token from a tool's output.
func versionOf(out string) string {
	for _, l := range Lines(out) {
		if v := versionRe.FindString(l); v != "" {
			return v
		}
	}
	if ls := Lines(out); len(ls) > 0 {
		l := ls[0]
		if len(l) > 60 {
			l = l[:60]
		}
		return l
	}
	return "?"
}

var toolList = []struct {
	name string
	args []string
}{
	{"node", []string{"--version"}}, {"npm", []string{"--version"}}, {"pnpm", []string{"--version"}},
	{"yarn", []string{"--version"}}, {"bun", []string{"--version"}}, {"deno", []string{"--version"}},
	{"python3", []string{"--version"}}, {"pip3", []string{"--version"}}, {"uv", []string{"--version"}},
	{"pipx", []string{"--version"}}, {"go", []string{"version"}}, {"ruby", []string{"--version"}},
	{"java", []string{"-version"}}, {"kotlin", []string{"-version"}}, {"rustc", []string{"--version"}},
	{"cargo", []string{"--version"}}, {"swift", []string{"--version"}}, {"clang", []string{"--version"}},
	{"gcc", []string{"--version"}}, {"make", []string{"--version"}}, {"cmake", []string{"--version"}},
	{"git", []string{"--version"}}, {"gh", []string{"--version"}}, {"perl", []string{"-e", "print $^V"}},
	{"php", []string{"--version"}}, {"lua", []string{"-v"}}, {"dotnet", []string{"--version"}},
	{"docker", []string{"--version"}}, {"podman", []string{"--version"}}, {"kubectl", []string{"version", "--client"}},
	{"terraform", []string{"version"}}, {"ffmpeg", []string{"-version"}}, {"jq", []string{"--version"}},
	{"rg", []string{"--version"}}, {"fzf", []string{"--version"}}, {"tmux", []string{"-V"}},
	{"vim", []string{"--version"}}, {"nvim", []string{"--version"}}, {"emacs", []string{"--version"}},
	{"sqlite3", []string{"--version"}}, {"psql", []string{"--version"}}, {"redis-server", []string{"--version"}},
	{"ollama", []string{"--version"}}, {"godot", []string{"--version"}}, {"blender", []string{"--version"}},
	{"openscad", []string{"--version"}}, {"asdf", []string{"--version"}}, {"mise", []string{"--version"}},
	{"pyenv", []string{"--version"}}, {"rbenv", []string{"--version"}}, {"volta", []string{"--version"}},
	{"rustup", []string{"--version"}}, {"direnv", []string{"--version"}}, {"nix", []string{"--version"}},
}

// On macOS, /usr/bin holds stubs for developer tools and Java. Running one
// without the command line tools or a JDK opens an install dialog on the
// screen, so stubs are only run when what they forward to exists.
var devStubs = map[string]bool{
	"clang": true, "gcc": true, "make": true, "git": true, "python3": true, "pip3": true,
	"swift": true, "cmake": true, "lldb": true,
}

func (e *Env) stubWouldPrompt(name, path string) bool {
	if e.OS != "darwin" || (!devStubs[name] && name != "java" && name != "kotlin") {
		return false
	}
	// A link elsewhere on PATH (~/bin/python3 -> /usr/bin/python3) is the
	// same stub.
	if filepath.Dir(path) != "/usr/bin" {
		real, err := filepath.EvalSymlinks(path)
		if err != nil || filepath.Dir(real) != "/usr/bin" {
			return false
		}
	}
	if name == "java" || name == "kotlin" {
		entries, err := os.ReadDir(e.Sys("/Library/Java/JavaVirtualMachines"))
		return err != nil || len(entries) == 0
	}
	cltMu.Lock()
	defer cltMu.Unlock()
	ok, known := cltFound[e.Path]
	if !known {
		out, err := e.Out("xcode-select", "-p")
		if err == nil {
			_, err = os.Stat(strings.TrimSpace(out))
		}
		ok = err == nil
		cltFound[e.Path] = ok
	}
	return !ok
}

// cltFound caches, per PATH, whether the command line tools are installed;
// every run of a /usr/bin developer tool asks.
var (
	cltMu    sync.Mutex
	cltFound = map[string]bool{}
)

func runtimes(e *Env, s *snapshot.Section) {
	type res struct{ key, value string }
	results := make([]res, len(toolList))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, t := range toolList {
		p := e.Look(t.name)
		if p == "" {
			continue
		}
		if e.stubWouldPrompt(t.name, p) {
			s.Add(t.name, "not installed (system stub) · "+p, "")
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			short := *e
			short.CmdTimeout = 10 * time.Second
			out, err := short.OutAll(p, t.args...)
			v := versionOf(out)
			if err != nil && !versionRe.MatchString(out) {
				v = "does not run (" + firstLine(out, err) + ")"
			}
			results[i] = res{t.name, v + " · " + p}
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r.key != "" {
			s.Add(r.key, r.value, "")
		}
	}
	if _, err := os.Stat(e.HomePath(".nvm")); err == nil {
		alias, _ := ReadFile(e.HomePath(".nvm", "alias", "default"), 1024)
		s.Add("nvm", "default "+orDash(strings.TrimSpace(string(alias))), "")
	}
}

func firstLine(out string, err error) string {
	if ls := Lines(out); len(ls) > 0 {
		l := ls[0]
		if len(l) > 60 {
			l = l[:60]
		}
		return l
	}
	return err.Error()
}

func packages(e *Env, s *snapshot.Section) {
	if out, err := e.Out("npm", "ls", "-g", "--depth=0", "--json"); err == nil || out != "" {
		var tree struct {
			Dependencies map[string]struct {
				Version string `json:"version"`
			} `json:"dependencies"`
		}
		if json.Unmarshal([]byte(out), &tree) == nil {
			for name, d := range tree.Dependencies {
				s.Add("npm › "+name, orDash(d.Version), "")
			}
		}
	}
	if out, err := e.Out("pipx", "list", "--short"); err == nil {
		for _, l := range Lines(out) {
			if f := strings.Fields(l); len(f) >= 2 {
				s.Add("pipx › "+f[0], f[1], "")
			}
		}
	}
	if out, err := e.Out("uv", "tool", "list"); err == nil {
		for _, l := range Lines(out) {
			if strings.HasPrefix(l, "-") {
				continue
			}
			if f := strings.Fields(l); len(f) >= 2 {
				s.Add("uv › "+f[0], strings.TrimPrefix(f[1], "v"), "")
			}
		}
	}
	if out, err := e.Out("cargo", "install", "--list"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if l == "" || strings.HasPrefix(l, " ") {
				continue
			}
			f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(l), ":"))
			if len(f) >= 2 {
				s.Add("cargo › "+f[0], strings.TrimPrefix(f[1], "v"), "")
			}
		}
	}
	gobin := os.Getenv("GOBIN")
	if gobin == "" {
		gobin = e.HomePath("go", "bin")
	}
	if entries, err := os.ReadDir(gobin); err == nil {
		for _, en := range entries {
			if !en.IsDir() && !strings.HasPrefix(en.Name(), ".") {
				s.Add("go › "+en.Name(), "installed", "")
			}
		}
	}
	morePackages(e, s)
	if out, err := e.Out("gh", "extension", "list"); err == nil {
		for _, l := range Lines(out) {
			if f := strings.Split(l, "\t"); len(f) >= 2 {
				v := "-"
				if len(f) >= 3 {
					v = strings.TrimSpace(f[2])
				}
				s.Add("gh › "+strings.TrimSpace(f[1]), v, "")
			}
		}
	}
}

func editors(e *Env, s *snapshot.Section) {
	type cli struct{ label, name, bundle string }
	for _, c := range []cli{
		{"vscode", "code", "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"},
		{"cursor", "cursor", "/Applications/Cursor.app/Contents/Resources/app/bin/cursor"},
		{"vscodium", "codium", "/Applications/VSCodium.app/Contents/Resources/app/bin/codium"},
	} {
		bin := e.Look(c.name)
		if bin == "" && e.OS == "darwin" && isExec(e.Sys(c.bundle)) {
			bin = e.Sys(c.bundle)
		}
		if bin == "" {
			continue
		}
		out, err := e.Out(bin, "--list-extensions", "--show-versions")
		if err != nil {
			continue
		}
		for _, l := range Lines(out) {
			name, ver, _ := strings.Cut(l, "@")
			s.Add(c.label+" › "+name, orDash(ver), "")
		}
	}
	plugins := func(label string, globs ...string) {
		for _, g := range globs {
			matches, _ := filepath.Glob(e.HomePath(g))
			for _, m := range matches {
				if st, err := os.Stat(m); err == nil && st.IsDir() {
					s.Add(label+" › "+filepath.Base(m), "installed", "")
				}
			}
		}
	}
	plugins("neovim", ".local/share/nvim/lazy/*", ".local/share/nvim/site/pack/*/start/*", ".local/share/nvim/site/pack/*/opt/*")
	plugins("vim", ".vim/pack/*/start/*", ".vim/pack/*/opt/*", ".vim/plugged/*", ".vim/bundle/*")
	plugins("tmux", ".tmux/plugins/*", ".config/tmux/plugins/*")
}

func fonts(e *Env, s *snapshot.Section) {
	var dirs []struct{ dir, label string }
	if e.OS == "darwin" {
		dirs = append(dirs, struct{ dir, label string }{e.HomePath("Library", "Fonts"), "user"}, struct{ dir, label string }{e.Sys("/Library/Fonts"), "system"})
	} else {
		dirs = append(dirs, struct{ dir, label string }{e.HomePath(".local", "share", "fonts"), "user"}, struct{ dir, label string }{e.HomePath(".fonts"), "user"}, struct{ dir, label string }{e.Sys("/usr/local/share/fonts"), "system"})
	}
	fontExt := map[string]bool{".ttf": true, ".otf": true, ".ttc": true, ".otc": true, ".dfont": true, ".woff": true, ".woff2": true}
	for _, d := range dirs {
		_ = filepath.WalkDir(d.dir, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if de.IsDir() && strings.Count(strings.TrimPrefix(p, d.dir), string(os.PathSeparator)) > 2 {
				return filepath.SkipDir
			}
			if !de.IsDir() && fontExt[strings.ToLower(filepath.Ext(p))] {
				// The path inside the folder is a tag: it is not compared
				// (the same font in another subfolder is the same font) but
				// it is what a copy needs.
				rel := strings.TrimPrefix(strings.TrimPrefix(p, d.dir), string(os.PathSeparator))
				s.AddTag(de.Name(), d.label, rel)
			}
			return nil
		})
	}
}

func cron(e *Env, s *snapshot.Section) {
	if e.Look("crontab") == "" {
		s.Status, s.Note = snapshot.Absent, "crontab is not installed"
		return
	}
	out, _ := e.Out("crontab", "-l")
	for _, l := range Lines(out) {
		if !strings.HasPrefix(l, "#") {
			// The line is the key, and keys only get token-shape redaction,
			// so apply the full secret rules here.
			line, _ := redact.Secrets(l)
			s.Add(line, "scheduled", "")
		}
	}
}

func etcFiles(e *Env, s *snapshot.Section) {
	if b, ok := ReadFile(e.Sys("/etc/hosts"), 1<<20); ok {
		for _, l := range Lines(string(b)) {
			if strings.HasPrefix(l, "#") {
				continue
			}
			if i := strings.Index(l, "#"); i >= 0 {
				l = l[:i]
			}
			f := strings.Fields(l)
			for _, name := range f[min(1, len(f)):] {
				s.Add("hosts › "+name, f[0], "")
			}
		}
	}
	listFile := func(p, label string) {
		if b, ok := ReadFile(e.Sys(p), 1<<20); ok {
			for i, l := range Lines(string(b)) {
				if !strings.HasPrefix(l, "#") {
					s.Add(label+" › "+l, "#"+itoa(i+1), "")
				}
			}
		}
	}
	listFile("/etc/paths", "paths")
	listFile("/etc/shells", "shells")
	for _, dir := range []string{"/etc/paths.d", "/etc/resolver"} {
		entries, err := os.ReadDir(e.Sys(dir))
		if err != nil {
			continue
		}
		for _, en := range entries {
			if b, ok := ReadFile(filepath.Join(e.Sys(dir), en.Name()), 64<<10); ok {
				lines := Lines(string(b))
				sort.Strings(lines)
				s.Add(filepath.Base(dir)+" › "+en.Name(), strings.Join(lines, "; "), "")
			}
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
