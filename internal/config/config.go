// Package config reads the machine list. The file lives in the user's config
// directory, never in a repository, and holds no secrets: how to log in
// (users, keys, ports, jump hosts) stays in ~/.ssh/config, and hostdiff only
// names an ssh destination.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Machine is one entry under [machines.NAME].
type Machine struct {
	Name string `toml:"-"`
	// SSH is an ssh destination: a Host alias from ~/.ssh/config or user@host.
	SSH string `toml:"ssh"`
	// Local marks the machine hostdiff runs on.
	Local bool `toml:"local"`
	// Command is where hostdiff lives on that machine. Empty: look on PATH
	// and in the usual install folders.
	Command string `toml:"command"`
	// Upload decides whether this binary may be sent over for one run when
	// hostdiff is not installed there: "auto" (default), "always", "never".
	Upload string `toml:"upload"`
}

// Config is the whole file.
type Config struct {
	Path     string              `toml:"-"`
	Found    bool                `toml:"-"`
	Machines map[string]*Machine `toml:"machines"`
	// Ignore hides items: "SECTION:GLOB" or "GLOB" (any section).
	Ignore []string `toml:"ignore"`
	// Skip lists sections that are never collected.
	Skip []string `toml:"skip"`
}

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sshRe     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@%+:\[\]-]{0,253}$`)
	commandRe = regexp.MustCompile(`^[A-Za-z0-9_./~+-]{1,256}$`)
)

// Path returns where the config file is looked for: $HOSTDIFF_CONFIG, then
// $XDG_CONFIG_HOME/hostdiff/config.toml, then ~/.config/hostdiff/config.toml.
func Path() string {
	if p := os.Getenv("HOSTDIFF_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "hostdiff", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hostdiff", "config.toml")
}

// Load reads and validates the config. A missing file is not an error: the
// result is an empty config with Found false.
func Load(path string) (*Config, error) {
	c := &Config{Path: path, Machines: map[string]*Machine{}}
	// Checked and decoded through one open file, so the file cannot be
	// swapped between the permission check and the read.
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	// The file decides which commands run on remote machines, so like
	// ~/.ssh/config it must not be writable by other users.
	if st.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by other users (mode %o); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	md, err := toml.NewDecoder(f).Decode(c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// A setting in the wrong place (skip = [...] below a [machines.x] table
	// belongs to that machine) would otherwise be silently ignored.
	if keys := md.Undecoded(); len(keys) > 0 {
		var names []string
		for _, k := range keys {
			names = append(names, k.String())
		}
		return nil, fmt.Errorf("%s: unknown setting %s (ignore and skip go above the first [machines.NAME] table)", path, strings.Join(names, ", "))
	}
	c.Found = true
	if c.Machines == nil {
		c.Machines = map[string]*Machine{}
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// AdHoc makes a machine that is not in the config file, from an ssh
// destination the user typed (in the machine picker, or on the command
// line as ssh:DESTINATION).
func AdHoc(dest string) (*Machine, error) {
	if !sshRe.MatchString(dest) {
		return nil, fmt.Errorf("%q is not a plain ssh destination (a Host alias from ~/.ssh/config, or user@host)", dest)
	}
	return &Machine{Name: dest, SSH: dest, Upload: "auto"}, nil
}

func (c *Config) validate() error {
	for name, m := range c.Machines {
		if m == nil {
			m = &Machine{}
			c.Machines[name] = m
		}
		m.Name = name
		if !nameRe.MatchString(name) {
			return fmt.Errorf("machine name %q: use letters, digits, dot, dash or underscore", name)
		}
		if name == "local" || name == "localhost" {
			return fmt.Errorf(`machine name %q is reserved for this machine`, name)
		}
		if m.Local == (m.SSH != "") {
			return fmt.Errorf("machine %q: set exactly one of ssh = \"DESTINATION\" or local = true", name)
		}
		if m.SSH != "" && !sshRe.MatchString(m.SSH) {
			return fmt.Errorf("machine %q: ssh = %q is not a plain destination (a Host alias or user@host; options belong in ~/.ssh/config)", name, m.SSH)
		}
		if m.Command != "" && !commandRe.MatchString(m.Command) {
			return fmt.Errorf("machine %q: command = %q must be a plain path", name, m.Command)
		}
		switch m.Upload {
		case "":
			m.Upload = "auto"
		case "auto", "always", "never":
		default:
			return fmt.Errorf("machine %q: upload must be auto, always or never", name)
		}
	}
	for _, p := range c.Ignore {
		if p == "" {
			return fmt.Errorf("ignore: empty pattern")
		}
	}
	return nil
}

// Names returns machine names in order.
func (c *Config) Names() []string {
	var out []string
	for n := range c.Machines {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Example is written by `hostdiff config init`.
const Example = `# hostdiff machines. This file holds no secrets: how to log in (user, key,
# port, jump host) belongs in ~/.ssh/config, and hostdiff just runs
# "ssh DESTINATION". Keep this file out of repositories.

# Items to hide from every diff: "SECTION:GLOB" or "GLOB".
# ignore = ["apps:Xcode*.app", "runtimes:docker"]

# Sections never to collect (see: hostdiff sections).
# skip = ["fonts"]

# Machines. Keep them below the settings above: in TOML every line after a
# machine's heading belongs to that machine.

# A machine reached over ssh. DESTINATION is a Host alias from ~/.ssh/config
# (recommended) or user@host.
# [machines.laptop]
# ssh = "laptop"
# command = "~/.local/bin/hostdiff"  # where hostdiff lives there (default: search)
# upload = "auto"                     # send this binary for one run if missing: auto, always, never

# The machine you are on. "localhost" always works too, this just gives it a name.
# [machines.desk]
# local = true
`

// WriteExample creates the config file with the example text. It refuses to
// overwrite an existing file.
func WriteExample(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists", path)
		}
		return err
	}
	if _, err := f.WriteString(Example); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// MatchIgnore reports whether an item is hidden by an ignore pattern.
func MatchIgnore(patterns []string, kind, key string) bool {
	for _, p := range patterns {
		sec, glob := "", p
		if i := strings.Index(p, ":"); i > 0 && nameRe.MatchString(p[:i]) {
			sec, glob = p[:i], p[i+1:]
		}
		if sec != "" && sec != kind {
			continue
		}
		if ok, _ := filepath.Match(glob, key); ok {
			return true
		}
	}
	return false
}
