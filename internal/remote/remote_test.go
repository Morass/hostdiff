package remote

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/morass/hostdiff/internal/config"
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
