// Package redact finds and removes secrets and personal details. Secrets is
// applied to everything hostdiff collects, on the machine it is collected on,
// so a token in a dotfile never reaches a snapshot or the ssh connection.
// Text additionally generalises personal details for output meant to be
// shared (e-mail addresses, ssh hosts, home directories).
package redact

import (
	"os"
	"os/user"
	"regexp"
	"strings"
)

// Well-known credential shapes. Order matters: longer, more specific
// prefixes first so a generic rule does not eat half a token.
var tokens = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|$)`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+`),
	regexp.MustCompile(`https://(?:discord|discordapp)\.com/api/webhooks/[A-Za-z0-9/_-]+`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`),
	regexp.MustCompile(`\bhv[sb]\.[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
}

// A key whose name says it holds a secret, then a separator, then a value.
// The name may carry prefixes and suffixes (AWS_SECRET_ACCESS_KEY,
// SECRET_KEY_BASE, db_password_prod). Quoted values are taken whole.
const secretWord = `(?:api[_-]?key|access[_-]?key|secret|token|passw(?:or)?d|passphrase|pwd|private[_-]?key|credential|client[_-]?secret|auth)`

var assignment = regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*` + secretWord + `[A-Za-z0-9_.-]*["']?\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;]+)`)

// Header and netrc/CLI forms that carry no = or :.
var (
	authHeader = regexp.MustCompile(`(?i)(\bauthorization\s*[:=]\s*["']?)((?:bearer|basic|token|digest)\s+)?([^\s"']+)`)
	bearer     = regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{12,})`)
	netrc      = regexp.MustCompile(`(?i)(\b(?:password|passwd)\s+)(\S+)`)
	urlCreds   = regexp.MustCompile(`([a-z][a-z0-9+.-]*://[^/\s:@]+:)([^@\s/]+)(@)`)
	email      = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`)
	sshHost    = regexp.MustCompile(`(?im)^(\s*(?:HostName|User|IdentityFile|ProxyJump)\s+)(\S+)`)
	otherHome  = regexp.MustCompile(`(/(?:Users|home)/)([^/\s"':]+)`)

	// fish `set -gx API_TOKEN value`, csh `setenv API_TOKEN value`.
	setVar = regexp.MustCompile(`(?im)(\b(?:set(?:\s+-[A-Za-z-]+)*|setenv)\s+[A-Za-z0-9_]*` + secretWord + `[A-Za-z0-9_]*\s+)("[^"\n]*"|'[^'\n]*'|[^\s"';]+)`)
	// curl's `user = "name:password"`, `-u name:password`, `--proxy-user`.
	userPass = regexp.MustCompile(`(?im)((?:^|\s)(?:-u|-U|--user|--proxy-user|user|proxy-user)(?:\s*=\s*|\s+)["']?[^:\s"'=]+:)([^\s"']+)`)
	// A value after a secret-named flag: `--password value`, and the same
	// pair as consecutive plist array strings (launchd ProgramArguments).
	flagValue = regexp.MustCompile(`(?i)((?:^|[\s>"'])-{1,2}[A-Za-z0-9_-]*` + secretWord + `[A-Za-z0-9_-]*(?:"|')?(?:</string>\s*<string>|\s+))("[^"\n]*"|'[^'\n]*'|[^\s"'<]+)`)
	// <key>API_TOKEN</key><string>value</string> in a plist.
	plistValue = regexp.MustCompile(`(?i)(<key>[^<]*` + secretWord + `[^<]*</key>\s*<string>)([^<]*)(</string>)`)
	secretName = regexp.MustCompile(`(?i)^[A-Za-z0-9_-]*` + secretWord + `[A-Za-z0-9_-]*$`)
	// "auth" also starts author and authority; a name secret only because of
	// that is not one (authorization still is).
	authorWord = regexp.MustCompile(`(?i)author(?:s|ity|ities|ed|ing)?([^A-Za-z]|$)`)
	anySecret  = regexp.MustCompile(`(?i)` + secretWord)
)

// namesSecret reports whether a matched name really is secret-named.
func namesSecret(name string) bool {
	return anySecret.MatchString(authorWord.ReplaceAllString(name, "$1"))
}

// Placeholder values are not secrets; skipping them keeps shell files that
// only reference a variable (export TOKEN="$GH_TOKEN") diffable.
func placeholder(v string) bool {
	// In single quotes a shell does not expand $, so '$x' is a literal.
	literal := strings.HasPrefix(v, "'")
	v = strings.Trim(v, `"'`)
	if len(v) < 3 {
		return true
	}
	if (!literal && strings.HasPrefix(v, "$")) || strings.HasPrefix(v, "<") || strings.HasPrefix(v, "%") || strings.HasPrefix(v, "[REDACTED") {
		return true
	}
	switch strings.ToLower(v) {
	case "true", "false", "yes", "off", "nil", "none", "null", "changeme", "example", "xxxx", "your-token-here":
		return true
	}
	return false
}

// ContainsSecret reports whether content holds something that must not be
// copied into the store: a private key, a known token shape, credentials in a
// URL, an authorization header, or a literal value assigned to a secret-named
// key. It errs towards yes: a file that is not stored is still diffed by hash.
func ContainsSecret(b []byte) bool {
	s := string(b)
	for _, re := range tokens {
		if re.MatchString(s) {
			return true
		}
	}
	if urlCreds.MatchString(s) {
		return true
	}
	for _, m := range authHeader.FindAllStringSubmatch(s, -1) {
		if !placeholder(m[3]) {
			return true
		}
	}
	for _, m := range assignment.FindAllStringSubmatch(s, -1) {
		if !placeholder(m[2]) && len(strings.Trim(m[2], `"'`)) >= 6 {
			return true
		}
	}
	for _, m := range netrc.FindAllStringSubmatch(s, -1) {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(m[1])), "pass") && !placeholder(m[2]) && len(m[2]) >= 6 {
			return true
		}
	}
	return false
}

// Secrets returns s with credentials replaced by [REDACTED]: key blocks,
// known token shapes, credentials in URLs, authorization headers, netrc
// passwords and literal values assigned to secret-named keys. n counts them.
func Secrets(s string) (string, int) {
	n := 0
	for _, re := range tokens {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return "[REDACTED]" })
	}
	s = urlCreds.ReplaceAllStringFunc(s, func(m string) string {
		n++
		sub := urlCreds.FindStringSubmatch(m)
		return sub[1] + "[REDACTED]" + sub[3]
	})
	s = authHeader.ReplaceAllStringFunc(s, func(m string) string {
		sub := authHeader.FindStringSubmatch(m)
		if placeholder(sub[3]) {
			return m
		}
		n++
		return sub[1] + sub[2] + "[REDACTED]"
	})
	s = bearer.ReplaceAllStringFunc(s, func(m string) string {
		sub := bearer.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], "[REDACTED") {
			return m
		}
		n++
		return sub[1] + "[REDACTED]"
	})
	s = assignment.ReplaceAllStringFunc(s, func(m string) string {
		sub := assignment.FindStringSubmatch(m)
		if placeholder(sub[2]) || !namesSecret(sub[1]) {
			return m
		}
		n++
		return sub[1] + "[REDACTED]"
	})
	s = netrc.ReplaceAllStringFunc(s, func(m string) string {
		sub := netrc.FindStringSubmatch(m)
		if placeholder(sub[2]) {
			return m
		}
		n++
		return sub[1] + "[REDACTED]"
	})
	for _, re := range []*regexp.Regexp{setVar, userPass, flagValue} {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if placeholder(sub[2]) || strings.HasPrefix(sub[2], "-") || (re != userPass && !namesSecret(sub[1])) {
				return m
			}
			n++
			return sub[1] + "[REDACTED]"
		})
	}
	s = plistValue.ReplaceAllStringFunc(s, func(m string) string {
		sub := plistValue.FindStringSubmatch(m)
		if placeholder(sub[2]) {
			return m
		}
		n++
		return sub[1] + "[REDACTED]" + sub[3]
	})
	return s, n
}

// Value returns value, or [REDACTED] when name (a setting or variable name
// without its value, such as the last part of a git config key) says it
// holds a secret. It is for values that were split from their names before
// Secrets could see the pair.
func Value(name, value string) string {
	if secretName.MatchString(name) && namesSecret(name) && !placeholder(value) {
		return "[REDACTED]"
	}
	return value
}

// Text returns s with secrets replaced by [REDACTED], e-mail addresses,
// SSH host details and home directories generalised. home is the home
// directory the text was recorded under (may differ from the current one).
// n counts replacements of secrets.
func Text(s, home string) (string, int) {
	s, n := Secrets(s)
	s = email.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "git@") || strings.HasSuffix(m, "@example.com") || strings.HasSuffix(m, "users.noreply.github.com") {
			return m
		}
		return "[email]"
	})
	s = sshHost.ReplaceAllString(s, "${1}[redacted]")
	return Paths(s, home), n
}

// Argv redacts a command line: token shapes, secret assignments and the
// value following a secret-named flag (--password X, --token=X).
func Argv(argv []string) []string {
	out := make([]string, len(argv))
	flag := regexp.MustCompile(`(?i)^-{1,2}[A-Za-z0-9_-]*` + secretWord + `[A-Za-z0-9_-]*$`)
	for i, a := range argv {
		if i > 0 && flag.MatchString(argv[i-1]) && !strings.HasPrefix(a, "-") {
			out[i] = "[REDACTED]"
			continue
		}
		if j := strings.Index(a, "="); j > 0 && flag.MatchString(a[:j]) {
			out[i] = a[:j+1] + "[REDACTED]"
			continue
		}
		out[i], _ = Text(a, "")
	}
	return out
}

// Paths replaces the recorded home (or the current one) with ~, other
// users' home directories with /Users/[user], and the user name with $USER.
func Paths(s, home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" && home != "/" {
		s = strings.ReplaceAll(s, home, "~")
	}
	s = otherHome.ReplaceAllString(s, "${1}[user]")
	if u, err := user.Current(); err == nil && len(u.Username) > 2 {
		s = regexp.MustCompile(`\b`+regexp.QuoteMeta(u.Username)+`\b`).ReplaceAllString(s, "$$USER")
	}
	return s
}

// Shapes redacts only unmistakable credentials: key blocks, known token
// formats and passwords inside URLs. It is for names (configuration keys,
// paths) where the name=value rule of Secrets would misfire, as in git's
// "credential.https://example.org.helper".
func Shapes(s string) (string, int) {
	n := 0
	for _, re := range tokens {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return "[REDACTED]" })
	}
	s = urlCreds.ReplaceAllStringFunc(s, func(m string) string {
		n++
		sub := urlCreds.FindStringSubmatch(m)
		return sub[1] + "[REDACTED]" + sub[3]
	})
	return s, n
}
