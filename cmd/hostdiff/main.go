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
	// Shared ssh connections are closed before leaving, whatever happened.
	remote.CloseControl()
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
		if isTerminal(stdout) && isTerminal(os.Stdin) && os.Getenv("HOSTDIFF_NO_TUI") == "" {
			return cmdStart(stdout)
		}
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

var (
	savedRe     = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,63})@(last|prev)$`)
	savedNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

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
	if dest, ok := strings.CutPrefix(arg, "ssh:"); ok {
		m, err := config.AdHoc(dest)
		if err != nil {
			return nil, err
		}
		return adHocTarget(dest, m), nil
	}
	if strings.Contains(arg, "/") || strings.HasSuffix(arg, ".json") {
		if _, err := os.Stat(arg); err != nil {
			return nil, err
		}
		return &target{label: snapshot.StripControls(strings.TrimSuffix(filepath.Base(arg), ".json")), file: arg}, nil
	}
	// A destination that is not in the config: ssh:DEST always, and a name
	// that can only be a host (user@host, host.domain, an address).
	if strings.ContainsAny(arg, "@.:") {
		m, err := config.AdHoc(arg)
		if err != nil {
			return nil, err
		}
		return adHocTarget(arg, m), nil
	}
	hint := "run: hostdiff config init"
	if cfg.Found {
		hint = "machines in " + cfg.Path + ": " + strings.Join(cfg.Names(), ", ")
	}
	return nil, fmt.Errorf("%q is not a configured machine, \"localhost\", a snapshot file, or ssh:DESTINATION (%s)", arg, hint)
}

// adHocTarget names a machine that is not in the config after its host.
func adHocTarget(dest string, m *config.Machine) *target {
	label := dest
	if _, host, ok := strings.Cut(dest, "@"); ok && host != "" {
		label = host
	}
	save := ""
	if savedNameRe.MatchString(label) {
		save = label
	}
	return &target{label: label, save: save, machine: m}
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
	// onProgress receives structured progress (the interactive view, and a
	// remote `snap --json` reporting back).
	onProgress func(snapshot.Progress)
}

func (t *target) collect(cfg *config.Config, opt collectOpts) (*snapshot.Snapshot, error) {
	if t.file != "" {
		s, err := snapshot.Load(t.file)
		if err == nil && opt.onProgress != nil {
			for _, sec := range s.Sections {
				opt.onProgress(snapshot.Progress{Stage: "done", Kind: sec.Kind, Items: len(sec.Items), Status: sec.Status})
			}
		}
		return s, err
	}
	skip := append(append([]string{}, cfg.Skip...), opt.skip...)
	if t.machine == nil {
		if opt.progress != nil {
			fmt.Fprintf(opt.progress, "collecting %s…\n", t.label)
		}
		return collect.Snapshot(collect.Current(), collect.Options{Only: opt.only, Skip: skip, Tool: "hostdiff " + Version, Progress: opt.onProgress}), nil
	}
	if opt.progress != nil {
		fmt.Fprintf(opt.progress, "collecting %s over ssh…\n", t.label)
	}
	return remote.Snapshot(t.machine, remote.Options{Only: opt.only, Skip: skip, Progress: opt.onProgress})
}

// cmdStart is `hostdiff` alone in a terminal: choose the other machine and
// what to compare, then the interactive view.
func cmdStart(stdout io.Writer) error {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	s := newSession(cfg, nil)
	if len(s.Others()) == 0 {
		overview(stdout)
		fmt.Fprintf(stdout, "\nNo other machines are configured yet. Create the config with\n  hostdiff config init\nthen add a machine to %s and run hostdiff again.\n", cfg.Path)
		return nil
	}
	return tui.Run(s, tui.Start{})
}

// session is the interactive mode's view of the machines: it resolves the
// two sides, collects sections as they are asked for and compares.
type session struct {
	cfg     *config.Config
	skip    []string
	mu      sync.Mutex
	targets [2]*target
	snaps   [2]*snapshot.Snapshot
}

func newSession(cfg *config.Config, skip []string) *session {
	return &session{cfg: cfg, skip: skip}
}

func (s *session) Here() tui.Machine {
	name := "localhost"
	if t, err := resolve(s.cfg, "localhost"); err == nil {
		name = t.label
	}
	return tui.Machine{Name: name, Where: "this machine"}
}

func (s *session) Others() []tui.Machine {
	var out []tui.Machine
	for _, n := range s.cfg.Names() {
		if m := s.cfg.Machines[n]; !m.Local {
			out = append(out, tui.Machine{Name: n, Where: "ssh " + m.SSH})
		}
	}
	return out
}

func (s *session) Groups() []tui.Group {
	var out []tui.Group
	for _, c := range collect.All() {
		out = append(out, tui.Group{Kind: c.Kind, Title: c.Title, Reads: c.Reads, OS: c.OS})
	}
	return out
}

func (s *session) Select(a, b string) error {
	ta, err := resolve(s.cfg, a)
	if err != nil {
		return err
	}
	tb, err := resolve(s.cfg, b)
	if err != nil {
		return err
	}
	if why, same := sameDevice(ta, tb); same {
		return fmt.Errorf("%s and %s are the same machine (%s)", a, b, why)
	}
	if ta.label == tb.label {
		tb.label += " (2)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.targets[0] == nil || s.targets[0].label != ta.label || s.targets[1].label != tb.label {
		s.snaps = [2]*snapshot.Snapshot{{Format: snapshot.Format}, {Format: snapshot.Format}}
	}
	s.targets = [2]*target{ta, tb}
	return nil
}

func (s *session) Labels() [2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return [2]string{s.targets[0].label, s.targets[1].label}
}

func (s *session) Collect(side int, kinds []string, progress func(snapshot.Progress)) error {
	s.mu.Lock()
	t := s.targets[side]
	s.mu.Unlock()
	fresh, err := t.collect(s.cfg, collectOpts{only: kinds, skip: s.skip, onProgress: progress})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := *fresh
	merged.Sections = replaceSections(s.snaps[side].Sections, fresh.Sections)
	s.snaps[side] = &merged
	return nil
}

func (s *session) Result(kinds []string) *diff.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return diff.Compare(diff.Side{Label: s.targets[0].label, Snap: s.snaps[0]}, diff.Side{Label: s.targets[1].label, Snap: s.snaps[1]}, diff.Options{Only: kinds, Ignore: s.cfg.Ignore})
}

func (s *session) Reach(side int) (tui.Reach, string) {
	s.mu.Lock()
	t := s.targets[side]
	s.mu.Unlock()
	if t == nil || t.machine == nil {
		return tui.Reachable, ""
	}
	reach, msg := remote.Check(t.machine, remote.Options{})
	return map[remote.Reach]tui.Reach{
		remote.Reachable:    tui.Reachable,
		remote.NeedsHostKey: tui.NeedsHostKey,
		remote.NeedsAuth:    tui.NeedsAuth,
		remote.Unreachable:  tui.Unreachable,
	}[reach], msg
}

func (s *session) Connect(side int) (*tui.Started, error) {
	s.mu.Lock()
	t := s.targets[side]
	s.mu.Unlock()
	if t == nil || t.machine == nil {
		return nil, errors.New("this side is not reached over ssh")
	}
	return &tui.Started{Cmd: remote.ConnectCommand(t.machine, remote.Options{}), Cleanup: func() {}}, nil
}

func (s *session) Side(side int) tui.Side {
	s.mu.Lock()
	t := s.targets[side]
	s.mu.Unlock()
	return t.installSide(true)
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
		return tui.Side{Where: "ssh " + m.SSH, Remote: true, Dest: m.SSH, Prepare: func(script string) (*tui.Started, error) {
			cmd, results, err := remote.InstallCommand(m, script, tty)
			if err != nil {
				return nil, err
			}
			return &tui.Started{Cmd: cmd, Results: results, Cleanup: func() {}}, nil
		}}
	}
	return tui.Side{Where: "this machine", Prepare: localInstaller}
}

// localInstaller writes the script to a private temporary folder and runs
// it with the PATH hostdiff collects with.
func localInstaller(script string) (*tui.Started, error) {
	dir, err := os.MkdirTemp("", "hostdiff-install-")
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(p, []byte(script), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	results := filepath.Join(dir, "results")
	cmd := exec.Command("/bin/sh", p)
	cmd.Env = append(os.Environ(), "PATH="+collect.Current().Path, "HOSTDIFF_RESULTS="+results)
	return &tui.Started{
		Cmd: cmd,
		Results: func() (map[int]int, error) {
			b, err := os.ReadFile(results)
			return remote.ParseResults(string(b)), err
		},
		Cleanup: func() { os.RemoveAll(dir) },
	}, nil
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
	fmt.Fprintf(stdout, "To bring over what %s has, hostdiff will run these %d commands on %s (%s), in this order, exactly as written:\n\n", other.label, len(acts), t.label, how.Where)
	for i, a := range acts {
		fmt.Fprintf(stdout, "  %3d  %s\n", i+1, a.Command())
	}
	fmt.Fprintln(stdout, "\nEach command is printed with its number before it runs; a failure does not stop the rest.")
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
	started, err := how.Prepare(fix.Installer(t.label, acts, false))
	if err != nil {
		return err
	}
	defer started.Cleanup()
	cmd := started.Cmd
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
	interactive := true
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "format", "all", "details", "script", "install", "save":
			interactive = false
		}
	})
	if interactive && isTerminal(stdout) && isTerminal(os.Stdin) && os.Getenv("HOSTDIFF_NO_TUI") == "" {
		return tui.Run(newSession(cfg, skipK), tui.Start{A: pos[0], B: pos[1], Kinds: onlyK})
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
	switch {
	case *script:
		fmt.Fprint(stdout, fix.Script(res))
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
	var onProgress func(snapshot.Progress)
	if *asJSON && os.Getenv("HOSTDIFF_PROGRESS") != "" && t.machine == nil {
		// Read by the machine that asked for this snapshot over ssh.
		var mu sync.Mutex
		onProgress = func(p snapshot.Progress) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(stderr, "%s %s %s %d %s\n", remote.ProgressMarker, p.Stage, p.Kind, p.Items, orNone(string(p.Status)))
		}
	}
	start := time.Now()
	s, err := t.collect(cfg, collectOpts{only: onlyK, skip: skipK, progress: progress, onProgress: onProgress})
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

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
