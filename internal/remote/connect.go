package remote

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/morass/hostdiff/internal/config"
	"github.com/morass/hostdiff/internal/redact"
	"github.com/morass/hostdiff/internal/snapshot"
)

// Reach says what stops hostdiff from using a machine on its own.
type Reach int

const (
	// Reachable: ssh connects without asking anything.
	Reachable Reach = iota
	// NeedsHostKey: the machine's key is unknown or has changed, so
	// somebody has to look at the fingerprint and accept it.
	NeedsHostKey
	// NeedsAuth: the login needs a password or a key that is not set up.
	NeedsAuth
	// Unreachable: no connection at all (name, network, port).
	Unreachable
)

// controlPath is one shared ssh connection per hostdiff run: the user types
// a password or accepts a host key once, and the snapshot, the install and
// reading the results all reuse that connection.
var (
	controlOnce sync.Once
	controlDir  string
)

func controlArgs() []string {
	controlOnce.Do(func() {
		// Short path: a unix socket name is limited to about 100 bytes.
		dir, err := os.MkdirTemp("/tmp", "hd-ssh-")
		if err != nil {
			return
		}
		controlDir = dir
	})
	if controlDir == "" {
		return nil
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + filepath.Join(controlDir, "%C"),
		"-o", "ControlPersist=120",
	}
}

// CloseControl ends the shared connections and removes their sockets.
func CloseControl() {
	if controlDir == "" {
		return
	}
	entries, _ := os.ReadDir(controlDir)
	for _, e := range entries {
		exec.Command("ssh", "-O", "exit", "-o", "ControlPath="+filepath.Join(controlDir, e.Name()), "x").Run()
	}
	os.RemoveAll(controlDir)
	controlDir = ""
}

// Check connects without asking anything, to find out whether hostdiff can
// use the machine. The second result is what ssh said, redacted.
func Check(m *config.Machine, opt Options) (Reach, string) {
	if m == nil || m.SSH == "" {
		return Unreachable, "no ssh destination"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}, controlArgs()...)
	args = append(args, "-T", "--", m.SSH, "true")
	cmd := exec.CommandContext(ctx, sshProgram(opt), args...)
	var errb progressWriter
	cmd.Stdout, cmd.Stderr = nil, &errb
	err := cmd.Run()
	msg := strings.TrimSpace(string(errb.bytes()))
	if err == nil {
		return Reachable, msg
	}
	return classify(msg), failureText(msg)
}

// classify reads ssh's own words.
func classify(stderr string) Reach {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "host key verification failed"),
		strings.Contains(s, "authenticity of host"),
		strings.Contains(s, "host identification has changed"),
		strings.Contains(s, "no matching host key"):
		return NeedsHostKey
	case strings.Contains(s, "permission denied"),
		strings.Contains(s, "authentication failed"),
		strings.Contains(s, "no supported authentication"),
		strings.Contains(s, "password"),
		strings.Contains(s, "publickey"):
		return NeedsAuth
	}
	return Unreachable
}

// failureText keeps the last lines of ssh's message, redacted first so a
// banner cannot leak anything.
func failureText(msg string) string {
	red, _ := redact.Secrets(msg)
	lines := strings.Split(snapshot.StripControls(red), "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ConnectCommand returns ssh run in this terminal, so the user can check a
// fingerprint and type a password. It opens the shared connection, which
// the rest of the run then reuses without asking again.
func ConnectCommand(m *config.Machine, opt Options) *exec.Cmd {
	args := append([]string{"-o", "ConnectTimeout=30"}, controlArgs()...)
	args = append(args, "-t", "--", m.SSH, "echo hostdiff: connected as \"$(id -un)\" on \"$(uname -n)\"; sleep 1")
	return exec.Command(sshProgram(opt), args...)
}

// connectHint explains what to do about a connection ssh refused.
func connectHint(m *config.Machine, stderr []byte) string {
	switch classify(string(stderr)) {
	case NeedsHostKey:
		return "\nhostdiff cannot answer ssh's questions: run `ssh " + m.SSH + "` once in a terminal to check and accept the host key, or use hostdiff interactively (just `hostdiff`), which offers to connect for you"
	case NeedsAuth:
		return "\nthe login needs a password or a key: run `ssh-copy-id " + m.SSH + "` to install your key, or use hostdiff interactively (just `hostdiff`), which lets you type the password once"
	}
	return ""
}
