package collect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/morass/hostdiff/internal/snapshot"
)

// pythonProbe lists every distribution an interpreter can import, marking
// the ones in the user site. It needs no pip, works for Python 3.8+, and
// reads nothing but package metadata. Its first line drops the current
// folder from the import path, so a json.py lying in the home folder is
// never imported (and run).
const pythonProbe = `import sys
sys.path[:] = [p for p in sys.path if p not in ("", ".")]
import json, site
v = "%d.%d" % sys.version_info[:2]
try:
    import importlib.metadata as md
except ImportError:
    print(json.dumps({"v": v, "pkgs": []})); sys.exit()
user = site.getusersitepackages() if hasattr(site, "getusersitepackages") else ""
seen = {}
for d in md.distributions():
    name = d.metadata["Name"]
    if not name:
        continue
    loc = str(d.locate_file(""))
    seen.setdefault(name.lower(), [name, d.version, "user" if user and loc.startswith(user) else "system"])
print(json.dumps({"v": v, "pkgs": sorted(seen.values())}))`

// pythons returns the interpreters to inspect: python3 on PATH and the
// side-by-side versioned ones package managers install, without duplicates.
func (e *Env) pythons() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || strings.HasSuffix(p, "-config") || len(out) >= 6 {
			return
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			real = p
		}
		if seen[real] || e.stubWouldPrompt("python3", p) {
			return
		}
		seen[real] = true
		out = append(out, p)
	}
	add(e.Look("python3"))
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"} {
		matches, _ := filepath.Glob(filepath.Join(e.Sys(dir), "python3.*"))
		sort.Strings(matches)
		for _, m := range matches {
			if isExec(m) {
				add(m)
			}
		}
	}
	return out
}

var pyVersionRe = regexp.MustCompile(`^\d+\.\d+$`)

func libraries(e *Env, s *snapshot.Section) {
	for _, py := range e.pythons() {
		out, err := e.Out(py, "-E", "-c", pythonProbe)
		if err != nil {
			continue
		}
		var res struct {
			V    string     `json:"v"`
			Pkgs [][]string `json:"pkgs"`
		}
		if json.Unmarshal([]byte(out), &res) != nil || !pyVersionRe.MatchString(res.V) {
			continue
		}
		for _, p := range res.Pkgs {
			if len(p) == 3 {
				s.AddTag("python"+res.V+" › "+p[0], p[1], p[2])
			}
		}
	}

	if out, err := e.Out("gem", "list", "--local", "--no-details"); err == nil {
		re := regexp.MustCompile(`^(\S+) \((.*)\)$`)
		for _, l := range Lines(out) {
			m := re.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			tag := ""
			if strings.HasPrefix(m[2], "default: ") && !strings.Contains(m[2], ",") {
				tag = "default"
			}
			s.AddTag("gem › "+m[1], strings.ReplaceAll(m[2], "default: ", ""), tag)
		}
	}

	if e.Look("perl") != "" {
		script := `use ExtUtils::Installed; my $i = ExtUtils::Installed->new(skip_cwd => 1); for my $m ($i->modules) { next if $m eq "Perl"; my $v = eval { $i->version($m) }; print "$m\t", (defined $v ? $v : "?"), "\n" }`
		if out, err := e.Out("perl", "-e", script); err == nil {
			for _, l := range Lines(out) {
				if name, v, ok := strings.Cut(l, "\t"); ok {
					s.Add("perl › "+name, v, "")
				}
			}
		}
	}

	if out, err := e.Out("composer", "global", "show", "--format=json", "--no-interaction"); err == nil {
		var res struct {
			Installed []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"installed"`
		}
		if json.Unmarshal([]byte(out), &res) == nil {
			for _, p := range res.Installed {
				s.Add("composer › "+p.Name, orDash(p.Version), "")
			}
		}
	}

	if out, err := e.Out("Rscript", "--no-init-file", "-e", `ip <- installed.packages(priority = "NA"); cat(paste(ip[, "Package"], ip[, "Version"], sep = "\t"), sep = "\n")`); err == nil {
		for _, l := range Lines(out) {
			if name, v, ok := strings.Cut(l, "\t"); ok {
				s.Add("R › "+name, v, "")
			}
		}
	}

	envs, _ := filepath.Glob(e.HomePath(".julia", "environments", "*", "Project.toml"))
	if e.pathProtected(e.HomePath(".julia")) {
		envs = nil
	}
	for _, p := range envs {
		b, ok := ReadFile(p, 1<<20)
		if !ok {
			continue
		}
		env := filepath.Base(filepath.Dir(p))
		inDeps := false
		for _, l := range Lines(string(b)) {
			if strings.HasPrefix(l, "[") {
				inDeps = l == "[deps]"
				continue
			}
			if name, _, ok := strings.Cut(l, "="); ok && inDeps {
				s.Add("julia "+env+" › "+strings.TrimSpace(name), "added", "")
			}
		}
	}

	if out, err := e.Out("luarocks", "list", "--porcelain"); err == nil {
		for _, l := range Lines(out) {
			if f := strings.Split(l, "\t"); len(f) >= 2 {
				s.Add("luarocks › "+f[0], f[1], "")
			}
		}
	}

	if out, err := e.Out("dart", "pub", "global", "list"); err == nil {
		for _, l := range Lines(out) {
			if f := strings.Fields(l); len(f) >= 2 {
				s.Add("dart › "+f[0], f[1], "")
			}
		}
	}
}

func toolchains(e *Env, s *snapshot.Section) {
	lines := func(label string, name string, args ...string) {
		out, err := e.Out(name, args...)
		if err != nil {
			return
		}
		for _, l := range Lines(out) {
			s.Add(label+" › "+l, "installed", "")
		}
	}
	dirs := func(label string, glob string) {
		matches, _ := filepath.Glob(glob)
		for _, m := range matches {
			base := filepath.Base(m)
			if st, err := os.Stat(m); err == nil && st.IsDir() && base != "current" && !strings.HasPrefix(base, ".") {
				s.Add(label+" › "+base, "installed", "")
			}
		}
	}

	lines("pyenv", "pyenv", "versions", "--bare")
	lines("rbenv", "rbenv", "versions", "--bare")
	dirs("nvm", e.HomePath(".nvm", "versions", "node", "*"))
	dirs("fnm", e.HomePath(".local", "share", "fnm", "node-versions", "*"))
	dirs("volta node", e.HomePath(".volta", "tools", "image", "node", "*"))
	dirs("rvm", e.HomePath(".rvm", "rubies", "*"))
	dirs("go sdk", e.HomePath("sdk", "go*"))

	if out, err := e.Out("asdf", "list"); err == nil {
		plugin := ""
		for _, raw := range strings.Split(out, "\n") {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			if !strings.HasPrefix(raw, " ") {
				plugin = strings.TrimSpace(raw)
				continue
			}
			v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "*"))
			if plugin != "" && v != "" && !strings.HasPrefix(v, "No versions") {
				s.Add("asdf › "+plugin+" "+v, "installed", "")
			}
		}
	}
	if out, err := e.Out("mise", "ls", "--json"); err == nil {
		var tools map[string][]struct {
			Version string `json:"version"`
			Active  bool   `json:"active"`
		}
		if json.Unmarshal([]byte(out), &tools) == nil {
			for tool, versions := range tools {
				for _, v := range versions {
					s.Add("mise › "+tool+" "+v.Version, yesNo(v.Active, "active", "installed"), "")
				}
			}
		}
	}

	if out, err := e.Out("rustup", "toolchain", "list"); err == nil {
		for _, l := range Lines(out) {
			name, state := l, "installed"
			if i := strings.Index(l, " ("); i > 0 {
				name, state = l[:i], strings.Trim(l[i+1:], "()")
			}
			s.Add("rustup › "+name, state, "")
		}
		if out, err := e.Out("rustup", "component", "list", "--installed"); err == nil {
			for _, l := range Lines(out) {
				s.Add("rustup component › "+l, "installed", "")
			}
		}
	}

	if e.OS == "darwin" {
		dirs("jdk", e.Sys("/Library/Java/JavaVirtualMachines/*"))
	} else {
		dirs("jdk", e.Sys("/usr/lib/jvm/*"))
	}
	candidates, _ := filepath.Glob(e.HomePath(".sdkman", "candidates", "*"))
	for _, c := range candidates {
		dirs("sdkman "+filepath.Base(c), filepath.Join(c, "*"))
	}

	for _, kind := range []string{"sdks", "runtimes"} {
		if out, err := e.Out("dotnet", "--list-"+kind); err == nil {
			for _, l := range Lines(out) {
				if i := strings.Index(l, " ["); i > 0 {
					l = l[:i]
				}
				s.Add("dotnet "+strings.TrimSuffix(kind, "s")+" › "+l, "installed", "")
			}
		}
	}

	if out, err := e.Out("conda", "env", "list"); err == nil {
		for _, l := range Lines(out) {
			if f := strings.Fields(l); len(f) >= 1 && !strings.HasPrefix(l, "#") && !strings.HasPrefix(f[0], "/") {
				s.Add("conda env › "+f[0], "present", "")
			}
		}
	}
	if out, err := e.Out("uv", "python", "list", "--only-installed"); err == nil {
		for _, l := range Lines(out) {
			if f := strings.Fields(l); len(f) >= 1 {
				s.Add("uv python › "+f[0], "installed", "")
			}
		}
	}
}

// morePackages adds global packages of the less common Node and .NET tools.
func morePackages(e *Env, s *snapshot.Section) {
	if out, err := e.Out("pnpm", "ls", "-g", "--json"); err == nil {
		var roots []struct {
			Dependencies map[string]struct {
				Version string `json:"version"`
			} `json:"dependencies"`
		}
		if json.Unmarshal([]byte(out), &roots) == nil {
			for _, r := range roots {
				for name, d := range r.Dependencies {
					s.Add("pnpm › "+name, orDash(d.Version), "")
				}
			}
		}
	}
	if out, err := e.Out("yarn", "global", "list", "--depth=0"); err == nil {
		re := regexp.MustCompile(`^info "(.+)@([^@"]+)" has binaries`)
		for _, l := range Lines(out) {
			if m := re.FindStringSubmatch(l); m != nil {
				s.Add("yarn › "+m[1], m[2], "")
			}
		}
	}
	if out, err := e.Out("dotnet", "tool", "list", "-g"); err == nil {
		for i, l := range Lines(out) {
			if f := strings.Fields(l); i >= 2 && len(f) >= 2 {
				s.Add("dotnet › "+f[0], f[1], "")
			}
		}
	}
	if entries, err := os.ReadDir(e.HomePath(".deno", "bin")); err == nil {
		for _, en := range entries {
			if n := en.Name(); n != "deno" && !strings.HasPrefix(n, ".") {
				s.Add("deno › "+n, "installed", "")
			}
		}
	}
}
