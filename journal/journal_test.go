package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func header(id string) Header {
	return Header{RunID: id, Package: "p", PackageDigest: "d", Started: time.Unix(1_800_000_000, 0).UTC()}
}

func TestARunContinuesFromItsRecord(t *testing.T) {
	root := t.TempDir()
	j, err := Create(root, header("run-000001"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	big := bytes.Repeat([]byte("x"), InlineLimit+1)
	must(t, j.Begin("r1", "call", "d1", "read", now))
	must(t, j.End("r1", "ran", []byte(`{"result":"small"}`), now))
	must(t, j.Begin("r2", "call", "d2", "read", now))
	must(t, j.End("r2", "ran", big, now))
	must(t, j.Begin("r3", "exec", "d3", "write", now)) // and the machine loses power
	j.Close()

	j, err = Open(root, "run-000001")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for _, c := range []struct {
		id, digest string
		state      State
		reply      string
	}{
		{"r1", "d1", Finished, `{"result":"small"}`},
		{"r2", "d2", Finished, string(big)},
		{"r3", "d3", Interrupted, ""},
		{"r4", "d4", Unseen, ""},
	} {
		state, reply, err := j.Lookup(c.id, c.digest)
		if err != nil || state != c.state || string(reply) != c.reply {
			t.Errorf("%s: state %v, %d bytes, err %v", c.id, state, len(reply), err)
		}
	}
	if j.Effect("r3") != "write" {
		t.Error("the effect of an interrupted request is not known on resume")
	}
	if f, i := j.Counts(); f != 2 || i != 1 {
		t.Errorf("counts %d finished, %d interrupted", f, i)
	}
}

// Keyed by id, not by order: the second run may ask in any order.
func TestLookupDoesNotDependOnOrder(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-000002"))
	now := time.Now()
	// Three requests in flight at once; answers arrive out of order.
	for _, id := range []string{"a", "b", "c"} {
		must(t, j.Begin(id, "call", "d-"+id, "read", now))
	}
	for _, id := range []string{"c", "a", "b"} {
		must(t, j.End(id, "ran", []byte(`"`+id+`"`), now))
	}
	j.Close()
	j, err := Open(root, "run-000002")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for _, id := range []string{"b", "c", "a"} {
		state, reply, err := j.Lookup(id, "d-"+id)
		if err != nil || state != Finished || string(reply) != `"`+id+`"` {
			t.Errorf("%s: got %s, state %v, err %v", id, reply, state, err)
		}
	}
}

func TestAChangedRequestIsNoticed(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-000003"))
	defer j.Close() // Windows does not remove a file that is open
	must(t, j.Begin("r1", "call", Digest(map[string]any{"alias": "search", "q": "a"}), "read", time.Now()))
	must(t, j.End("r1", "ran", []byte(`1`), time.Now()))
	if _, _, err := j.Lookup("r1", Digest(map[string]any{"alias": "search", "q": "b"})); !errors.Is(err, ErrChanged) {
		t.Fatalf("a different request under a known id was answered from the record: %v", err)
	}
	if _, _, err := j.Lookup("r1", Digest(map[string]any{"q": "a", "alias": "search"})); err != nil {
		t.Fatalf("the same request written in another order was refused: %v", err)
	}
}

func TestALineCutShortIsIgnored(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-000004"))
	must(t, j.Begin("r1", "call", "d1", "write", time.Now()))
	j.Close()
	f, _ := os.OpenFile(filepath.Join(root, "run-000004", "index.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"phase":"end","id":"r1","outcome":"ran","reply":"tr`) // power lost mid-write
	f.Close()
	j, err := Open(root, "run-000004")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if state, _, _ := j.Lookup("r1", "d1"); state != Interrupted {
		t.Fatalf("a half-written answer was taken as an answer: state %v", state)
	}
}

func TestAnAlteredResultIsRefused(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-000005"))
	defer j.Close() // Windows does not remove a file that is open
	big := bytes.Repeat([]byte("y"), InlineLimit*2)
	must(t, j.Begin("r1", "call", "d1", "read", time.Now()))
	must(t, j.End("r1", "ran", big, time.Now()))
	entries, _ := os.ReadDir(filepath.Join(root, "run-000005", "blobs"))
	if len(entries) != 1 {
		t.Fatalf("%d blobs", len(entries))
	}
	os.WriteFile(filepath.Join(root, "run-000005", "blobs", entries[0].Name()), []byte("tampered"), 0o600)
	if _, _, err := j.Lookup("r1", "d1"); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Fatalf("an altered result was replayed: %v", err)
	}
}

func TestTheSameLargeResultIsStoredOnce(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-000006"))
	defer j.Close() // Windows does not remove a file that is open
	big := bytes.Repeat([]byte("z"), InlineLimit*3)
	for _, id := range []string{"r1", "r2", "r3"} {
		must(t, j.Begin(id, "call", "d", "read", time.Now()))
		must(t, j.End(id, "ran", big, time.Now()))
	}
	entries, _ := os.ReadDir(filepath.Join(root, "run-000006", "blobs"))
	if len(entries) != 1 {
		t.Fatalf("%d blobs for one distinct result", len(entries))
	}
	info, _ := os.Stat(filepath.Join(root, "run-000006", "index.jsonl"))
	if info.Size() > 4096 {
		t.Fatalf("the index grew to %d bytes; large results are leaking into it", info.Size())
	}
}

func TestAFinishedRunCannotBeResumed(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-000007"))
	must(t, j.Finish("completed", time.Now()))
	j.Close()
	if _, err := Open(root, "run-000007"); err == nil {
		t.Fatal("a finished run was opened for resume")
	}
	if _, err := Create(root, header("run-000007")); err == nil {
		t.Fatal("a run id was used twice")
	}
	if _, err := Open(root, "../../etc"); err == nil {
		t.Fatal("a run id that is a path was accepted")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A run that another run started names it, and what it returned is kept in
// its finish record for a parent that stopped before it heard.
func TestAChildRunKeepsWhatItReturned(t *testing.T) {
	root := t.TempDir()
	parent, err := Create(root, header("run-parent-001"))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BeginChild("r1", "run", "digest", "write", "run-child-0001", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := parent.Child("r1"); got != "run-child-0001" {
		t.Fatalf("the begin record names %q", got)
	}
	h := header("run-child-0001")
	h.ParentRunID, h.ParentRequestID = "run-parent-001", "r1"
	child, err := Create(root, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, finished, err := FinishedReply(root, "run-child-0001"); err != nil || finished {
		t.Fatalf("an unfinished run reads as finished: %v %v", finished, err)
	}
	big := make([]byte, InlineLimit+10)
	for i := range big {
		big[i] = 'a'
	}
	want := `{"stdout":"` + string(big) + `"}`
	if err := child.FinishWith("completed", []byte(want), time.Now()); err != nil {
		t.Fatal(err)
	}
	child.Close()
	reply, outcome, finished, err := FinishedReply(root, "run-child-0001")
	if err != nil || !finished || outcome != "completed" || string(reply) != want {
		t.Fatalf("the finish record gave back %d bytes, %q, finished=%v, %v", len(reply), outcome, finished, err)
	}
	// A run that finished without keeping anything says so.
	plain, err := Create(root, header("run-plain-0001"))
	if err != nil {
		t.Fatal(err)
	}
	plain.Finish("completed", time.Now())
	plain.Close()
	reply, _, finished, err = FinishedReply(root, "run-plain-0001")
	if err != nil || !finished || len(reply) != 0 {
		t.Fatalf("an older record: %d bytes, finished=%v, %v", len(reply), finished, err)
	}
	// And the parent's record, reopened, still names the child.
	parent.Close()
	reopened, err := Open(root, "run-parent-001")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Child("r1") != "run-child-0001" {
		t.Fatalf("the link was lost on reopening")
	}
}
