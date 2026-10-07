package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManifestSnapshotIntegrityAndLegacy(t *testing.T) {
	root := t.TempDir()
	h := header("run-snapshot-identity")
	raw := []byte("exact bytes\n")
	j, err := CreateWithManifest(root, h, raw)
	if err != nil {
		t.Fatal(err)
	}
	must(t, j.BeginCall("r1", "call", "request-hash", "write", &CallIdentity{Alias: "save", Server: "host", Tool: "save_record"}, time.Now()))
	must(t, j.End("r1", "refused", []byte(`{"secret":"private"}`), time.Now()))
	must(t, j.Close())
	s, err := Inspect(root, h.RunID, 100)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(root, s.Header)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("snapshot %q, %v", got, err)
	}
	if s.Events[0].Call.Tool != "save_record" || s.Events[1].Outcome != "refused" {
		t.Fatalf("trace %#v", s.Events)
	}
	path := filepath.Join(root, h.RunID, "blobs", s.Header.ManifestDigest)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private mode %v, %v", info, err)
	}
	// Resume uses the original header and does not replace saved content.
	j, err = Open(root, h.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Header.ManifestDigest != s.Header.ManifestDigest {
		t.Fatal("resume lost snapshot")
	}
	must(t, j.Close())
	must(t, os.WriteFile(path, []byte("altered bytes"), 0600))
	if _, err := ReadManifest(root, s.Header); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
	must(t, os.Remove(path))
	outside := filepath.Join(t.TempDir(), "manifest")
	must(t, os.WriteFile(outside, raw, 0600))
	must(t, os.Symlink(outside, path))
	if _, err := ReadManifest(root, s.Header); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("symlink accepted %v", err)
	}
	legacy, err := Create(root, header("run-snapshot-legacy"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, legacy.Close())
	if got, err := ReadManifest(root, legacy.Header); got != nil || err != nil {
		t.Fatalf("legacy fabricated snapshot %q %v", got, err)
	}
}

func TestCallIdentityBoundedWithoutChangingReplay(t *testing.T) {
	root := t.TempDir()
	j, err := Create(root, header("run-identity-bound"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, j.BeginCall("r1", "call", "same-hash", "read", &CallIdentity{Alias: "lookup", Tool: strings.Repeat("x", 10000)}, time.Now()))
	must(t, j.End("r1", "ran", []byte(`{"ok":true}`), time.Now()))
	must(t, j.Close())
	j, err = Open(root, j.Header.RunID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := j.Lookup("r1", "same-hash")
	if state != Finished || err != nil {
		t.Fatalf("replay changed %v %v", state, err)
	}
	must(t, j.Close())
	s, err := Inspect(root, j.Header.RunID, 10)
	if err != nil || !s.Events[0].Call.Truncated || s.Events[0].Call.Alias != "lookup" || s.Events[0].Call.Tool != "" {
		t.Fatalf("unbounded identity %#v %v", s, err)
	}
}
