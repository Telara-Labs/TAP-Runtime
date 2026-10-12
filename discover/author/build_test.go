package author

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

func TestBuildRefusesCompilerSourceAndDependencyChanges(t *testing.T) {
	for _, name := range []string{"source.txt", "go.mod", "go.sum"} {
		t.Run(name, func(t *testing.T) {
			command, _ := json.Marshal("printf '\\000asm\\001\\000\\000\\000' > main.wasm; printf changed > " + name)
			dir := compiledDraft(t, string(command))
			prior := []byte("prior executable")
			os.WriteFile(filepath.Join(dir, "main.wasm"), prior, 0600)
			os.WriteFile(filepath.Join(dir, pack.BuildReceiptFile), []byte("prior receipt"), 0600)
			if _, err := BuildPackage(dir); err == nil || !strings.Contains(err.Error(), "modified source or dependency") {
				t.Fatalf("%v", err)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "main.wasm"))
			receipt, _ := os.ReadFile(filepath.Join(dir, pack.BuildReceiptFile))
			if !bytes.Equal(prior, after) || string(receipt) != "prior receipt" {
				t.Fatal("failed build changed prior artifact/receipt")
			}
		})
	}
}

func TestRebuildUsesIdenticalSnapshotWithoutPriorReceipt(t *testing.T) {
	dir := compiledDraft(t, `"test ! -e BUILD.json && printf '\\000asm\\001\\000\\000\\000' > main.wasm"`)
	if _, err := BuildPackage(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPackage(dir); err != nil {
		t.Fatalf("rebuild with prior receipt: %v", err)
	}
}

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

func TestBuildRecordsTheArtifactFormat(t *testing.T) {
	for _, c := range []struct{ preamble, want string }{
		{`\\000asm\\001\\000\\000\\000`, pack.FormatModule},
		{`\\000asm\\015\\000\\001\\000`, pack.FormatComponent},
	} {
		t.Run(c.want, func(t *testing.T) {
			dir := compiledDraft(t, `"printf '`+c.preamble+`' > main.wasm"`)
			r, err := BuildPackage(dir)
			if err != nil {
				t.Fatal(err)
			}
			if r.Format != c.want {
				t.Fatalf("format %q, want %q", r.Format, c.want)
			}
			b, _ := os.ReadFile(filepath.Join(dir, pack.BuildReceiptFile))
			if !strings.Contains(string(b), `"format": "`+c.want+`"`) {
				t.Fatalf("BUILD.json does not record the format:\n%s", b)
			}
			if _, err := pack.CheckLifecycle(dir, ""); err != nil {
				t.Fatal(err)
			}
			// A receipt whose format does not match the artifact is stale.
			var other pack.BuildReceipt
			json.Unmarshal(b, &other)
			other.Format = map[string]string{pack.FormatModule: pack.FormatComponent, pack.FormatComponent: pack.FormatModule}[c.want]
			wrong, _ := json.Marshal(other)
			os.WriteFile(filepath.Join(dir, pack.BuildReceiptFile), wrong, 0600)
			if _, err := pack.CheckLifecycle(dir, ""); err == nil {
				t.Fatal("accepted a receipt for the other format")
			}
			// Receipts written before the field existed describe modules only.
			other.Format = ""
			old, _ := json.Marshal(other)
			os.WriteFile(filepath.Join(dir, pack.BuildReceiptFile), old, 0600)
			_, err = pack.CheckLifecycle(dir, "")
			if (err == nil) != (c.want == pack.FormatModule) {
				t.Fatalf("receipt without a format, artifact %s: %v", c.want, err)
			}
		})
	}
}

func TestBuildRefusesAnArtifactThatIsNotWebAssembly(t *testing.T) {
	dir := compiledDraft(t, `"printf '\\000asm\\014\\000\\001\\000' > main.wasm"`)
	if _, err := BuildPackage(dir); err == nil || !strings.Contains(err.Error(), "module or component") {
		t.Fatalf("%v", err)
	}
}

// The Rust example's real build recipe, run twice by the author build, must
// reproduce the same component and record its format.
func TestTheRustComponentExampleBuildsReproducibly(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the example's build script needs it")
	}
	rustc, err := exec.Command("rustup", "which", "rustc").Output()
	if err != nil {
		path, lerr := exec.LookPath("rustc")
		if lerr != nil {
			t.Skip("rustc is not installed; install Rust with the wasm32-wasip2 target to run this test")
		}
		rustc = []byte(path)
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "examples", "languages", "rust"))); err != nil {
		t.Fatal(err)
	}
	for _, generated := range []string{"main.wasm", pack.BuildReceiptFile} {
		os.Remove(filepath.Join(dir, generated))
	}
	cfg, _ := json.Marshal(map[string]string{"rustc": strings.TrimSpace(string(rustc))})
	os.WriteFile(filepath.Join(dir, "compiler.json"), cfg, 0o644)
	r, err := BuildPackage(dir)
	if err != nil {
		if strings.Contains(err.Error(), "can't find crate for `std`") {
			t.Skip("the Rust wasm32-wasip2 target is not installed (rustup target add wasm32-wasip2)")
		}
		t.Fatal(err)
	}
	if r.Format != pack.FormatComponent {
		t.Fatalf("%+v", r)
	}
	if _, err := pack.CheckLifecycle(dir, ""); err != nil {
		t.Fatal(err)
	}
}
