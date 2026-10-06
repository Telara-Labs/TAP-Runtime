//go:build linux

package launch

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This helper runs the same Go test binary as the existing nobody account.
// It has no shell and only reads the specific owned fixture named by its parent.
func TestSharedOutputReaderProcess(t *testing.T) {
	a := flag.Args()
	if len(a) != 4 || a[0] != "tap-sharing-reader" {
		t.Skip("reader subprocess only")
	}
	if strconv.Itoa(os.Geteuid()) != a[1] {
		t.Fatal("reader did not run as the requested distinct UID")
	}
	b, err := os.ReadFile(a[2])
	if a[3] == "deny" {
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("private output read: %q, %v; want permission denied", b, err)
		}
		fmt.Println("distinct UID: private output denied")
		return
	}
	if err != nil || string(b) != a[3] {
		t.Fatalf("reviewed export read: %q, %v", b, err)
	}
	fmt.Println("distinct UID: reviewed export read")
}

func TestPublishedOutputDeliberateSharing(t *testing.T) {
	v := version(t)
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal("the native acceptance runner needs the existing nobody account:", err)
	}
	if nobody.Uid == strconv.Itoa(os.Geteuid()) {
		t.Fatal("owner and reader must have different UIDs")
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		t.Fatal("native acceptance needs sudo to use the existing nobody account:", err)
	}
	m := newMachine(t)
	tap := npmTap(t, m, v)
	work := t.TempDir()
	primitive := filepath.Join(work, "share-output")
	if err := os.Mkdir(primitive, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.example, name: share-output, version: 0.1.0}\nexecution: {entrypoint: main.py}\nfiles:\n  - {path: out, access: write}\n"
	for name, body := range map[string]string{"primitive.yaml": manifest, "main.py": "tap.write(\"out/reviewed.txt\", \"reviewed fixture only\")\n"} {
		if err := os.WriteFile(filepath.Join(primitive, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want(t, m.run(work, tap, "--approve", "share-output"), 0, "RESULT (exit 0)")
	source := filepath.Join(work, "out", "reviewed.txt")
	assertPrivate := func() {
		t.Helper()
		b, err := os.ReadFile(source)
		if err != nil || string(b) != "reviewed fixture only" {
			t.Fatalf("private source: %q, %v", b, err)
		}
		for path, mode := range map[string]os.FileMode{source: 0o600, filepath.Dir(source): 0o700} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("private source permissions for %s: %v, %v; want %o", path, info, err, mode)
			}
		}
	}
	assertPrivate()
	// Only this explicit reviewed export gets the documented group access.
	shared, err := os.MkdirTemp("", "tap-reviewed-export-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shared) })
	if err := os.Chmod(shared, 0o750); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(shared, "reviewed.txt")
	b, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(export, b, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(export, 0o640); err != nil {
		t.Fatal(err)
	}
	// The test executable is public code; copy it because Go's build directory
	// is private. Do not grant traversal to the owner's work/cache directories.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(shared, "reader.test")
	if err := os.WriteFile(helper, b, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(helper, 0o750); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sudo, "-n", "chgrp", "--", nobody.Gid, shared, export, helper)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set group on owned export only: %v\n%s", err, out)
	}
	readAsNobody := func(path, expectation, marker string) {
		t.Helper()
		cmd := exec.Command(sudo, "-n", "-u", "#"+nobody.Uid, "-g", "#"+nobody.Gid, "--", helper, "-test.run=^TestSharedOutputReaderProcess$", "-test.v", "--", "tap-sharing-reader", nobody.Uid, path, expectation)
		out, err := cmd.CombinedOutput()
		t.Logf("reader UID %s: %s", nobody.Uid, out)
		if err != nil || !strings.Contains(string(out), marker) {
			t.Fatalf("distinct-UID read: %v\n%s", err, out)
		}
	}
	readAsNobody(source, "deny", "private output denied")
	readAsNobody(export, "reviewed fixture only", "reviewed export read")
	assertPrivate()
	info, err := os.Stat(export)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("reviewed export mode: %v, %v", info, err)
	}
}
