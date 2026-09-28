package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const repo = ".."

var host = runtime.GOOS + "/" + runtime.GOARCH

func build(t *testing.T, key string) string {
	t.Helper()
	out := t.TempDir()
	if err := Build(repo, out, "0.0.0-test", []string{host}, key); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAReleaseIsReproducible(t *testing.T) {
	a, b := build(t, ""), build(t, "")
	x, _ := os.ReadFile(filepath.Join(a, "SHA256SUMS"))
	y, _ := os.ReadFile(filepath.Join(b, "SHA256SUMS"))
	if string(x) != string(y) || len(x) == 0 {
		t.Fatalf("two builds of the same source differ:\n%s\n%s", x, y)
	}
	for _, want := range []string{"tap-runtime-0.0.0-test-", "sh-0.0.0-test.wasm", "THIRD_PARTY_NOTICES.txt"} {
		if !strings.Contains(string(x), want) {
			t.Errorf("the checksums do not list %s:\n%s", want, x)
		}
	}
}

func TestVerify(t *testing.T) {
	keys := filepath.Join(t.TempDir(), "k")
	if err := Keygen(keys); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	Keygen(other)

	out := build(t, keys+".key")
	if err := Verify(out, keys+".pub"); err != nil {
		t.Fatalf("a good release did not verify: %v", err)
	}
	if err := Verify(out, other+".pub"); err == nil {
		t.Error("a release verified against a key that did not sign it")
	}

	entries, _ := os.ReadDir(out)
	var binary string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tap-runtime-") {
			binary = filepath.Join(out, e.Name())
		}
	}
	raw, _ := os.ReadFile(binary)
	os.WriteFile(binary, append(raw, 0), 0o755)
	if err := Verify(out, keys+".pub"); err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Errorf("an altered binary verified: %v", err)
	}
	os.WriteFile(binary, raw, 0o755)

	// Altering the checksums to match an altered file breaks the signature.
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	os.WriteFile(filepath.Join(out, "SHA256SUMS"), append(sums, []byte("0000  extra\n")...), 0o644)
	if err := Verify(out, keys+".pub"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("altered checksums verified: %v", err)
	}
	os.WriteFile(filepath.Join(out, "SHA256SUMS"), sums, 0o644)

	os.WriteFile(filepath.Join(out, "planted"), []byte("x"), 0o644)
	if err := Verify(out, keys+".pub"); err == nil || !strings.Contains(err.Error(), "not in its checksums") {
		t.Errorf("a file nobody listed was accepted: %v", err)
	}
	os.Remove(filepath.Join(out, "planted"))

	os.Remove(filepath.Join(out, "SHA256SUMS.sig"))
	if err := Verify(out, keys+".pub"); err == nil {
		t.Error("an unsigned release verified when a signature was required")
	}
	if err := Verify(out, ""); err != nil {
		t.Errorf("checksums alone did not verify: %v", err)
	}
}

// The notices are derived from what is compiled in. Anything compiled in
// without a licence file stops the release.
func TestNoticesCoverWhatIsCompiledIn(t *testing.T) {
	n, err := Notices(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github.com/tetratelabs/wazero", "github.com/itchyny/gojq", "github.com/evanw/esbuild",
		"github.com/santhosh-tekuri/jsonschema", "gopkg.in/yaml.v3", "mvdan.cc/sh/v3 v3.14.1, modified"} {
		if !strings.Contains(string(n), want) {
			t.Errorf("the notices do not cover %s", want)
		}
	}
	for _, never := range []string{"GNU GENERAL PUBLIC LICENSE", "GNU AFFERO", "GNU LESSER"} {
		if strings.Contains(string(n), never) {
			t.Errorf("something compiled in is under the %s", never)
		}
	}
}
