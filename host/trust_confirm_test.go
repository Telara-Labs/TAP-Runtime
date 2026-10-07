package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// Kilo ran tap trust from its shell to approve its own primitive. Without a
// terminal to ask the person on, tap trust refuses and trusts nothing.
func TestTapTrustRefusesWithoutATerminal(t *testing.T) {
	inDir(t)
	oldTTY, oldConfirm := ttyPath, confirmTrust
	t.Cleanup(func() { ttyPath, confirmTrust = oldTTY, oldConfirm })
	ttyPath = filepath.Join(t.TempDir(), "no-terminal")
	confirmTrust = askAtTerminal
	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: self-trust, version: 1.0.0}\nexecution: {entrypoint: main.sh}\nfetch:\n  - {origin: https://example.com}\n", "echo hi\n")
	var out, errOut bytes.Buffer
	if code := trustCommand([]string{pkg}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "your own terminal") {
		t.Fatalf("trusted without a terminal: %d %s", code, errOut.String())
	}
	if newTrustStore().has(mustDigest(t, pkg)) {
		t.Fatal("a refused tap trust kept the package")
	}
}

func mustDigest(t *testing.T, pkg string) string {
	t.Helper()
	d, _, err := packageDigest(pkg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
