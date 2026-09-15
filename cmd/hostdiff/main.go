// Command hostdiff compares two machines: what is installed, configured and
// bound to keys on one and not the other.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/morass/hostdiff/internal/collect"
	"github.com/morass/hostdiff/internal/config"
	"github.com/morass/hostdiff/internal/diff"
	"github.com/morass/hostdiff/internal/fix"
	"github.com/morass/hostdiff/internal/remote"
	"github.com/morass/hostdiff/internal/render"
	"github.com/morass/hostdiff/internal/snapshot"
	"github.com/morass/hostdiff/internal/tui"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "0.1.0-dev"

// errDifferent makes diff exit 1 without printing an error.
var errDifferent = errors.New("differences found")

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, errDifferent):
		os.Exit(1)
	case errors.Is(err, flag.ErrHelp):
	default:
		fmt.Fprintln(os.Stderr, "hostdiff:", snapshot.StripControls(err.Error()))
		os.Exit(2)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		overview(stdout)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		if len(rest) > 0 {
			if c := findCommand(rest[0]); c != nil {
				commandHelp(stdout, c)
				return nil
			}
			return fmt.Errorf("unknown command %q", rest[0])
		}
		overview(stdout)
		return nil
	case "-v", "--version", "version":
		fmt.Fprintln(stdout, "hostdiff", Version)
		return nil
	}
	c := findCommand(cmd)
	if c == nil {
		return fmt.Errorf("unknown command %q (see hostdiff --help)", cmd)
	}
	for _, a := range rest {
		if a == "-h" || a == "--help" {
			commandHelp(stdout, c)
			return nil
		}
	}
	switch cmd {
	case "diff":
		return cmdDiff(rest, stdout, stderr)
	case "snap":
		return cmdSnap(rest, stdout, stderr)
	case "machines":
		return cmdMachines(rest, stdout)
	case "config":
		return cmdConfig(rest, stdout)
	case "sections":
		sectionsList(stdout)
		return nil
	}
	return nil
}

// parse accepts flags before and after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, "see: hostdiff help %s\n", name) }
	return fs
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func checkKinds(kinds []string) error {
	valid := collect.Kinds()
	for _, k := range kinds {
		ok := false
		for _, v := range valid {
			ok = ok || v == k
		}
		if !ok {
			return fmt.Errorf("unknown section %q; sections: %s", k, strings.Join(valid, ", "))
		}
	}
	return nil
}

// target is one side of a comparison.
type target struct {
	label   string
	file    string
	machine *config.Machine // nil: this machine
	save    string          // label to save under
}

var savedRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,63})@(last|prev)$`)

func resolve(cfg *config.Config, arg string) (*target, error) {
	if m := savedRe.FindStringSubmatch(arg); m != nil {
		p, err := savedSnapshot(m[1], m[2])
		if err != nil {
			return nil, err
		}
		return &target{label: arg, file: p}, nil
	}
	if arg == "localhost" || arg == "local" || arg == "." {
		for _, n := range cfg.Names() {
			if cfg.Machines[n].Local {
				return &target{label: n, save: n}, nil
			}
		}
		return &target{label: "localhost", save: "localhost"}, nil
	}
	if m, ok := cfg.Machines[arg]; ok {
		t := &target{label: arg, save: arg}
		if !m.Local {
			t.machine = m
		}
		return t, nil
	}
	if strings.Contains(arg, "/") || strings.HasSuffix(arg, ".json") {
		if _, err := os.Stat(arg); err != nil {
			return nil, err
		}
		return &target{label: snapshot.StripControls(strings.TrimSuffix(filepath.Base(arg), ".json")), file: arg}, nil
	}
	hint := "run: hostdiff config init"
	if cfg.Found {
		hint = "machines in " + cfg.Path + ": " + strings.Join(cfg.Names(), ", ")
	}
	return nil, fmt.Errorf("%q is not a configured machine, \"localhost\", or a snapshot file (%s)", arg, hint)
}

func stateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "hostdiff", "snapshots")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "hostdiff", "snapshots")
}

func savedSnapshot(name, which string) (string, error) {
	dir := filepath.Join(stateDir(), name)
	entries, err := os.ReadDir(dir)
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	need := 1
	if which == "prev" {
		need = 2
	}
	if err != nil || len(files) < need {
		return "", fmt.Errorf("no %s snapshot saved for %s (use --save when collecting it)", which, name)
	}
	return filepath.Join(dir, files[len(files)-need]), nil
}

// writePrivate writes a file only the user can read, replacing any existing
// file or symlink instead of writing through it.
func writePrivate(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hostdiff-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func save(label string, s *snapshot.Snapshot) (string, error) {
	dir := filepath.Join(stateDir(), label)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, s.Created.UTC().Format("2006-01-02T15-04-05Z")+".json")
	return p, writePrivate(p, func(w io.Writer) error { return snapshot.Write(w, s) })
}

type collectOpts struct {
	only, skip []string
	progress   io.Writer
}

func (t *target) collect(cfg *config.Config, opt collectOpts) (*snapshot.Snapshot, error) {
	if t.file != "" {
		return snapshot.Load(t.file)
	}
	skip := append(append([]string{}, cfg.Skip...), opt.skip...)
	if t.machine == nil {
		if opt.progress != nil {
			fmt.Fprintf(opt.progress, "collecting %s…\n", t.label)
		}
		return collect.Snapshot(collect.Current(), collect.Options{Only: opt.only, Skip: skip, Tool: "hostdiff " + Version}), nil
	}
	if opt.progress != nil {
		fmt.Fprintf(opt.progress, "collecting %s over ssh…\n", t.label)
	}
	return remote.Snapshot(t.machine, remote.Options{Only: opt.only, Skip: skip})
}

// replaceSections returns old with every section of the same kind replaced
// by its fresh copy.
func replaceSections(old, fresh []snapshot.Section) []snapshot.Section {
	out := make([]snapshot.Section, 0, len(old)+len(fresh))
	used := map[string]bool{}
	for _, s := range old {
		for _, f := range fresh {
			if f.Kind == s.Kind {
				s, used[f.Kind] = f, true
			}
		}
		out = append(out, s)
	}
	for _, f := range fresh {
		if !used[f.Kind] {
			out = append(out, f)
		}
	}
	return out
}

// installSide says how to install on a target: live machines only.
func (t *target) installSide(tty bool) tui.Side {
	switch {
	case t.file != "":
		return tui.Side{Where: "snapshot file", NoInstall: "it is a snapshot file, not a live machine"}
	case t.machine != nil:
		m := t.machine
		return tui.Side{Where: "ssh " + m.SSH, Prepare: func(script string) (*exec.Cmd, func(), error) {
			cmd, err := remote.InstallCommand(m, script, tty)
			return cmd, func() {}, err
		}}
	}
	return tui.Side{Where: "this machine", Prepare: localInstaller}
}

// localInstaller writes the script to a private temporary folder and runs
// it with the PATH hostdiff collects with.
func localInstaller(script string) (*exec.Cmd, func(), error) {
	dir, err := os.MkdirTemp("", "hostdiff-install-")
	if err != nil {
		return nil, nil, err
	}
	p := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(p, []byte(script), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	cmd := exec.Command("/bin/sh", p)
	cmd.Env = append(os.Environ(), "PATH="+collect.Current().Path)
	return cmd, func() { os.RemoveAll(dir) }, nil
}

// installCLI prints what would be installed on one side, asks, runs it in
// this terminal and shows the affected sections collected again.
func installCLI(res *diff.Result, targets []*target, side int, yes bool, refresh func(int, []string) (*diff.Result, error), stdout, stderr io.Writer, color bool) error {
	t, other := targets[side], targets[1-side]
	tty := isTerminal(os.Stdin)
	how := t.installSide(tty)
	if how.NoInstall != "" {
		return fmt.Errorf("cannot install on %s: %s", t.label, how.NoInstall)
	}
	var acts []fix.Action
	notes := 0
	for _, a := range fix.Plan(res, side == 0) {
		if a.Runnable() {
			acts = append(acts, a)
		} else {
			notes++
		}
	}
	if len(acts) == 0 {
		fmt.Fprintf(stdout, "Nothing hostdiff can install on %s from %s.\n", t.label, other.label)
		return nil
	}
	fmt.Fprintf(stdout, "On %s (%s), to bring over what %s has:\n", t.label, how.Where, other.label)
	for _, a := range acts {
		fmt.Fprintln(stdout, "  "+a.Command())
	}
	if notes > 0 {
		fmt.Fprintf(stdout, "(%d more differences have no install command; --script lists them)\n", notes)
	}
	if !yes {
		if !tty {
			return errors.New("add --yes to run these without a terminal to confirm in")
		}
		fmt.Fprintf(stdout, "Run these %d commands on %s? [y/N] ", len(acts), t.label)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Nothing was run.")
			return nil
		}
	}
	cmd, cleanup, err := how.Prepare(fix.Installer(t.label, acts, false))
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	runErr := cmd.Run()

	kinds := map[string]bool{}
	var list []string
	for _, a := range acts {
		if !kinds[a.Kind] {
			kinds[a.Kind] = true
			list = append(list, a.Kind)
		}
	}
	fresh, err := refresh(side, list)
	if err != nil {
		return fmt.Errorf("collecting %s again: %w", strings.Join(list, ", "), err)
	}
	shown := *fresh
	shown.Sections = nil
	for _, s := range fresh.Sections {
		if kinds[s.Kind] {
			shown.Sections = append(shown.Sections, s)
		}
	}
	fmt.Fprintf(stdout, "\nCollected again on %s:\n\n", t.label)
	render.Text(stdout, &shown, render.Options{Color: color})
	if runErr != nil {
		return errors.New("some install steps failed or were stopped (see above)")
	}
	return nil
}

// sameDevice reports, before anything is collected, when both sides would be
// read live from the same account on the same machine: this machine twice,
// or an ssh destination that leads back here (or to the other side's host).
// It only uses what ssh -G and name resolution say locally; when in doubt
// the sides count as different.
func sameDevice(a, b *target) (string, bool) {
	ids := func(t *target) (map[string]bool, string) {
		if t.file != "" {
			return nil, ""
		}
		me := currentUser()
		if t.machine == nil {
			return map[string]bool{me + "@this machine": true}, "this machine"
		}
		ep, err := remote.Resolve(t.machine, remote.Options{})
		if err != nil {
			return nil, ""
		}
		user := ep.User
		if user == "" {
			user = me
		}
		addrs, here := ep.Addresses()
		if here {
			return map[string]bool{user + "@this machine": true}, "ssh " + t.machine.SSH + " leads back to this machine"
		}
		set := map[string]bool{}
		for _, ip := range addrs {
			set[user+"@"+ip+":"+ep.Port] = true
		}
		return set, "ssh " + t.machine.SSH + " leads to " + ep.Host
	}
	ia, wa := ids(a)
	if ia == nil {
		return "", false
	}
	ib, wb := ids(b)
	for k := range ia {
		if ib[k] {
			switch {
			case a.machine == nil && b.machine == nil:
				return "both are this machine", true
			case a.machine == nil:
				return wb, true
			case b.machine == nil:
				return wa, true
			}
			return wa + ", as does " + b.machine.SSH, true
		}
	}
	return "", false
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

func cmdDiff(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("diff", stderr)
	only := fs.String("only", "", "")
	skip := fs.String("skip", "", "")
	all := fs.Bool("all", false, "")
	details := fs.Bool("details", false, "")
	format := fs.String("format", "text", "")
	script := fs.Bool("script", false, "")
	doSave := fs.Bool("save", false, "")
	noColor := fs.Bool("no-color", false, "")
	install := fs.String("install", "", "")
	yes := fs.Bool("yes", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *install != "" && *script {
		return errors.New("use --script or --install, not both")
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("usage: hostdiff diff A [B]")
	}
	if len(pos) == 1 {
		pos = []string{"localhost", pos[0]}
	}
	onlyK, skipK := splitList(*only), splitList(*skip)
	if err := checkKinds(append(append([]string{}, onlyK...), skipK...)); err != nil {
		return err
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	targets := make([]*target, 2)
	for i, p := range pos {
		if targets[i], err = resolve(cfg, p); err != nil {
			return err
		}
	}
	if why, same := sameDevice(targets[0], targets[1]); same {
		return fmt.Errorf("%s and %s are the same machine (%s); nothing to compare live. To see what changes over time, run hostdiff snap %s --save now and hostdiff diff %s@last later", pos[0], pos[1], why, targets[1].save, targets[1].save)
	}
	if targets[0].label == targets[1].label {
		targets[1].label += " (2)"
	}
	var progress io.Writer
	if isTerminal(stderr) {
		progress = stderr
	}
	snaps := make([]*snapshot.Snapshot, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps[i], errs[i] = t.collect(cfg, collectOpts{only: onlyK, skip: skipK, progress: progress})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if *doSave {
		for i, t := range targets {
			if t.save != "" {
				p, err := save(t.save, snaps[i])
				if err != nil {
					return err
				}
				fmt.Fprintf(stderr, "saved %s\n", p)
			}
		}
	}
	compare := func() *diff.Result {
		return diff.Compare(diff.Side{Label: targets[0].label, Snap: snaps[0]}, diff.Side{Label: targets[1].label, Snap: snaps[1]}, diff.Options{Only: onlyK, Ignore: cfg.Ignore})
	}
	res := compare()
	// refresh collects some sections of one side again, after an install.
	refresh := func(side int, kinds []string) (*diff.Result, error) {
		fresh, err := targets[side].collect(cfg, collectOpts{only: kinds, skip: skipK})
		if err != nil {
			return nil, err
		}
		merged := *snaps[side]
		merged.Sections = replaceSections(snaps[side].Sections, fresh.Sections)
		snaps[side] = &merged
		return compare(), nil
	}
	if *install != "" {
		side := -1
		for i, t := range targets {
			if *install == pos[i] || *install == t.label || (*install == "localhost" && t.machine == nil && t.file == "") {
				side = i
				break
			}
		}
		if side < 0 {
			return fmt.Errorf("--install %s: name one of the two sides (%s or %s)", *install, targets[0].label, targets[1].label)
		}
		return installCLI(res, targets, side, *yes, refresh, stdout, stderr, !*noColor && isTerminal(stdout) && os.Getenv("NO_COLOR") == "")
	}
	formatSet := false
	fs.Visit(func(f *flag.Flag) {
		formatSet = formatSet || f.Name == "format" || f.Name == "all" || f.Name == "details"
	})
	switch {
	case *script:
		fmt.Fprint(stdout, fix.Script(res))
	case !formatSet && isTerminal(stdout) && isTerminal(os.Stdin) && os.Getenv("HOSTDIFF_NO_TUI") == "":
		return tui.Run(res, tui.Options{
			Sides:   [2]tui.Side{targets[0].installSide(true), targets[1].installSide(true)},
			Refresh: refresh,
		})
	case *format == "json":
		if err := render.JSON(stdout, res); err != nil {
			return err
		}
	case *format == "markdown" || *format == "md":
		render.Markdown(stdout, res, render.Options{All: *all, Details: *details})
	case *format == "text":
		color := isTerminal(stdout) && !*noColor && os.Getenv("NO_COLOR") == ""
		render.Text(stdout, res, render.Options{Color: color, All: *all, Details: *details})
	default:
		return fmt.Errorf("unknown format %q (text, markdown, json)", *format)
	}
	if res.Differences() > 0 {
		return errDifferent
	}
	comparable := 0
	for _, s := range res.Sections {
		if s.Comparable {
			comparable++
		}
	}
	if comparable == 0 && len(res.Sections) > 0 {
		// "No differences" must not pass a check when nothing was compared.
		return errors.New("no section could be compared on both sides")
	}
	return nil
}

func cmdSnap(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("snap", stderr)
	out := fs.String("o", "", "")
	asJSON := fs.Bool("json", false, "")
	doSave := fs.Bool("save", false, "")
	only := fs.String("only", "", "")
	skip := fs.String("skip", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("usage: hostdiff snap [NAME]")
	}
	onlyK, skipK := splitList(*only), splitList(*skip)
	if err := checkKinds(append(append([]string{}, onlyK...), skipK...)); err != nil {
		return err
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		// A broken config must not stop a remote `hostdiff snap --json`,
		// which never needs it.
		if len(pos) > 0 {
			return err
		}
		cfg = &config.Config{Machines: map[string]*config.Machine{}}
	}
	name := "localhost"
	if len(pos) == 1 {
		name = pos[0]
	}
	t, err := resolve(cfg, name)
	if err != nil {
		return err
	}
	if t.file != "" {
		return fmt.Errorf("%s is already a snapshot", name)
	}
	var progress io.Writer
	if isTerminal(stderr) && !*asJSON {
		progress = stderr
	}
	start := time.Now()
	s, err := t.collect(cfg, collectOpts{only: onlyK, skip: skipK, progress: progress})
	if err != nil {
		return err
	}
	if *doSave {
		p, err := save(t.save, s)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "saved %s\n", p)
	}
	switch {
	case *out != "":
		if err := writePrivate(*out, func(w io.Writer) error { return snapshot.Write(w, s) }); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "wrote %s (%d sections, %s)\n", *out, len(s.Sections), time.Since(start).Round(100*time.Millisecond))
	case *asJSON || !isTerminal(stdout):
		return snapshot.Write(stdout, s)
	default:
		render.Summary(stdout, s, t.label)
		fmt.Fprintf(stdout, "\nSave it with -o FILE, or compare: hostdiff diff NAME\n")
	}
	return nil
}

func cmdMachines(args []string, stdout io.Writer) error {
	fs := newFlags("machines", io.Discard)
	check := fs.Bool("check", false, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	if !cfg.Found {
		fmt.Fprintf(stdout, "No config file at %s.\nCreate one: hostdiff config init\n", cfg.Path)
		return nil
	}
	if len(cfg.Machines) == 0 {
		fmt.Fprintf(stdout, "No machines in %s yet.\n", cfg.Path)
		return nil
	}
	for _, n := range cfg.Names() {
		m := cfg.Machines[n]
		how := "this machine"
		if !m.Local {
			how = "ssh " + m.SSH
			if m.Command != "" {
				how += ", command " + m.Command
			}
			how += ", upload " + m.Upload
		}
		fmt.Fprintf(stdout, "  %-16s %s\n", n, how)
		if *check && !m.Local {
			_, err := remote.Snapshot(&config.Machine{Name: m.Name, SSH: m.SSH, Command: m.Command, Upload: m.Upload}, remote.Options{Only: []string{"system"}, Timeout: 60 * time.Second})
			if err != nil {
				fmt.Fprintf(stdout, "  %-16s ✗ %v\n", "", err)
			} else {
				fmt.Fprintf(stdout, "  %-16s ✓ reachable, snapshot works\n", "")
			}
		}
	}
	return nil
}

func cmdConfig(args []string, stdout io.Writer) error {
	p := config.Path()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "path":
		fmt.Fprintln(stdout, p)
	case "init":
		if err := config.WriteExample(p); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created %s (mode 600). Add your machines there.\n", p)
	case "":
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stdout, "No config file at %s.\nCreate one: hostdiff config init\n", p)
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := config.Load(p); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "# %s\n%s", p, b)
	default:
		return fmt.Errorf("unknown config command %q (init, path)", sub)
	}
	return nil
}
