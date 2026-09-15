package collect

import (
	"strings"

	"github.com/morass/hostdiff/internal/snapshot"
)

func sysPackages(e *Env, s *snapshot.Section) {
	found := false
	if e.Look("apt-mark") != "" {
		found = true
		manual, err := e.Out("apt-mark", "showmanual")
		if err == nil {
			versions := map[string]string{}
			if out, err := e.Out("dpkg-query", "-W", "-f=${Package}\t${Version}\n"); err == nil {
				for _, l := range Lines(out) {
					if f := strings.SplitN(l, "\t", 2); len(f) == 2 {
						versions[f[0]] = f[1]
					}
				}
			}
			for _, p := range Lines(manual) {
				s.Add("apt › "+p, orDash(versions[p]), "")
			}
		}
	}
	if e.Look("dnf") != "" {
		found = true
		if out, err := e.Out("dnf", "repoquery", "--userinstalled", "--qf", "%{name}\t%{version}\n"); err == nil {
			for _, l := range Lines(out) {
				if f := strings.SplitN(l, "\t", 2); len(f) == 2 {
					s.Add("dnf › "+f[0], f[1], "")
				}
			}
		}
	}
	if e.Look("pacman") != "" {
		found = true
		if out, err := e.Out("pacman", "-Qe"); err == nil {
			for _, l := range Lines(out) {
				if f := strings.Fields(l); len(f) == 2 {
					s.Add("pacman › "+f[0], f[1], "")
				}
			}
		}
	}
	if e.Look("snap") != "" {
		found = true
		if out, err := e.Out("snap", "list"); err == nil {
			for i, l := range Lines(out) {
				if f := strings.Fields(l); i > 0 && len(f) >= 2 {
					s.Add("snap › "+f[0], f[1], "")
				}
			}
		}
	}
	if e.Look("flatpak") != "" {
		found = true
		if out, err := e.Out("flatpak", "list", "--app", "--columns=application,version"); err == nil {
			for _, l := range Lines(out) {
				f := strings.Fields(l)
				if len(f) >= 1 {
					v := "-"
					if len(f) >= 2 {
						v = f[1]
					}
					s.Add("flatpak › "+f[0], v, "")
				}
			}
		}
	}
	if !found {
		s.Status, s.Note = snapshot.Absent, "no apt, dnf, pacman, snap or flatpak found"
	}
}

func systemd(e *Env, s *snapshot.Section) {
	if e.Look("systemctl") == "" {
		s.Status, s.Note = snapshot.Absent, "systemctl not found"
		return
	}
	for _, scope := range []struct {
		label string
		args  []string
	}{
		{"system", []string{"list-unit-files", "--state=enabled", "--no-legend", "--no-pager"}},
		{"user", []string{"--user", "list-unit-files", "--state=enabled", "--no-legend", "--no-pager"}},
	} {
		out, err := e.Out("systemctl", scope.args...)
		if err != nil {
			continue
		}
		for _, l := range Lines(out) {
			if f := strings.Fields(l); len(f) >= 2 {
				s.Add(scope.label+" › "+f[0], f[1], "")
			}
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
