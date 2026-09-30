package discover

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
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
func credCorpus() []Session {
	rng := rand.New(rand.NewSource(4))
	noise := []string{"ls", "pwd", "date", "id", "uptime"}
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []Session
	for i := 0; i < 12; i++ {
		s := Session{Client: "fake", ID: fmt.Sprintf("c%02d", i), Start: t0.AddDate(0, 0, 5*i)}
		s.addRequest(fmt.Sprintf("sync project %d", i))
		add := func(c Call) {
			c.Request, c.Time = 0, s.Start
			s.Calls = append(s.Calls, c)
		}
		add(Call{Tool: "shell", Command: noise[rng.Intn(len(noise))]})
		add(Call{Tool: "shell", Command: fmt.Sprintf("curl -s -H 'Authorization: %s' https://api.example.com/projects/%d", fakeBearer, i)})
		add(Call{Tool: "mcp:vault_write", Args: map[string]string{"path": "secret/app", "token": fakeGitlab, "data": fmt.Sprintf(`{"db_password": "pw-%d-abcdefgh"}`, i)}, RawArgs: map[string]bool{"data": true}})
		add(Call{Tool: "shell", Command: fmt.Sprintf("git clone https://deploy:hunter2secret@git.example.com/p%d.git", i)})
		out = append(out, s)
	}
	return out
}

func TestDraftNeverWritesCredentials(t *testing.T) {
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: credCorpus()}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	var r *Routine
	for i := range rep.Routines {
		if strings.Contains(labelsOf(rep.Routines[i].Candidate), "vault_write") {
			r = &rep.Routines[i]
		}
	}
	if r == nil || r.Draft() == nil {
		t.Fatalf("no drafted routine: %+v", rep.Funnel)
	}
	d := r.Draft()
	for name, body := range d.Files {
		for _, secret := range []string{"abcDEF1234567890ghiJKL", fakeGitlab, "hunter2secret", "pw-"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Errorf("%s contains a recorded credential %q", name, secret)
			}
		}
	}
	sensitive := 0
	for _, in := range d.Inputs {
		if in.Sensitive {
			sensitive++
			if in.Example != "" || in.Type != SlotSecret {
				t.Errorf("sensitive input keeps a value or type: %+v", in)
			}
		}
	}
	if sensitive < 3 {
		t.Fatalf("want the header, the token and the URL password as credential inputs, got %d: %+v", sensitive, d.Inputs)
	}
	if len(d.Blocked) != 0 {
		t.Fatalf("every credential became an input, so nothing should remain: %v", d.Blocked)
	}
	if _, _, err := d.Package(); err != nil {
		t.Fatalf("a clean draft must package: %v", err)
	}
	var buf bytes.Buffer
	WriteFunnel(&buf, rep, 0, true)
	if strings.Contains(buf.String(), "abcDEF1234567890ghiJKL") || strings.Contains(buf.String(), "hunter2secret") {
		t.Fatal("the printed report leaks a credential")
	}
}

func TestLeftoverCredentialBlocksEverything(t *testing.T) {
	d := &Draft{Files: map[string][]byte{"main.sh": []byte("curl -H 'Authorization: " + fakeBearer + "' x\n")}}
	d.Blocked = scanArtifacts(d.Files)
	if len(d.Blocked) != 1 || strings.Contains(d.Blocked[0], "abcDEF") {
		t.Fatalf("blocked = %v (it must say where, never what)", d.Blocked)
	}
	if _, err := d.Artifacts(); !errors.Is(err, ErrBlocked) {
		t.Fatalf("artifacts: %v", err)
	}
	if _, _, err := d.Package(); !errors.Is(err, ErrBlocked) {
		t.Fatalf("package: %v", err)
	}
	if _, _, err := d.Save(t.TempDir()); err == nil {
		t.Fatal("save must refuse a blocked draft")
	}
}
