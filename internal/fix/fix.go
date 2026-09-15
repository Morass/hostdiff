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
	pkgRe       = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9@._+/:-]{0,200}$`)
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
		if !pkgRe.MatchString(n) {
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
func ForRemove(kind string, it snapshot.Item) (a Action, ok bool) {
	defer func() { a.Verb = verb(kind, Remove) }()
	key := it.Key
	prefixed := func(prefix string, cmd ...string) (Action, bool) {
		n, ok := strings.CutPrefix(key, prefix)
		if !ok {
			return Action{}, false
		}
		if !pkgRe.MatchString(n) {
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
		if !pkgRe.MatchString(name) {
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
		var out []Action
		for _, it := range extra {
			if s.Kind == "brew" && it.Tag == "dependency" {
				continue // leaves with whatever needed it
			}
			if a, ok := ForRemove(s.Kind, it); ok {
				out = append(out, a)
			}
		}
		// Casks and formulae before the taps they came from.
		sort.SliceStable(out, func(i, j int) bool { return brewOrder(out[i].Key) > brewOrder(out[j].Key) })
		removals = append(removals, out...)
	}
	return append(append(installs, updates...), removals...)
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
		return valid(pkgRe, label, "-m", "pip", "uninstall", "--yes", name)
	case label == "gem":
		if tag == "default" {
			return note(kind, key, "ships with Ruby"), true
		}
		return valid(pkgRe, "gem", "uninstall", "--all", "--executables", name)
	case label == "perl":
		return valid(perlNameRe, "cpanm", "--uninstall", "--force", name)
	case label == "composer":
		return valid(pkgRe, "composer", "global", "remove", name)
	case label == "R":
		return valid(rNameRe, "Rscript", "-e", "remove.packages('"+name+"')")
	case strings.HasPrefix(label, "julia "):
		return valid(juliaNameRe, "julia", "-e", `using Pkg; Pkg.rm("`+name+`")`)
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
		if !pkgRe.MatchString(name) {
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
		return valid(pkgRe, "gem", "install", name)
	case label == "perl":
		return valid(perlNameRe, "cpanm", name)
	case label == "composer":
		return valid(pkgRe, "composer", "global", "require", name)
	case label == "R":
		return valid(rNameRe, "Rscript", "-e", "install.packages('"+name+"', repos = 'https://cloud.r-project.org')")
	case strings.HasPrefix(label, "julia "):
		return valid(juliaNameRe, "julia", "-e", `using Pkg; Pkg.add("`+name+`")`)
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
step() {
	n=$1; label=$2; shift 2
	[ -n "$stop" ] && return 0
	printf '\n\033[1m==> [%s] %s\033[0m\n' "$n" "$label"
	if ! command -v "$1" >/dev/null 2>&1; then
		printf '\033[31m✗ %s is not installed on this machine\033[0m\n' "$1"
		fail=$((fail+1)); failed="$failed
  [$n] $label ($1 not installed)"
		return 0
	fi
	if "$@"; then
		ok=$((ok+1)); printf '\033[32m✓ done\033[0m\n'
	else
		rc=$?; fail=$((fail+1)); printf '\033[31m✗ failed (exit %s)\033[0m\n' "$rc"
		failed="$failed
  [$n] $label"
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
		fmt.Fprintf(&b, "step %d/%d %s %s\n", i+1, n, Quote(a.Command()), a.Command())
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
