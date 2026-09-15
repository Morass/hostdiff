package remote

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
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
func InstallCommand(m *config.Machine, script string, tty bool) (*exec.Cmd, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opt := Options{}
	out, errOut, code, err := run(ctx, opt, m.SSH, `umask 077; f=$(mktemp "${TMPDIR:-/tmp}/hostdiff-install.XXXXXX") && cat > "$f" && echo "$f"`, []byte(script))
	if err != nil {
		return nil, fmt.Errorf("%s: ssh: %w", m.Name, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("%s: %s", m.Name, failure(code, errOut))
	}
	path := strings.TrimSpace(string(out))
	if !tmpPathRe.MatchString(path) {
		return nil, fmt.Errorf("%s: unexpected temporary file name from the remote side", m.Name)
	}
	flag := "-T"
	if tty {
		flag = "-t"
	}
	wrapper := `trap "rm -f ` + path + `" EXIT; trap "exit 130" INT; trap "exit 129" HUP; /bin/sh ` + path
	return exec.Command(sshProgram(opt), "-o", "ConnectTimeout=15", flag, "--", m.SSH, "/bin/sh -c '"+wrapper+"'"), nil
}
