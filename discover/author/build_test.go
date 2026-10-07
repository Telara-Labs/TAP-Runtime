package author

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

func compiledDraft(t *testing.T, command string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: local.test, name: compiled, version: 0.1.0}\nexecution: {entrypoint: main.wasm}\nprovenance:\n  source: source.txt\n  toolchain: test-byte-builder\n  build: " + command + "\n"
	files := map[string]string{"primitive.yaml": manifest, "CHANGELOG.md": "## 0.1.0\n\n- Initial compiled package.\n", "source.txt": "input\n"}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestExplicitBuildProducesFreshReceiptAndRejectsChangedInputs(t *testing.T) {
	// Exercise actual processes and independent rebuilds without requiring a
	// particular compiler for this receipt-freshness unit test.
	dir := compiledDraft(t, `"printf '\\000asm\\001\\000\\000\\000' > main.wasm"`)
	r, err := BuildPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "tap.build/v1" {
		t.Fatal(r)
	}
	if _, err := pack.CheckLifecycle(dir, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source.txt", "main.wasm", "CHANGELOG.md", "primitive.yaml"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := pack.CheckLifecycle(dir, ""); err == nil {
			t.Fatalf("accepted stale %s", name)
		}
		os.WriteFile(filepath.Join(dir, name), b, 0600)
	}
}

func TestBuildRequiresExplicitApprovalAndFailurePreservesArtifact(t *testing.T) {
	dir := compiledDraft(t, `"exit 7"`)
	prior := []byte("previous executable")
	os.WriteFile(filepath.Join(dir, "main.wasm"), prior, 0600)
	var out, errOut bytes.Buffer
	if code := BuildCommand([]string{dir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "nothing built") {
		t.Fatalf("%d %s", code, errOut.String())
	}
	if _, err := BuildPackage(dir); err == nil {
		t.Fatal("failed command accepted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "main.wasm"))
	if !bytes.Equal(prior, after) {
		t.Fatal("failed build changed executable")
	}
	if _, err := os.Stat(filepath.Join(dir, pack.BuildReceiptFile)); !os.IsNotExist(err) {
		t.Fatalf("failed build left receipt: %v", err)
	}
}

func TestSaveCheckNeverRunsBuildRecipe(t *testing.T) {
	dir := compiledDraft(t, `"touch build-ran; exit 1"`)
	os.WriteFile(filepath.Join(dir, "main.wasm"), []byte("\x00asm\x01\x00\x00\x00"), 0600)
	if _, err := pack.CheckLifecycle(dir, ""); err == nil || !strings.Contains(err.Error(), "BUILD.json") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build-ran")); !os.IsNotExist(err) {
		t.Fatal("save executed author build")
	}
}
