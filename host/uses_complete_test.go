package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// installAt puts a copy of a package where the local catalog looks.
func installAt(t *testing.T, cfg, src, name string) {
	t.Helper()
	dst := filepath.Join(cfg, "tap", "primitives", name)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"primitive.yaml", "main.wasm"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dst, f), b, 0o600)
	}
}

func TestManifestCompleteFillsWhatUsesLeaveOut(t *testing.T) {
	inDir(t)
	cfg := t.TempDir()
	old := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = old })

	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesDraft}.write(t)
	installAt(t, cfg, kid, "kid")
	digest, _, _ := packageDigest(kid)

	pkg := pkgSpec{name: "boss", rest: "uses:\n  - {alias: helper, ref: dev.test/kid}\n"}.write(t)
	var out, errb bytes.Buffer
	if code := manifestCommand([]string{"complete", "--write", pkg}, &out, &errb); code != 0 {
		t.Fatalf("complete exited %d: %s", code, errb.String())
	}
	m, err := mf.Load(pkg)
	if err != nil {
		t.Fatal(err)
	}
	u := m.Uses[0]
	if u.Version != "0.1.0" || u.Digest != digest || u.Effect != "write" {
		t.Fatalf("the entry was filled as %+v, want version 0.1.0, digest %s, effect write", u, digest)
	}
	if p := m.RunProblems(); len(p) != 0 {
		t.Fatalf("the completed manifest cannot run: %v", p)
	}

	// What is already written is never replaced.
	pkg2 := pkgSpec{name: "boss2", rest: "uses:\n  - {alias: helper, ref: dev.test/kid, effect: destructive}\n"}.write(t)
	if code := manifestCommand([]string{"complete", "--write", pkg2}, &out, &errb); code != 0 {
		t.Fatalf("complete exited %d: %s", code, errb.String())
	}
	if m2, _ := mf.Load(pkg2); m2.Uses[0].Effect != "destructive" || m2.Uses[0].Digest != digest {
		t.Fatalf("a written effect was replaced: %+v", m2.Uses[0])
	}

	// A ref that names two installed primitives is refused with both named.
	other := pkgSpec{name: "kid", rest: "files:\n  - {path: out, access: write}\n"}.write(t)
	installAt(t, cfg, other, "kid-two")
	otherDigest, _, _ := packageDigest(other)
	pkg3 := pkgSpec{name: "boss3", rest: "uses:\n  - {alias: helper, ref: dev.test/kid}\n"}.write(t)
	errb.Reset()
	if code := manifestCommand([]string{"complete", "--write", pkg3}, &out, &errb); code == 0 {
		t.Fatal("an ambiguous ref was completed")
	}
	for _, want := range []string{"matches 2 installed primitives", shortDigest(digest), shortDigest(otherDigest)} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("the refusal does not say %q: %s", want, errb.String())
		}
	}
	// Naming the digest settles it.
	pkg4 := pkgSpec{name: "boss4", rest: "uses:\n  - {alias: helper, ref: dev.test/kid, digest: " + otherDigest + "}\n"}.write(t)
	errb.Reset()
	if code := manifestCommand([]string{"complete", "--write", pkg4}, &out, &errb); code != 0 {
		t.Fatalf("a named digest was refused: %s", errb.String())
	}
	if m4, _ := mf.Load(pkg4); m4.Uses[0].Digest != otherDigest || m4.Uses[0].Effect != "write" {
		t.Fatalf("got %+v", m4.Uses[0])
	}

	// A ref that is not installed is refused.
	pkg5 := pkgSpec{name: "boss5", rest: "uses:\n  - {alias: helper, ref: dev.test/nothing}\n"}.write(t)
	errb.Reset()
	if code := manifestCommand([]string{"complete", pkg5}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "not installed") {
		t.Fatalf("a missing ref was not refused: %q", errb.String())
	}
}
