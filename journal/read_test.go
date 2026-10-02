package journal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInspectFinishedRunExposesOnlyMetadata(t *testing.T) {
	root := t.TempDir()
	j, err := Create(root, header("run-inspect-finished"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	must(t, j.Begin("req-1", "tools.call", "digest", "write", now))
	must(t, j.End("req-1", "ran", []byte(`{"secret":"do not expose"}`), now))
	must(t, j.Finish("completed", now))
	must(t, j.Close())

	s, err := Inspect(root, "run-inspect-finished", 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != InspectFinished || s.Outcome != "completed" || len(s.Events) != 3 {
		t.Fatalf("unexpected snapshot: %#v", s)
	}
	if got := s.Events[0]; got.Phase != "begin" || got.ID != "req-1" || got.Method != "tools.call" || got.Effect != "write" {
		t.Fatalf("unexpected begin event: %#v", got)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("secret")) || bytes.Contains(b, []byte("blob")) {
		t.Fatalf("unsafe data escaped into snapshot: %s", b)
	}
}

func TestInspectIncompleteAndPartialTrailingRecord(t *testing.T) {
	root := t.TempDir()
	j, err := Create(root, header("run-inspect-partial"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, j.Begin("req-1", "tools.call", "digest", "write", time.Now()))
	must(t, j.Close())
	path := filepath.Join(root, "run-inspect-partial", "index.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"phase":"end","id":"req-1","reply":"cut`); err != nil {
		t.Fatal(err)
	}
	must(t, f.Close())

	s, err := Inspect(root, "run-inspect-partial", 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != InspectInterrupted || !s.Truncated || len(s.Events) != 1 {
		t.Fatalf("partial record was not safely represented: %#v", s)
	}
}

func TestInspectRejectsMalformedCompleteRecordAndUnsafePaths(t *testing.T) {
	root := t.TempDir()
	if _, err := Inspect(root, "../../etc", 1); err == nil {
		t.Fatal("accepted traversal run id")
	}
	dir := filepath.Join(root, "run-inspect-bad")
	must(t, os.Mkdir(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "index.jsonl"), []byte("not json\n"), 0o600))
	if _, err := Inspect(root, "run-inspect-bad", 1); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed complete record error = %v", err)
	}
	outside := t.TempDir()
	must(t, os.Symlink(outside, filepath.Join(root, "run-inspect-link")))
	if _, err := Inspect(root, "run-inspect-link", 1); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestInspectBoundsEventsAndDoesNotMutateFiles(t *testing.T) {
	root := t.TempDir()
	j, err := Create(root, header("run-inspect-bounds"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		must(t, j.Begin(string(rune('a'+i)), "call", "digest", "read", time.Now()))
	}
	must(t, j.Close())
	path := filepath.Join(root, "run-inspect-bounds", "index.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Inspect(root, "run-inspect-bounds", 1)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Events) != 1 || !s.Truncated || s.State != InspectInterrupted || !bytes.Equal(before, after) {
		t.Fatalf("bounded inspect failed: snapshot=%#v changed=%t", s, !bytes.Equal(before, after))
	}
}

func TestInspectReportsLiveLeaseAndBoundsOversizedLines(t *testing.T) {
	root := t.TempDir()
	j, err := Create(root, header("run-inspect-live"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, j.Begin("req-1", "call", "digest", "read", time.Now()))
	s, err := Inspect(root, "run-inspect-live", 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != InspectRunning {
		t.Fatalf("live run state = %q, want %q", s.State, InspectRunning)
	}
	must(t, j.Close())

	path := filepath.Join(root, "run-inspect-live", "index.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(bytes.Repeat([]byte("x"), maxInspectLineBytes+1), '\n')); err != nil {
		t.Fatal(err)
	}
	must(t, f.Close())
	s, err = Inspect(root, "run-inspect-live", 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != InspectUnknown || !s.Truncated {
		t.Fatalf("oversized record was not bounded: %#v", s)
	}
}

func TestInspectBoundsTotalJournalBytes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run-inspect-total")
	must(t, os.Mkdir(dir, 0o700))
	headerLine, err := json.Marshal(Record{Phase: "header", Header: &Header{RunID: "run-inspect-total"}})
	if err != nil {
		t.Fatal(err)
	}
	content := append(headerLine, '\n')
	line := []byte("{\"phase\":\"begin\",\"id\":\"x\",\"method\":\"call\",\"effect\":\"read\",\"at\":\"2026-10-02T00:00:00Z\"}\n")
	for len(content) <= maxInspectTotalBytes {
		content = append(content, line...)
	}
	must(t, os.WriteFile(filepath.Join(dir, "index.jsonl"), content, 0o600))

	s, err := Inspect(root, "run-inspect-total", 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != InspectUnknown || !s.Truncated || len(s.Events) != 1 {
		t.Fatalf("total-byte bound was not reported: %#v", s)
	}
}
