package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// A fixed vector: the digest is sha256(primitive.yaml || entrypoint), so a
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
	// printf '<primitive.yaml><main.sh>' | shasum -a 256
	if want := "6c49b6066634494183dd894a1c41ad78194a42f3aeae44979b9b85e45b629dec"; got != want {
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
