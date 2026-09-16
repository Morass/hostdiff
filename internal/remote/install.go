package remote

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/morass/hostdiff/internal/config"
)

var tmpPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/+-]{1,400}$`)

// Install is a prepared run on another machine.
type Install struct {
	// Cmd runs the script there in this terminal.
	Cmd *exec.Cmd
	// Results reads back the exit status of each step.
	Results func() (map[int]int, error)
	// Cleanup removes the script and its results from the other machine;
	// call it once, whatever happened.
	Cleanup func()
}

// InstallCommand copies an installer script to m and returns the ssh command
// that runs it there. With tty set the session gets a terminal, so password
// prompts (sudo, a cask's installer) work and Ctrl-C reaches the steps. The
// script and its results file are created inside one private temporary
// folder (mktemp -d, mode 700) over a plain connection first: nobody else
// can create or replace them, and the command line that runs the script
// only carries a path hostdiff has checked.
func InstallCommand(m *config.Machine, script string, tty bool) (*Install, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opt := Options{}
	out, errOut, code, err := run(ctx, opt, m.SSH, `umask 077; d=$(mktemp -d "${TMPDIR:-/tmp}/hostdiff-install.XXXXXX") && cat > "$d/install.sh" && : > "$d/results" && echo "$d"`, []byte(script))
	if err != nil {
		return nil, fmt.Errorf("%s: ssh: %w", m.Name, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("%s: %s", m.Name, failure(code, errOut))
	}
	dir := strings.TrimSpace(string(out))
	if !tmpPathRe.MatchString(dir) {
		return nil, fmt.Errorf("%s: unexpected temporary folder name from the remote side", m.Name)
	}
	remoteRun := func(script string) ([]byte, []byte, int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return run(ctx, opt, m.SSH, script, nil)
	}
	flag := "-T"
	if tty {
		flag = "-t"
	}
	wrapper := `trap "exit 130" INT; trap "exit 129" HUP; HOSTDIFF_RESULTS=` + dir + `/results /bin/sh ` + dir + `/install.sh`
	args := append([]string{"-o", "ConnectTimeout=15"}, controlArgs()...)
	args = append(args, flag, "--", m.SSH, "/bin/sh -c '"+wrapper+"'")
	return &Install{
		Cmd: exec.Command(sshProgram(opt), args...),
		Results: func() (map[int]int, error) {
			out, errOut, code, err := remoteRun(`cat ` + dir + `/results`)
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, fmt.Errorf("could not read the results: %s", failure(code, errOut))
			}
			return ParseResults(string(out)), nil
		},
		Cleanup: func() { remoteRun(`rm -rf ` + dir) },
	}, nil
}

// ParseResults reads the "STEP EXITCODE" lines an installer script writes.
func ParseResults(s string) map[int]int {
	out := map[int]int{}
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		step, err1 := strconv.Atoi(f[0])
		code, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && step > 0 {
			out[step] = code
		}
	}
	return out
}
