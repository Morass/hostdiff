package redact

import (
	"os"
	"strings"
	"testing"
)

// Token-shaped fixtures are assembled at run time so the source never holds
// a literal that secret scanners (gitleaks, trufflehog, push protection) flag.
func fake(prefix string, n int) string { return prefix + strings.Repeat("Q7", n)[:n] }

var (
	ghToken  = fake("ghp"+"_", 36)
	awsKey   = "AK" + "IA" + "ABCDEFGHIJKLMNOP"
	npmToken = fake("npm"+"_", 36)
	antKey   = "sk-" + "ant-" + fake("api03-", 24)
	jwt      = "ey" + "JhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0." + "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	pemBlock = "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END " + "OPENSSH PRIVATE KEY-----"
)

func TestSecretsRedacted(t *testing.T) {
	cases := []struct{ text, leak string }{
		{"export GH=" + ghToken, ghToken[4:12]},
		{"aws_access_key_id = " + awsKey, awsKey[4:]},
		{"//registry.npmjs.org/:_authToken=" + npmToken, npmToken[4:12]},
		{"ANTHROPIC_API_KEY=" + antKey, antKey[8:16]},
		{"password: hunter2hunter2", "hunter2"},
		{"password=abc12", "abc12"},
		{`password="correct horse battery staple"`, "horse"},
		{"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCY", "wJalrXUt"},
		{"SECRET_KEY_BASE=9f8e7d6c5b4a39281706", "9f8e7d6c"},
		{"Authorization: Bearer abcdefghijklmnop123", "abcdefghijklmnop"},
		{"curl -H 'Authorization: Basic dXNlcjpwYXNz' x", "dXNlcjpwYXNz"},
		{"machine api.example.org login bob password hunter2hunter2", "hunter2"},
		{"git clone https://bob:s3cretpass@example.org/r.git", "s3cretpass"},
		{"url=https://hooks.slack.com/services/T000/B000/XXXXXXXX", "T000/B000"},
		{pemBlock, "b3BlbnNzaC1rZXk"},
		{"\n# truncated\n" + pemBlock[:60], "b3BlbnNzaC1rZXk"[:6]},
		{"Authorization: " + jwt, "dozjgNry"},
	}
	for _, c := range cases {
		out, n := Text(c.text, "/nonexistent")
		if n == 0 || strings.Contains(out, c.leak) {
			t.Errorf("not redacted (%d): %q -> %q", n, c.text, out)
		}
		if !ContainsSecret([]byte(c.text)) && !strings.Contains(c.text, "abc12") {
			t.Errorf("ContainsSecret missed %q", c.text)
		}
	}
}

func TestPersonalDetailsGeneralised(t *testing.T) {
	in := "[user]\n\temail = alice@corp.example.org\nHost box\n  HostName workstation.internal\n  User alice\nsource /home/bob/private.sh\n"
	out, _ := Text(in, "/Users/alice")
	for _, leak := range []string{"alice@", "workstation.internal", "User alice", "/home/bob"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in:\n%s", leak, out)
		}
	}
	out, _ = Text("git@github.com:org/repo.git", "")
	if out != "git@github.com:org/repo.git" {
		t.Errorf("ssh remote mangled: %q", out)
	}
}

func TestPlainTextUntouched(t *testing.T) {
	for _, s := range []string{
		`export PATH="/opt/foo/bin:$PATH"` + "\nalias ll='ls -l'\n",
		"export GH_TOKEN=\"$(security find-generic-password -w gh)\"\n",
		"token_file = ${XDG_CONFIG_HOME}/x\n",
		".TH TREE 1 \"2.3.2\"\n",
	} {
		if ContainsSecret([]byte(s)) {
			t.Errorf("false positive: %q", s)
		}
		if out, n := Text(s, "/nonexistent"); n != 0 {
			t.Errorf("changed innocent text (%d): %q -> %q", n, s, out)
		}
	}
}

func TestArgv(t *testing.T) {
	got := strings.Join(Argv([]string{"deploy", "--password", "hunter2hunter2", "--api-token=abcdef123456", "GH=" + ghToken, "--verbose"}), " ")
	for _, leak := range []string{"hunter2", "abcdef123456", ghToken[4:12]} {
		if strings.Contains(got, leak) {
			t.Errorf("argv leaked %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "--verbose") || !strings.HasPrefix(got, "deploy --password [REDACTED]") {
		t.Errorf("argv over-redacted: %s", got)
	}
}

func TestHomeGeneralised(t *testing.T) {
	home, _ := os.UserHomeDir()
	out, _ := Text("source "+home+"/.cargo/env", "")
	if strings.Contains(out, home) || !strings.Contains(out, "~/.cargo/env") {
		t.Errorf("home not generalised: %q", out)
	}
	out, _ = Text("source /Users/recorded/.cargo/env", "/Users/recorded")
	if out != "source ~/.cargo/env" {
		t.Errorf("recorded home not generalised: %q", out)
	}
}

func TestSecretsKeepsPersonalDetails(t *testing.T) {
	in := "email = alice@corp.example.org\nexport GH=" + ghToken + "\nHostName workstation.internal\n"
	out, n := Secrets(in)
	if n != 1 || strings.Contains(out, ghToken[4:12]) {
		t.Fatalf("token not redacted (%d): %q", n, out)
	}
	for _, keep := range []string{"alice@corp.example.org", "workstation.internal"} {
		if !strings.Contains(out, keep) {
			t.Errorf("Secrets removed %q, which is not a secret: %q", keep, out)
		}
	}
}

func TestLoginIsNotASecret(t *testing.T) {
	for _, s := range []string{"login shell = /bin/bash", "it needs the login session; run it there", "machine example.org login bob"} {
		if out, n := Secrets(s); n != 0 || out != s {
			t.Errorf("redacted a non-secret: %q -> %q", s, out)
		}
	}
}

func TestShapes(t *testing.T) {
	if out, n := Shapes("credential.https://github.com.helper"); n != 0 || out != "credential.https://github.com.helper" {
		t.Errorf("config key mangled: %q", out)
	}
	key := `url.https://x-access-token:` + ghToken + `@github.com/.insteadof`
	if out, n := Shapes(key); n == 0 || strings.Contains(out, ghToken[4:12]) {
		t.Errorf("token in key kept: %q", out)
	}
}
