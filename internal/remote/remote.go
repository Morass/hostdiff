// Package remote takes a snapshot on another machine over ssh. hostdiff runs
// the system ssh client with the destination from the config, so users, keys,
// ports and jump hosts come from ~/.ssh/config and the ssh agent; hostdiff
// never sees a password or key. The remote side runs `hostdiff snap --json`
// (installed there, or this binary sent over stdin for one run) and all
// secret redaction happens there, before anything crosses the connection.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/morass/hostdiff/internal/config"
	"github.com/morass/hostdiff/internal/redact"
	"github.com/morass/hostdiff/internal/snapshot"
)

var kindRe = regexp.MustCompile(`^[a-z]{1,20}$`)

// missingMarker is printed by the probe when hostdiff is not installed.
const missingMarker = "HOSTDIFF-MISSING"

// Options for one remote snapshot.
type Options struct {
	Only    []string
	Skip    []string
	Timeout time.Duration
	// Self is the path of this binary, sent over when upload is allowed.
	Self string
	// SSH is the ssh program; $HOSTDIFF_SSH overrides "ssh" (used by tests).
	SSH string
}

// snapArgs builds the fixed argument string for the remote hostdiff. Section
// names are validated, so nothing from the config reaches the remote shell
// unchecked.
func snapArgs(opt Options) (string, error) {
	args := "snap --json"
	for _, set := range []struct {
		flag  string
		kinds []string
	}{{"--only", opt.Only}, {"--skip", opt.Skip}} {
		if len(set.kinds) == 0 {
			continue
		}
		for _, k := range set.kinds {
			if !kindRe.MatchString(k) {
				return "", fmt.Errorf("invalid section name %q", k)
			}
		}
		args += " " + set.flag + " " + strings.Join(set.kinds, ",")
	}
	return args, nil
}

// probeScript finds an installed hostdiff and runs it. It is written without
// single quotes or backslashes so it survives being wrapped in
// `/bin/sh -c '...'` by any login shell (bash, zsh, fish).
func probeScript(m *config.Machine, args string) string {
	if m.Command != "" {
		cmd := m.Command
		if rest, ok := strings.CutPrefix(cmd, "~/"); ok {
			cmd = `"$HOME"/` + rest
		}
		return fmt.Sprintf(`if [ -x %[1]s ]; then exec %[1]s %[2]s; fi; echo "%[3]s $(uname -s) $(uname -m)" >&2; exit 127`, cmd, args, missingMarker)
	}
	return fmt.Sprintf(`for p in "$(command -v hostdiff 2>/dev/null)" "$HOME/.local/bin/hostdiff" "$HOME/go/bin/hostdiff" /opt/homebrew/bin/hostdiff /usr/local/bin/hostdiff /home/linuxbrew/.linuxbrew/bin/hostdiff; do if [ -n "$p" ] && [ -x "$p" ]; then exec "$p" %s; fi; done; echo "%s $(uname -s) $(uname -m)" >&2; exit 127`, args, missingMarker)
}

// uploadScript receives a binary on stdin into a private temporary folder,
// runs it once and removes it.
func uploadScript(args string) string {
	return fmt.Sprintf(`umask 077; d=$(mktemp -d "${TMPDIR:-/tmp}/hostdiff.XXXXXX") || exit 1; cat > "$d/hostdiff" && chmod 700 "$d/hostdiff" && "$d/hostdiff" %s; rc=$?; rm -rf "$d"; exit $rc`, args)
}

func sshProgram(opt Options) string {
	if opt.SSH != "" {
		return opt.SSH
	}
	if p := os.Getenv("HOSTDIFF_SSH"); p != "" {
		return p
	}
	return "ssh"
}

// run executes one ssh command. The destination has been validated as a
// plain name by config, and "--" ends ssh's options, so it cannot be read as
// an option.
func run(ctx context.Context, opt Options, dest, script string, stdin []byte) (stdout, stderr []byte, code int, err error) {
	cmd := exec.CommandContext(ctx, sshProgram(opt), "-o", "ConnectTimeout=15", "-T", "--", dest, "/bin/sh -c '"+script+"'")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.WaitDelay = 3 * time.Second
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code, err = ee.ExitCode(), nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", opt.Timeout)
	}
	return out.Bytes(), errb.Bytes(), code, err
}

// Snapshot collects a snapshot from machine m.
func Snapshot(m *config.Machine, opt Options) (*snapshot.Snapshot, error) {
	if m.SSH == "" {
		return nil, fmt.Errorf("machine %q has no ssh destination", m.Name)
	}
	if strings.ContainsAny(m.Command, `'\`) {
		return nil, fmt.Errorf("machine %q: command must not contain quotes", m.Name)
	}
	args, err := snapArgs(opt)
	if err != nil {
		return nil, err
	}
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()

	upload := m.Upload == "always"
	if !upload {
		out, errOut, code, err := run(ctx, opt, m.SSH, probeScript(m, args), nil)
		if err != nil {
			return nil, fmt.Errorf("%s: ssh: %w", m.Name, err)
		}
		if code == 0 {
			return parse(m.Name, out)
		}
		osName, arch, missing := parseMissing(errOut)
		if !missing {
			return nil, fmt.Errorf("%s: %s", m.Name, failure(code, errOut))
		}
		if m.Upload == "never" {
			return nil, fmt.Errorf("%s: hostdiff is not installed there (upload = \"never\"); install it or set command = \"PATH\"", m.Name)
		}
		if osName != runtime.GOOS || arch != runtime.GOARCH {
			return nil, fmt.Errorf("%s: hostdiff is not installed there, and this binary (%s/%s) cannot run on %s/%s; install hostdiff there, or run `hostdiff snap -o FILE` on that machine and compare the file", m.Name, runtime.GOOS, runtime.GOARCH, osName, arch)
		}
		upload = true
	}
	self := opt.Self
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		return nil, fmt.Errorf("reading this binary to send it: %w", err)
	}
	out, errOut, code, err := run(ctx, opt, m.SSH, uploadScript(args), bin)
	if err != nil {
		return nil, fmt.Errorf("%s: ssh: %w", m.Name, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("%s: %s", m.Name, failure(code, errOut))
	}
	return parse(m.Name, out)
}

func parse(name string, out []byte) (*snapshot.Snapshot, error) {
	s, err := snapshot.Read(bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return s, nil
}

// parseMissing reads "HOSTDIFF-MISSING Darwin arm64" from the probe.
func parseMissing(stderr []byte) (goos, goarch string, ok bool) {
	for _, l := range strings.Split(string(stderr), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == missingMarker {
			goos = strings.ToLower(f[1])
			switch f[2] {
			case "x86_64", "amd64":
				goarch = "amd64"
			case "arm64", "aarch64":
				goarch = "arm64"
			default:
				goarch = f[2]
			}
			return goos, goarch, true
		}
	}
	return "", "", false
}

// failure summarises a failed remote run without echoing secrets that a
// remote login banner or error might contain.
func failure(code int, stderr []byte) string {
	msg := strings.TrimSpace(string(stderr))
	lines := strings.Split(msg, "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	msg, _ = redact.Secrets(strings.Join(lines, "\n"))
	if code == 255 {
		return "ssh could not connect: " + msg
	}
	return fmt.Sprintf("remote hostdiff exited with %d: %s", code, msg)
}

// Endpoint is where an ssh destination leads, as ssh itself resolves it from
// ~/.ssh/config without connecting (ssh -G).
type Endpoint struct {
	User string
	Host string
	Port string
	// Proxied is true when a ProxyJump or ProxyCommand is involved; the
	// host name is then resolved elsewhere and says nothing about this side.
	Proxied bool
}

// Resolve asks the local ssh client where a destination leads. It never
// opens a connection.
func Resolve(m *config.Machine, opt Options) (Endpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshProgram(opt), "-G", "--", m.SSH)
	out, err := cmd.Output()
	if err != nil {
		return Endpoint{}, err
	}
	var ep Endpoint
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch strings.ToLower(k) {
		case "user":
			ep.User = v
		case "hostname":
			ep.Host = v
		case "port":
			ep.Port = v
		case "proxyjump", "proxycommand":
			if v != "" && v != "none" {
				ep.Proxied = true
			}
		}
	}
	if ep.Host == "" {
		return Endpoint{}, errors.New("ssh -G printed no host name")
	}
	return ep, nil
}

// Addresses returns the IP addresses the endpoint's host name stands for,
// "this machine" when one of them belongs to a local interface. Names that
// do not resolve within the deadline yield nothing.
func (ep Endpoint) Addresses() (addrs []string, here bool) {
	if ep.Proxied {
		return nil, false
	}
	host := strings.Trim(ep.Host, "[]")
	if strings.EqualFold(host, "localhost") {
		return nil, true
	}
	local := map[string]bool{}
	if ifaddrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifaddrs {
			if ipn, ok := a.(*net.IPNet); ok {
				local[ipn.IP.String()] = true
			}
		}
	}
	names := []string{host}
	if net.ParseIP(host) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		names, _ = net.DefaultResolver.LookupHost(ctx, host)
		if h, err := os.Hostname(); err == nil && strings.EqualFold(strings.TrimSuffix(host, "."), h) {
			here = true
		}
	}
	for _, n := range names {
		ip := net.ParseIP(n)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || local[ip.String()] {
			here = true
		}
		addrs = append(addrs, ip.String())
	}
	return addrs, here
}
