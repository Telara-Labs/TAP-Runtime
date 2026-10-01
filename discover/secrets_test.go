package discover

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Synthetic credentials only. None of these is real.
const (
	fakeBearer = "Bearer abcDEF1234567890ghiJKL"
	fakeGitlab = "glpat-AbCdEfGhIjKlMnOpQrSt"
	fakeJWT    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	fakeKey    = "AKIAABCDEFGHIJKLMNOP"
)

func credCorpus() []trace.Session {
	rng := rand.New(rand.NewSource(4))
	noise := []string{"ls", "pwd", "date", "id", "uptime"}
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []trace.Session
	for i := 0; i < 12; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("c%02d", i), Start: t0.AddDate(0, 0, 5*i)}
		s.AddRequest(fmt.Sprintf("sync project %d", i))
		add := func(c trace.Call) {
			c.Request, c.Time = 0, s.Start
			s.Calls = append(s.Calls, c)
		}
		add(trace.Call{Tool: "shell", Command: noise[rng.Intn(len(noise))]})
		add(trace.Call{Tool: "shell", Command: fmt.Sprintf("curl -s -H 'Authorization: %s' https://api.example.com/projects/%d", fakeBearer, i)})
		add(trace.Call{Tool: "mcp:vault_write", Args: map[string]string{"path": "secret/app", "token": fakeGitlab, "data": fmt.Sprintf(`{"db_password": "pw-%d-abcdefgh"}`, i)}, RawArgs: map[string]bool{"data": true}})
		add(trace.Call{Tool: "shell", Command: fmt.Sprintf("git clone https://deploy:hunter2secret@git.example.com/p%d.git", i)})
		out = append(out, s)
	}
	return out
}

func TestDraftNeverWritesCredentials(t *testing.T) {
	o := DefaultOptions()
	o.Readers = []trace.Reader{fakeReader{sessions: credCorpus()}}
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
			if in.Example != "" || in.Type != trace.SlotSecret {
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
	d.Blocked = redact.ScanArtifacts(d.Files)
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
