package redact

import (
	"strings"
	"testing"
)

// Synthetic credentials only. None of these is real.
const (
	fakeBearer = "Bearer abcDEF1234567890ghiJKL"
	fakeGitlab = "glpat-AbCdEfGhIjKlMnOpQrSt"
	fakeJWT    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	fakeKey    = "AKIAABCDEFGHIJKLMNOP"
)

func TestRedactCatchesCredentialShapes(t *testing.T) {
	for _, in := range []string{
		"curl -H 'Authorization: " + fakeBearer + "' https://api.example.com",
		"curl -H 'PRIVATE-TOKEN: " + fakeGitlab + "' https://gitlab.com/api/v4",
		"token is " + fakeJWT,
		"aws " + fakeKey,
		"psql postgres://admin:hunter2secret@db.internal:5432/app",
		"https://bucket.s3.amazonaws.com/f?X-Amz-Signature=abcdef0123456789",
		`{"client_secret": "s3cr3t-value-here"}`,
		"export GITLAB_API_TOKEN=abcdefgh12345678",
		"-----BEGIN RSA PRIVATE KEY-----",
		"xoxb-1234567890-abcdefghij",
		"sk-ant-api03-abcdefghijklmnopqrstuvwx",
	} {
		if out := Redact(in); out == in || !strings.Contains(out, "<redacted") {
			t.Errorf("not redacted: %q -> %q", in, out)
		}
	}
	for _, in := range []string{
		"issue_key: TENG-3054",
		"max_output_tokens=4000",
		"sha256:9f2c1e0b7a6d5c4b3a291807f6e5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3",
		"kubectl --context minikube get pods",
		"git commit -m 'fix the token refresh bug'",
		"curl -H \"Authorization: Bearer $GITLAB_TOKEN\" https://gitlab.com/api/v4",
	} {
		if out := Redact(in); out != in {
			t.Errorf("redacted a non-credential: %q -> %q", in, out)
		}
	}
}

// credCorpus: 12 sessions each run one request with a credential in four
// places: a curl header identical in every run, a token argument, a password
// nested in a JSON argument, and a URL with a password.
