package remote

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/morass/hostdiff/internal/config"
)

var tmpPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/+-]{1,400}$`)

// InstallCommand copies an installer script to m and returns the ssh command
// that runs it there. With tty set the session gets a terminal, so password
// prompts (sudo, a cask's installer) work and Ctrl-C reaches the steps. The
// script is sent over a plain connection first: the command line that runs
// it only carries a path hostdiff has checked, and the file removes itself.
func InstallCommand(m *config.Machine, script string, tty bool) (*exec.Cmd, func() (map[int]int, error), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opt := Options{}
	out, errOut, code, err := run(ctx, opt, m.SSH, `umask 077; f=$(mktemp "${TMPDIR:-/tmp}/hostdiff-install.XXXXXX") && cat > "$f" && echo "$f"`, []byte(script))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: ssh: %w", m.Name, err)
	}
	if code != 0 {
		return nil, nil, fmt.Errorf("%s: %s", m.Name, failure(code, errOut))
	}
	path := strings.TrimSpace(string(out))
	if !tmpPathRe.MatchString(path) {
		return nil, nil, fmt.Errorf("%s: unexpected temporary file name from the remote side", m.Name)
	}
	flag := "-T"
	if tty {
		flag = "-t"
	}
	wrapper := `trap "rm -f ` + path + `" EXIT; trap "exit 130" INT; trap "exit 129" HUP; HOSTDIFF_RESULTS=` + path + `.results /bin/sh ` + path
	args := append([]string{"-o", "ConnectTimeout=15"}, controlArgs()...)
	args = append(args, flag, "--", m.SSH, "/bin/sh -c '"+wrapper+"'")
	cmd := exec.Command(sshProgram(opt), args...)
	// What each step did is read back afterwards, then removed.
	results := func() (map[int]int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, errOut, code, err := run(ctx, opt, m.SSH, `cat `+path+`.results 2>/dev/null; rm -f `+path+`.results`, nil)
		if err != nil {
			return nil, err
		}
		if code != 0 {
			return nil, errors.New(failure(code, errOut))
		}
		return ParseResults(string(out)), nil
	}
	return cmd, results, nil
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
