package integration

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover"
	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/routine"

	"gitlab.com/telara-labs/tap-runtime/discover/pack"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestDraftNeverWritesCredentials(t *testing.T) {
	o := discover.DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: testkit.CredCorpus()}}
	rep, err := discover.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	var r *model.Routine
	for i := range rep.Routines {
		if strings.Contains(model.LabelsOf(rep.Routines[i].Candidate), "vault_write") {
			r = &rep.Routines[i]
		}
	}
	if r == nil || routine.RoutineDraft(r) == nil {
		t.Fatalf("no drafted routine: %+v", rep.Funnel)
	}
	d := routine.RoutineDraft(r)
	for name, body := range d.Files {
		for _, secret := range []string{"abcDEF1234567890ghiJKL", testkit.FakeGitlab, "hunter2secret", "pw-"} {
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
	if _, _, err := pack.PackageDraft(d); err != nil {
		t.Fatalf("a clean draft must package: %v", err)
	}
	var buf bytes.Buffer
	routine.WriteFunnel(&buf, rep, 0, true)
	if strings.Contains(buf.String(), "abcDEF1234567890ghiJKL") || strings.Contains(buf.String(), "hunter2secret") {
		t.Fatal("the printed report leaks a credential")
	}
}

func TestLeftoverCredentialBlocksEverything(t *testing.T) {
	d := &model.Draft{Files: map[string][]byte{"main.sh": []byte("curl -H 'Authorization: " + testkit.FakeBearer + "' x\n")}}
	d.Blocked = redact.ScanArtifacts(d.Files)
	if len(d.Blocked) != 1 || strings.Contains(d.Blocked[0], "abcDEF") {
		t.Fatalf("blocked = %v (it must say where, never what)", d.Blocked)
	}
	if _, err := pack.DraftArtifacts(d); !errors.Is(err, model.ErrBlocked) {
		t.Fatalf("artifacts: %v", err)
	}
	if _, _, err := pack.PackageDraft(d); !errors.Is(err, model.ErrBlocked) {
		t.Fatalf("package: %v", err)
	}
	if _, _, err := pack.SaveDraft(d, t.TempDir()); err == nil {
		t.Fatal("save must refuse a blocked draft")
	}
}
