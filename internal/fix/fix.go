// Package fix turns a comparison into commands that make one machine more
// like the other: a reviewable script (`--script`, the `s` view) and the
// step-by-step installer the interactive view and `--install` run.
// Everything taken from a snapshot is untrusted (it may come from another
// machine), so names are validated, commands are argument lists rather than
// shell text, and anything unusual is left out with a sanitised note.
package fix

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/snapshot"
)

var (
	// Names only: no ":" (npm file:, git+https:), no ".." segments. Some
	// ecosystems read other shapes as a location, so they get their own.
	pkgRe       = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9@._+/-]{0,200}$`)
	npmRe       = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._~-]*/)?[A-Za-z0-9][A-Za-z0-9._~-]{0,200}$`)
	pypiRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)
	cargoRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,100}$`)
	juliaEnvRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,40}$`)
	digitsRe    = regexp.MustCompile(`^[0-9]{1,15}$`)
	domainRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)
	defKeyRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._ -]{0,200}$`)
	intRe       = regexp.MustCompile(`^-?[0-9]{1,18}$`)
	floatRe     = regexp.MustCompile(`^-?[0-9]{1,18}(\.[0-9]{1,18})?([eE][-+]?[0-9]{1,3})?$`)
	controlRe   = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	safeWordRe  = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	pyLabelRe   = regexp.MustCompile(`^python3\.[0-9]{1,2}$`)
	rNameRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]{0,100}$`)
	juliaNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,100}$`)
	perlNameRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(::[A-Za-z0-9_]+)*$`)
	versionRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,60}$`)
)

// Action is what hostdiff can do about one difference on the machine that
// lacks something. Argv is nil when there is nothing it can run; Note then
// says why (or carries advice, such as a version difference).
type Action struct {
	Kind string
	Key  string
	// Verb is what the action does: install, update, remove, set or reset.
	Verb string
	Argv []string
	Note string
}

// Verbs.
const (
	Install = "install"
	Copy    = "copy"
	Update  = "update"
	Remove  = "remove"
	Set     = "set"
	Reset   = "reset"
)

// Runnable reports whether the action has a command.
func (a Action) Runnable() bool { return len(a.Argv) > 0 }

// Command renders Argv as one shell command line. Words made only of
// characters a shell treats literally stay bare; everything else is
// single-quoted.
func (a Action) Command() string {
	words := make([]string, len(a.Argv))
	for i, w := range a.Argv {
		if safeWordRe.MatchString(w) && !strings.HasPrefix(w, "=") {
			words[i] = w
		} else {
			words[i] = Quote(w)
		}
	}
	return strings.Join(words, " ")
}

// Quote single-quotes s for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// comment makes text safe inside a # comment: no newlines or control
// characters that could end the comment and start a command.
func comment(s string) string {
	s = controlRe.ReplaceAllString(s, "?")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// nameRe is the shape a package name must have for the tool behind prefix,
// so that nothing but a registry name reaches it.
func nameRe(prefix string) *regexp.Regexp {
	switch strings.TrimSuffix(prefix, " › ") {
	case "npm", "pnpm":
		return npmRe
	case "pipx", "uv":
		return pypiRe
	case "cargo":
		return cargoRe
	}
	return pkgRe
}

// validName checks a name against re and refuses path-like tricks.
func validName(re *regexp.Regexp, n string) bool {
	return re.MatchString(n) && !strings.Contains(n, "..") && !strings.HasSuffix(n, "/") && !strings.Contains(n, "//")
}

func run(kind, key string, argv ...string) Action {
	return Action{Kind: kind, Key: key, Argv: argv}
}

func note(kind, key, format string, args ...any) Action {
	return Action{Kind: kind, Key: key, Note: fmt.Sprintf(format, args...)}
}

func skipped(kind, key, why string) Action {
	return note(kind, key, "skipped: %s", why)
}

// ForMissing returns what brings over an item that only the other machine
// has. The second result is false when hostdiff has nothing to say about
// this kind of item at all (a font, a launch agent).
func ForMissing(kind string, it snapshot.Item) (a Action, ok bool) {
	defer func() { a.Verb = verb(kind, Install) }()
	key := it.Key
	prefixed := func(prefix string, cmd ...string) (Action, bool) {
		n, ok := strings.CutPrefix(key, prefix)
		if !ok {
			return Action{}, false
		}
		if !validName(nameRe(prefix), n) {
			return skipped(kind, key, "unusual characters"), true
		}
		return run(kind, key, append(cmd, n)...), true
	}
	first := func(options ...func() (Action, bool)) (Action, bool) {
		for _, o := range options {
			if a, ok := o(); ok {
				return a, true
			}
		}
		return Action{}, false
	}
	switch kind {
	case "brew":
		if strings.HasPrefix(key, "formula › ") && it.Tag == "dependency" {
			return note(kind, key, "installed there only as a dependency; it comes with whatever needs it"), true
		}
		return first(
			func() (Action, bool) { return prefixed("formula › ", "brew", "install") },
			func() (Action, bool) { return prefixed("cask › ", "brew", "install", "--cask") },
			func() (Action, bool) { return prefixed("tap › ", "brew", "tap") },
		)
	case "mas":
		if id, ok := strings.CutPrefix(it.Tag, "id "); ok && digitsRe.MatchString(id) {
			return run(kind, key, "mas", "install", id), true
		}
		return skipped(kind, key, "no App Store id"), true
	case "packages":
		return first(
			func() (Action, bool) { return prefixed("npm › ", "npm", "install", "-g") },
			func() (Action, bool) { return prefixed("pnpm › ", "pnpm", "add", "-g") },
			func() (Action, bool) { return prefixed("pipx › ", "pipx", "install") },
			func() (Action, bool) { return prefixed("uv › ", "uv", "tool", "install") },
			func() (Action, bool) { return prefixed("cargo › ", "cargo", "install") },
			func() (Action, bool) { return prefixed("gh › ", "gh", "extension", "install") },
			func() (Action, bool) { return prefixed("dotnet › ", "dotnet", "tool", "install", "-g") },
		)
	case "editors":
		return first(
			func() (Action, bool) { return prefixed("vscode › ", "code", "--install-extension") },
			func() (Action, bool) { return prefixed("cursor › ", "cursor", "--install-extension") },
			func() (Action, bool) { return prefixed("vscodium › ", "codium", "--install-extension") },
		)
	case "defaults":
		return writeDefault(key, it.Value, it.Tag), true
	case "libraries":
		return installLibrary(key, it.Tag)
	case "toolchains":
		return installToolchain(key)
	}
	return Action{}, false
}

// fontDir is where a user's own fonts live.
// firstOS is the optional operating-system argument of ForRemove.
func firstOS(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// fontRoots are the folders, inside the home folder, that hold a user's
// fonts; the first is where a copied font goes.
var fontRoots = map[string][]string{
	"darwin": {"Library/Fonts"},
	"linux":  {".local/share/fonts", ".fonts"},
}

// fontNameRe refuses everything a double-quoted shell word would still read
// as code: quotes, $, backquote, backslash and control characters.
var fontNameRe = regexp.MustCompile(`^[^"'` + "`" + `$\\\x00-\x1f]{1,300}$`)

// fontInside splits a home-relative font path into its root folder and the
// part inside it, or false when it is not in a user font folder.
func fontInside(osName, homeRel string) (root, rel string, ok bool) {
	if !fontNameRe.MatchString(homeRel) || strings.Contains(homeRel, "..") || strings.HasPrefix(homeRel, "/") {
		return "", "", false
	}
	for os, roots := range fontRoots {
		if osName != "" && os != osName {
			continue
		}
		for _, r := range roots {
			if in, found := strings.CutPrefix(homeRel, r+"/"); found && in != "" {
				return r, in, true
			}
		}
	}
	return "", "", false
}

func quoted(homeRel string) string { return `"$HOME/` + homeRel + `"` }

func parent(homeRel string) string {
	if i := strings.LastIndex(homeRel, "/"); i > 0 {
		return homeRel[:i]
	}
	return homeRel
}

// ForCopy returns the command that copies a user font between this machine
// and one reached over ssh; it runs here. from and to are the home-relative
// paths: the font where it is, and where it goes on the other side (the
// same place inside that system's font folder). sshOpts are the options of
// the run's shared connection. The file is written to a temporary name and
// renamed, so a link at the destination is replaced, never followed, and a
// failed transfer leaves nothing half written.
func ForCopy(kind string, it snapshot.Item, localOS, remoteOS, dest string, sshOpts []string, toRemote bool) (a Action, ok bool) {
	defer func() { a.Verb = Copy }()
	if kind != "fonts" || dest == "" {
		return Action{}, false
	}
	if it.Value != "user" {
		return note(kind, it.Key, "a font for all users: copy it yourself, it needs an administrator"), true
	}
	srcOS, dstOS := localOS, remoteOS
	if !toRemote {
		srcOS, dstOS = remoteOS, localOS
	}
	_, rel, found := fontInside(srcOS, it.Tag)
	if !found || len(fontRoots[dstOS]) == 0 {
		return skipped(kind, it.Key, "not a font file in a user font folder, or a name a shell would read as code"), true
	}
	src := it.Tag
	dst := fontRoots[dstOS][0] + "/" + rel
	tmp := dst + ".hostdiff-part"
	ssh := "ssh"
	for _, o := range sshOpts {
		ssh += " " + Quote(o)
	}
	ssh += " -- " + Quote(dest)
	check := "test -f " + quoted(src) + " && test ! -L " + quoted(src)
	if toRemote {
		remote := "mkdir -p " + quoted(parent(dst)) + " && cat > " + quoted(tmp) + " && mv -f " + quoted(tmp) + " " + quoted(dst)
		return run(kind, it.Key, "/bin/sh", "-c", check+" && "+ssh+" "+Quote(remote)+" < "+quoted(src)), true
	}
	return run(kind, it.Key, "/bin/sh", "-c",
		"mkdir -p "+quoted(parent(dst))+" && "+ssh+" "+Quote(check+" && cat "+quoted(src))+" > "+quoted(tmp)+
			" && mv -f "+quoted(tmp)+" "+quoted(dst)+" || { rm -f "+quoted(tmp)+"; exit 1; }"), true
}

// ForChange returns what makes the machine that has c's "have" side match
// the "want" side.
func ForChange(kind string, key, want, wantTag, have, haveTag string) (a Action, ok bool) {
	defer func() { a.Verb = verb(kind, Update) }()
	switch kind {
	case "defaults":
		return writeDefault(key, want, wantTag), true
	case "brew", "mas", "packages", "editors", "apps", "libraries":
		if wantTag == "dependency" && haveTag == "dependency" {
			return Action{}, false
		}
		return note(kind, key, "version differs: %s here, %s there (not updated by the script)", comment(have), comment(want)), true
	}
	return Action{}, false
}

// verb names settings changes as set and reset.
func verb(kind, v string) string {
	if kind != "defaults" {
		return v
	}
	if v == Remove {
		return Reset
	}
	return Set
}

// ForRemove returns what removes an item from the machine that has it.
// osName is the operating system of the machine the removal runs on; it is
// only needed where the path depends on it (fonts).
func ForRemove(kind string, it snapshot.Item, osName ...string) (a Action, ok bool) {
	defer func() { a.Verb = verb(kind, Remove) }()
	key := it.Key
	prefixed := func(prefix string, cmd ...string) (Action, bool) {
		n, ok := strings.CutPrefix(key, prefix)
		if !ok {
			return Action{}, false
		}
		if !validName(nameRe(prefix), n) {
			return skipped(kind, key, "unusual characters"), true
		}
		return run(kind, key, append(cmd, n)...), true
	}
	first := func(options ...func() (Action, bool)) (Action, bool) {
		for _, o := range options {
			if a, ok := o(); ok {
				return a, true
			}
		}
		return Action{}, false
	}
	switch kind {
	case "brew":
		return first(
			func() (Action, bool) { return prefixed("formula › ", "brew", "uninstall") },
			func() (Action, bool) { return prefixed("cask › ", "brew", "uninstall", "--cask") },
			func() (Action, bool) { return prefixed("tap › ", "brew", "untap") },
		)
	case "mas":
		if id, ok := strings.CutPrefix(it.Tag, "id "); ok && digitsRe.MatchString(id) {
			return run(kind, key, "sudo", "mas", "uninstall", id), true
		}
		return skipped(kind, key, "no App Store id"), true
	case "packages":
		return first(
			func() (Action, bool) { return prefixed("npm › ", "npm", "uninstall", "-g") },
			func() (Action, bool) { return prefixed("pnpm › ", "pnpm", "remove", "-g") },
			func() (Action, bool) { return prefixed("pipx › ", "pipx", "uninstall") },
			func() (Action, bool) { return prefixed("uv › ", "uv", "tool", "uninstall") },
			func() (Action, bool) { return prefixed("cargo › ", "cargo", "uninstall") },
			func() (Action, bool) { return prefixed("gh › ", "gh", "extension", "remove") },
			func() (Action, bool) { return prefixed("dotnet › ", "dotnet", "tool", "uninstall", "-g") },
		)
	case "editors":
		return first(
			func() (Action, bool) { return prefixed("vscode › ", "code", "--uninstall-extension") },
			func() (Action, bool) { return prefixed("cursor › ", "cursor", "--uninstall-extension") },
			func() (Action, bool) { return prefixed("vscodium › ", "codium", "--uninstall-extension") },
		)
	case "defaults":
		domain, _ := defaultsTag(it.Tag)
		_, k, found := strings.Cut(key, " › ")
		if !found || domain == "" {
			return skipped(kind, key, "not a simple value"), true
		}
		if !domainRe.MatchString(domain) || !defKeyRe.MatchString(k) {
			return skipped(kind, key, "unusual characters"), true
		}
		return run(kind, key, "defaults", "delete", domain, k), true
	case "libraries":
		return removeLibrary(key, it.Tag)
	case "toolchains":
		return removeToolchain(key)
	case "apps":
		return note(kind, key, "move the app to the Trash yourself (or remove its cask under Homebrew)"), true
	case "fonts":
		if it.Value != "user" {
			return note(kind, key, "a font for all users: remove it yourself, it needs an administrator"), true
		}
		// The tag is where the font is on the machine that has it.
		if _, _, found := fontInside(firstOS(osName), it.Tag); !found {
			return skipped(kind, key, "not a font file in a user font folder, or a name a shell would read as code"), true
		}
		p := quoted(it.Tag)
		return run(kind, key, "/bin/sh", "-c", "test ! -L "+p+" && rm -f "+p), true
	}
	return Action{}, false
}

// ForUpdate returns what brings the version (or value) the machine has, have,
// to the other machine's, want. Where a tool cannot install a given version,
// it upgrades to the newest instead and the command says so.
func ForUpdate(kind, key, want, wantTag, have, haveTag string) (a Action, ok bool) {
	defer func() { a.Verb = verb(kind, Update) }()
	label, name, hasLabel := strings.Cut(key, " › ")
	v := strings.TrimPrefix(want, "v")
	if !versionRe.MatchString(v) {
		v = ""
	}
	pinned := func(cmd ...string) (Action, bool) {
		if !validName(nameRe(label), name) {
			return skipped(kind, key, "unusual characters"), true
		}
		if v == "" {
			return note(kind, key, "the other version (%s) is not a single version", comment(want)), true
		}
		return run(kind, key, cmd...), true
	}
	switch kind {
	case "defaults":
		return writeDefault(key, want, wantTag), true
	case "brew":
		if !hasLabel || !pkgRe.MatchString(name) {
			return Action{}, false
		}
		switch label {
		case "formula":
			return run(kind, key, "brew", "upgrade", name), true
		case "cask":
			return run(kind, key, "brew", "upgrade", "--cask", name), true
		}
	case "mas":
		tag := haveTag
		if tag == "" {
			tag = wantTag
		}
		if id, found := strings.CutPrefix(tag, "id "); found && digitsRe.MatchString(id) {
			return run(kind, key, "mas", "upgrade", id), true
		}
	case "packages":
		switch label {
		case "npm":
			return pinned("npm", "install", "-g", name+"@"+v)
		case "pnpm":
			return pinned("pnpm", "add", "-g", name+"@"+v)
		case "pipx":
			return pinned("pipx", "install", "--force", name+"=="+v)
		case "uv":
			return pinned("uv", "tool", "install", "--force", name+"=="+v)
		case "cargo":
			return pinned("cargo", "install", "--force", "--version", v, name)
		case "gh":
			if pkgRe.MatchString(name) {
				return run(kind, key, "gh", "extension", "upgrade", name), true
			}
		case "dotnet":
			return pinned("dotnet", "tool", "update", "-g", name, "--version", v)
		}
	case "editors":
		cli := map[string]string{"vscode": "code", "cursor": "cursor", "vscodium": "codium"}[label]
		if cli != "" {
			return pinned(cli, "--install-extension", name+"@"+v, "--force")
		}
	case "libraries":
		switch {
		case pyLabelRe.MatchString(label):
			if haveTag != "user" && wantTag != "user" {
				return note(kind, key, "installed system-wide; update it the way that %s is managed", label), true
			}
			return pinned(label, "-m", "pip", "install", "--user", name+"=="+v)
		case label == "gem":
			return pinned("gem", "install", name, "-v", v)
		case label == "composer":
			return pinned("composer", "global", "require", name+":"+want)
		case label == "luarocks":
			return pinned("luarocks", "install", name, v)
		case label == "dart":
			return pinned("dart", "pub", "global", "activate", name, v)
		case label == "perl" && perlNameRe.MatchString(name):
			return pinned("cpanm", name+"@"+v)
		}
		return note(kind, key, "no generic command to install a given version"), true
	case "apps":
		return note(kind, key, "update the app itself (or its cask under Homebrew)"), true
	}
	return Action{}, false
}

// Clone returns everything that makes one side like the other within the
// compared sections: onA true changes A to match B. Installs come first,
// then updates and settings, removals last.
func Clone(r *diff.Result, onA bool) []Action {
	var installs, updates, removals []Action
	targetOS := ""
	if snap := r.B.Snap; !onA && snap != nil {
		targetOS = snap.Host.OS
	}
	if snap := r.A.Snap; onA && snap != nil {
		targetOS = snap.Host.OS
	}
	for i := range r.Sections {
		s := &r.Sections[i]
		if !s.Comparable {
			continue
		}
		missing, extra := s.OnlyA, s.OnlyB
		if onA {
			missing, extra = s.OnlyB, s.OnlyA
		}
		var in []Action
		for _, it := range missing {
			if a, ok := ForMissing(s.Kind, it); ok {
				in = append(in, a)
			}
		}
		sort.SliceStable(in, func(i, j int) bool { return brewOrder(in[i].Key) < brewOrder(in[j].Key) })
		installs = append(installs, in...)
		for _, c := range s.Changed {
			want, wantTag, have, haveTag := c.A, c.TagA, c.B, c.TagB
			if onA {
				want, wantTag, have, haveTag = c.B, c.TagB, c.A, c.TagA
			}
			if a, ok := ForUpdate(s.Kind, c.Key, want, wantTag, have, haveTag); ok {
				updates = append(updates, a)
			}
		}
		// Only remove what the source could have had: when the source lists
		// nothing at all of a kind (no casks, no gems), that is as likely a
		// listing that failed or a tool that is missing there as a choice.
		sourceHas := map[string]bool{}
		for _, list := range [][]snapshot.Item{s.Same, missing} {
			for _, it := range list {
				sourceHas[partOf(it.Key)] = true
			}
		}
		for _, c := range s.Changed {
			sourceHas[partOf(c.Key)] = true
		}
		var out []Action
		for _, it := range extra {
			if s.Kind == "brew" && it.Tag == "dependency" {
				continue // leaves with whatever needed it
			}
			if !sourceHas[partOf(it.Key)] {
				out = append(out, note(s.Kind, it.Key, "not removed: the other machine lists nothing of this kind, which may mean its list could not be read"))
				continue
			}
			if a, ok := ForRemove(s.Kind, it, targetOS); ok {
				out = append(out, a)
			}
		}
		// Casks and formulae before the taps they came from.
		sort.SliceStable(out, func(i, j int) bool { return brewOrder(out[i].Key) > brewOrder(out[j].Key) })
		removals = append(removals, out...)
	}
	return append(append(installs, updates...), removals...)
}

// partOf is the part of a section a key belongs to: "cask" for
// "cask › rectangle", "" for keys without one.
func partOf(key string) string {
	if p, _, ok := strings.Cut(key, " › "); ok {
		return p
	}
	return ""
}

func removeLibrary(key, tag string) (Action, bool) {
	const kind = "libraries"
	label, name, ok := strings.Cut(key, " › ")
	if !ok {
		return Action{}, false
	}
	valid := func(re *regexp.Regexp, argv ...string) (Action, bool) {
		if !re.MatchString(name) {
			return skipped(kind, key, "unusual characters"), true
		}
		return run(kind, key, argv...), true
	}
	switch {
	case pyLabelRe.MatchString(label):
		if tag != "user" {
			return note(kind, key, "installed system-wide; remove it the way that %s is managed", label), true
		}
		return valid(pypiRe, label, "-m", "pip", "uninstall", "--yes", name)
	case label == "gem":
		if tag == "default" {
			return note(kind, key, "ships with Ruby"), true
		}
		return valid(pypiRe, "gem", "uninstall", "--all", "--executables", name)
	case label == "perl":
		return valid(perlNameRe, "cpanm", "--uninstall", "--force", name)
	case label == "composer":
		return valid(pkgRe, "composer", "global", "remove", name)
	case label == "R":
		return valid(rNameRe, "Rscript", "-e", "remove.packages('"+name+"')")
	case strings.HasPrefix(label, "julia "):
		env := strings.TrimPrefix(label, "julia ")
		if !juliaEnvRe.MatchString(env) {
			return skipped(kind, key, "unusual environment name"), true
		}
		return valid(juliaNameRe, "julia", "--project=@"+env, "-e", `using Pkg; Pkg.rm("`+name+`")`)
	case label == "luarocks":
		return valid(pkgRe, "luarocks", "remove", name)
	case label == "dart":
		return valid(pkgRe, "dart", "pub", "global", "deactivate", name)
	}
	return Action{}, false
}

func removeToolchain(key string) (Action, bool) {
	const kind = "toolchains"
	label, name, ok := strings.Cut(key, " › ")
	if !ok {
		return Action{}, false
	}
	fields := strings.Fields(name)
	valid := len(fields) > 0
	for _, f := range fields {
		valid = valid && versionRe.MatchString(f)
	}
	if !valid {
		return skipped(kind, key, "unusual characters"), true
	}
	switch label {
	case "pyenv":
		return run(kind, key, "pyenv", "uninstall", "--force", name), true
	case "rbenv":
		return run(kind, key, "rbenv", "uninstall", "--force", name), true
	case "rustup":
		return run(kind, key, "rustup", "toolchain", "uninstall", name), true
	case "asdf":
		if len(fields) == 2 {
			return run(kind, key, "asdf", "uninstall", fields[0], fields[1]), true
		}
	case "mise":
		if len(fields) == 2 {
			return run(kind, key, "mise", "uninstall", fields[0]+"@"+fields[1]), true
		}
	case "uv python":
		return run(kind, key, "uv", "python", "uninstall", name), true
	case "nvm":
		return note(kind, key, "nvm is a shell function: run nvm uninstall %s in your shell", name), true
	}
	return note(kind, key, "installed with %s; no generic remove command", label), true
}

// defaultsTag reads the domain and type the collector recorded for a setting.
func defaultsTag(tag string) (domain, typ string) {
	for _, l := range strings.Split(tag, "\n") {
		if v, ok := strings.CutPrefix(l, "defaults "); ok {
			domain = v
		}
		if v, ok := strings.CutPrefix(l, "type "); ok {
			typ = v
		}
	}
	return domain, typ
}

func installLibrary(key, tag string) (Action, bool) {
	const kind = "libraries"
	label, name, ok := strings.Cut(key, " › ")
	if !ok {
		return Action{}, false
	}
	valid := func(re *regexp.Regexp, argv ...string) (Action, bool) {
		if !re.MatchString(name) {
			return skipped(kind, key, "unusual characters"), true
		}
		return run(kind, key, argv...), true
	}
	switch {
	case pyLabelRe.MatchString(label):
		if !validName(pypiRe, name) {
			return skipped(kind, key, "unusual characters"), true
		}
		if tag != "user" {
			return note(kind, key, "installed system-wide there; install it the way that %s is managed", label), true
		}
		return run(kind, key, label, "-m", "pip", "install", "--user", name), true
	case label == "gem":
		if tag == "default" {
			return note(kind, key, "ships with Ruby"), true
		}
		return valid(pypiRe, "gem", "install", name)
	case label == "perl":
		return valid(perlNameRe, "cpanm", name)
	case label == "composer":
		return valid(pkgRe, "composer", "global", "require", name)
	case label == "R":
		return valid(rNameRe, "Rscript", "-e", "install.packages('"+name+"', repos = 'https://cloud.r-project.org')")
	case strings.HasPrefix(label, "julia "):
		env := strings.TrimPrefix(label, "julia ")
		if !juliaEnvRe.MatchString(env) {
			return skipped(kind, key, "unusual environment name"), true
		}
		return valid(juliaNameRe, "julia", "--project=@"+env, "-e", `using Pkg; Pkg.add("`+name+`")`)
	case label == "luarocks":
		return valid(pkgRe, "luarocks", "install", name)
	case label == "dart":
		return valid(pkgRe, "dart", "pub", "global", "activate", name)
	}
	return Action{}, false
}

func installToolchain(key string) (Action, bool) {
	const kind = "toolchains"
	label, name, ok := strings.Cut(key, " › ")
	if !ok {
		return Action{}, false
	}
	fields := strings.Fields(name)
	valid := len(fields) > 0
	for _, f := range fields {
		valid = valid && versionRe.MatchString(f)
	}
	if !valid {
		return skipped(kind, key, "unusual characters"), true
	}
	switch label {
	case "pyenv":
		return run(kind, key, "pyenv", "install", "--skip-existing", name), true
	case "rbenv":
		return run(kind, key, "rbenv", "install", "--skip-existing", name), true
	case "rustup":
		return run(kind, key, "rustup", "toolchain", "install", name), true
	case "asdf":
		if len(fields) == 2 {
			return run(kind, key, "asdf", "install", fields[0], fields[1]), true
		}
	case "mise":
		if len(fields) == 2 {
			return run(kind, key, "mise", "install", fields[0]+"@"+fields[1]), true
		}
	case "uv python":
		return run(kind, key, "uv", "python", "install", name), true
	case "rustup component":
		return note(kind, key, "run rustup component add for it (the name includes the target)"), true
	case "nvm":
		return note(kind, key, "nvm is a shell function: run nvm install %s in your shell", name), true
	}
	return note(kind, key, "installed there with %s; no generic install command", label), true
}

// writeDefault returns `defaults write` for a scalar setting whose domain and
// type were recorded by the collector.
func writeDefault(key, value, tag string) Action {
	const kind = "defaults"
	domain, typ := defaultsTag(tag)
	_, k, ok := strings.Cut(key, " › ")
	if !ok || domain == "" || typ == "" {
		return skipped(kind, key, "not a simple value")
	}
	if !domainRe.MatchString(domain) || !defKeyRe.MatchString(k) {
		return skipped(kind, key, "unusual characters")
	}
	switch typ {
	case "bool":
		if value != "true" && value != "false" {
			return skipped(kind, key, "not a boolean")
		}
	case "int":
		if !intRe.MatchString(value) {
			return skipped(kind, key, "not an integer")
		}
	case "float":
		if !floatRe.MatchString(value) {
			return skipped(kind, key, "not a number")
		}
	case "string":
		if controlRe.MatchString(value) {
			return skipped(kind, key, "control characters")
		}
	default:
		return skipped(kind, key, "unsupported type")
	}
	return run(kind, key, "defaults", "write", domain, k, "-"+typ, value)
}

// Plan returns every action that makes one side more like the other: onA
// true brings what only B has to A, false the reverse. Taps come before
// formulae, formulae before casks.
func Plan(r *diff.Result, onA bool) []Action {
	var out []Action
	for i := range r.Sections {
		s := &r.Sections[i]
		if !s.Comparable {
			continue
		}
		missing := s.OnlyA
		if onA {
			missing = s.OnlyB
		}
		var acts []Action
		for _, it := range missing {
			if a, ok := ForMissing(s.Kind, it); ok {
				acts = append(acts, a)
			}
		}
		sort.SliceStable(acts, func(i, j int) bool { return brewOrder(acts[i].Key) < brewOrder(acts[j].Key) })
		out = append(out, acts...)
		for _, c := range s.Changed {
			want, wantTag, have, haveTag := c.A, c.TagA, c.B, c.TagB
			if onA {
				want, wantTag, have, haveTag = c.B, c.TagB, c.A, c.TagA
			}
			if a, ok := ForChange(s.Kind, c.Key, want, wantTag, have, haveTag); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

func brewOrder(key string) int {
	switch {
	case strings.HasPrefix(key, "tap › "):
		return 0
	case strings.HasPrefix(key, "formula › "):
		return 1
	}
	return 2
}

// Script returns the commands, run on A, that bring over what only B has, and
// comments for the rest (removals and version differences are never
// automatic). A is the first side, normally the machine you are on.
func Script(r *diff.Result) string {
	a, b := comment(r.A.Label), comment(r.B.Label)
	lines := []string{
		"#!/bin/sh",
		fmt.Sprintf("# Generated by hostdiff: run on %s to bring over what %s has.", a, b),
		"# Review every line first. Removals and upgrades are listed as comments only.",
		"set -e",
	}
	var skippedLines []string
	byKind := map[string][]Action{}
	for _, act := range Plan(r, true) {
		byKind[act.Kind] = append(byKind[act.Kind], act)
	}
	for i := range r.Sections {
		s := &r.Sections[i]
		var body []string
		for _, act := range byKind[s.Kind] {
			switch {
			case act.Runnable():
				body = append(body, act.Command())
			case act.Kind == "brew" && strings.Contains(act.Note, "as a dependency"):
				// Comes with whatever needs it; not worth a line.
			case strings.HasPrefix(act.Note, "skipped: "):
				skippedLines = append(skippedLines, "# skipped "+comment(act.Kind+" "+act.Key)+": "+comment(strings.TrimPrefix(act.Note, "skipped: ")))
			default:
				body = append(body, "# "+comment(act.Key)+": "+comment(act.Note))
			}
		}
		if s.Comparable {
			for _, it := range s.OnlyA {
				if cmd := remove(s.Kind, it.Key); cmd != "" {
					body = append(body, fmt.Sprintf("# only on %s: %s", a, cmd))
				}
			}
		}
		if len(body) > 0 {
			lines = append(append(lines, "", "# "+comment(s.Title)), body...)
		}
	}
	if len(skippedLines) > 0 {
		lines = append(append(lines, ""), skippedLines...)
	}
	return strings.Join(lines, "\n") + "\n"
}

func remove(kind, key string) string {
	var argv []string
	switch kind {
	case "brew":
		for prefix, cmd := range map[string][]string{"formula › ": {"brew", "uninstall"}, "cask › ": {"brew", "uninstall", "--cask"}, "tap › ": {"brew", "untap"}} {
			if n, ok := strings.CutPrefix(key, prefix); ok && pkgRe.MatchString(n) {
				argv = append(cmd, n)
			}
		}
	case "packages":
		for prefix, cmd := range map[string][]string{"npm › ": {"npm", "uninstall", "-g"}, "pipx › ": {"pipx", "uninstall"}, "uv › ": {"uv", "tool", "uninstall"}, "cargo › ": {"cargo", "uninstall"}} {
			if n, ok := strings.CutPrefix(key, prefix); ok && pkgRe.MatchString(n) {
				argv = append(cmd, n)
			}
		}
	case "editors":
		if n, ok := strings.CutPrefix(key, "vscode › "); ok && pkgRe.MatchString(n) {
			argv = []string{"code", "--uninstall-extension", n}
		}
	}
	if argv == nil {
		return ""
	}
	return Action{Argv: argv}.Command()
}

// Installer returns a shell script that runs the runnable actions one by one
// on the machine named label, in a terminal: each step is announced, a
// missing tool or a failed step does not stop the rest, Ctrl-C stops after
// the current step, and a summary waits for Enter when wait is set.
func Installer(label string, actions []Action, wait bool) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "# hostdiff: changes on %s\n", comment(label))
	// Non-interactive ssh sessions start with a minimal PATH (/usr/bin
	// first). HOSTDIFF_SYSROOT marks hostdiff's own test sandbox, which must
	// only ever reach its stub tools.
	b.WriteString(`if [ -z "$HOSTDIFF_SYSROOT" ]; then PATH="$HOME/.local/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.cargo/bin:$HOME/go/bin:$PATH"; export PATH; fi
ok=0; fail=0; failed=""; stop=""
trap 'stop=1' INT
# Each step records its exit status, so hostdiff can say afterwards which
# commands worked instead of guessing from a later scan.
record() { [ -n "$HOSTDIFF_RESULTS" ] && printf '%s %s\n' "$1" "$2" >> "$HOSTDIFF_RESULTS"; return 0; }
step() {
	i=$1; n=$2; label=$3; shift 3
	[ -n "$stop" ] && return 0
	printf '\n\033[1m==> [%s/%s] %s\033[0m\n' "$i" "$n" "$label"
	if ! command -v "$1" >/dev/null 2>&1; then
		printf '\033[31m✗ %s is not installed on this machine\033[0m\n' "$1"
		fail=$((fail+1)); failed="$failed
  [$i] $label ($1 not installed)"
		record "$i" 127
		return 0
	fi
	if "$@"; then
		ok=$((ok+1)); printf '\033[32m✓ done\033[0m\n'; record "$i" 0
	else
		rc=$?; fail=$((fail+1)); printf '\033[31m✗ failed (exit %s)\033[0m\n' "$rc"
		failed="$failed
  [$i] $label"
		record "$i" "$rc"
	fi
}
`)
	var steps []Action
	for _, a := range actions {
		if a.Runnable() {
			steps = append(steps, a)
		}
	}
	n := len(steps)
	// Each step runs exactly the command line the confirmation showed:
	// after "step N LABEL" comes the same text, which Command quotes so the
	// shell reads it as the argument list and nothing more.
	for i, a := range steps {
		fmt.Fprintf(&b, "step %d %d %s %s\n", i+1, n, Quote(a.Command()), a.Command())
	}
	fmt.Fprintf(&b, "printf '\\n\\033[1mhostdiff: %%d of %d done, %%d failed\\033[0m' \"$ok\" \"$fail\"\n", n)
	b.WriteString(`[ -n "$stop" ] && printf ' (stopped with Ctrl-C)'
printf '\n'
[ -n "$failed" ] && printf 'Failed:%s\n' "$failed"
`)
	if wait {
		b.WriteString("printf 'Press Enter to return to hostdiff '; read _ || true\n")
	}
	b.WriteString(`[ "$fail" -eq 0 ] && [ -z "$stop" ]` + "\n")
	return b.String()
}
