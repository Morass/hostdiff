package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/morass/hostdiff/internal/config"
	"github.com/morass/hostdiff/internal/snapshot"
)

func fakeSSH(t *testing.T, output string) Options {
	p := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n[ \"$1\" = -G ] || exit 9\ncat <<'X'\n"+output+"X\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{SSH: p}
}

func TestResolveFindsThisMachine(t *testing.T) {
	m := &config.Machine{SSH: "box"}
	for _, tc := range []struct {
		out  string
		here bool
	}{
		{"user me\nhostname localhost\nport 22\n", true},
		{"user me\nhostname ::1\nport 22\n", true},
		{"user me\nhostname 2001:db8::10\nport 22\n", false},
		{"user me\nhostname localhost\nport 22\nproxyjump gateway\n", false},
		{"user me\nhostname localhost\nport 2222\n", false},
		{"user me\nhostname 127.0.0.1\nport 2222\n", false},
		{"user me\nhostname localhost\nport 22\nproxycommand none\n", true},
	} {
		ep, err := Resolve(m, fakeSSH(t, tc.out))
		if err != nil {
			t.Fatalf("%q: %v", tc.out, err)
		}
		if _, here := ep.Addresses(); here != tc.here {
			t.Errorf("%q: here = %v", tc.out, here)
		}
	}
	if _, err := Resolve(m, fakeSSH(t, "")); err == nil {
		t.Error("empty ssh -G output accepted")
	}
}

// A long private key in remote stderr must be redacted before the message is
// cut to its last lines, which would drop the BEGIN marker.
func TestFailureRedactsBeforeTruncating(t *testing.T) {
	body := strings.Repeat("b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n", 10)
	key := "-----BEGIN " + "OPENSSH PRIVATE KEY-----\n" + body + "-----END " + "OPENSSH PRIVATE KEY-----"
	msg := failure(1, []byte("oops\n"+key+"\n\x1b[2Jdone"))
	if strings.Contains(msg, "b3BlbnNz") || strings.Contains(msg, "\x1b") {
		t.Fatalf("leaked: %q", msg)
	}
}

// The uploaded binary is removed when the run is interrupted, not only when
// it finishes.
func TestUploadScriptCleansUpWhenInterrupted(t *testing.T) {
	if strings.ContainsAny(uploadScript("snap --json"), `'\`) {
		t.Fatal("upload script must not contain single quotes or backslashes")
	}
	tmp := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", uploadScript("snap --json"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + tmp}
	cmd.Stdin = strings.NewReader("#!/bin/sh\nsleep 30\n")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m, _ := filepath.Glob(filepath.Join(tmp, "hostdiff.*", "hostdiff")); len(m) == 1 {
			if st, err := os.Stat(m[0]); err == nil && st.Mode()&0o100 != 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("binary never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	// Ctrl-C reaches the whole process group, the binary included.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wrapper did not end")
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "hostdiff.*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// Progress lines from the remote hostdiff are passed on, even when split
// across writes; everything else stays as the error text.
func TestProgressWriter(t *testing.T) {
	var got []snapshot.Progress
	w := &progressWriter{fn: func(p snapshot.Progress) { got = append(got, p) }}
	w.Write([]byte("hello\n" + ProgressMarker + " start brew 0 -\n" + ProgressMarker[:5]))
	w.Write([]byte(ProgressMarker[5:] + " done brew 12 ok\n" + ProgressMarker + " done bad;kind 1 ok\ntail"))
	rest := string(w.bytes())
	if rest != "hello\n"+ProgressMarker+" done bad;kind 1 ok\ntail" {
		t.Errorf("kept %q", rest)
	}
	if len(got) != 2 || got[1].Kind != "brew" || got[1].Items != 12 || got[1].Status != snapshot.OK {
		t.Errorf("progress %+v", got)
	}
}

// The remote side names the temporary folder; anything but a plain path is
// refused before it reaches a command line.
func TestInstallRefusesHostileFolder(t *testing.T) {
	for _, reply := range []string{"/tmp/x; rm -rf ~", "/tmp/a'b", "/tmp/$HOME", "relative/path", "/tmp/a b"} {
		p := filepath.Join(t.TempDir(), "ssh")
		os.WriteFile(p, []byte("#!/bin/sh\ncat >/dev/null\necho '"+strings.ReplaceAll(reply, "'", "'\\''")+"'\n"), 0o755)
		t.Setenv("HOSTDIFF_SSH", p)
		if in, err := InstallCommand(&config.Machine{Name: "box", SSH: "box"}, "echo hi", false); err == nil {
			t.Errorf("accepted %q: %v", reply, in.Cmd.Args)
		}
	}
}
