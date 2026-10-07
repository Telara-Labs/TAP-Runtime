package pack

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLockCrashHelper(t *testing.T) {
	args := os.Args
	if len(args) < 3 || args[len(args)-2] != "tap-lock-crash" {
		return
	}
	f, err := os.OpenFile(args[len(args)-1], os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockSave(f); err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // Deliberately bypass unlock/close to model a crashed saver.
}

func TestLifecycleRecoversInterruptedSaveAndIgnoresHiddenRetention(t *testing.T) {
	root := t.TempDir()
	lock := filepath.Join(root, ".lifecycle.saving-lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSaveLockCrashHelper$", "--", "tap-lock-crash", lock)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	stale := filepath.Join(VersionHistoryDir(root, "dev.test", "lifecycle"), ".retaining-interrupted")
	if err := os.MkdirAll(stale, 0755); err != nil {
		t.Fatal(err)
	}
	dir := lifecyclePackage(t, "0.1.0", "print(1)\n")
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("## 0.1.0\r\n\r\n- Initial revision.\r\n"), 0644)
	if _, _, err := installLifecycle(t, dir, root); err != nil {
		t.Fatal(err)
	}
}

func TestSaveLockExcludesConcurrentSaver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := lockSave(a); err != nil {
		t.Fatal(err)
	}
	defer unlockSave(a)
	b, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := lockSave(b); err == nil {
		unlockSave(b)
		t.Fatal("concurrent save acquired lock")
	}
}

func lifecyclePackage(t *testing.T, version, program string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"primitive.yaml": fmt.Sprintf("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: lifecycle, version: %s}\nexecution: {entrypoint: main.py}\n", version), "main.py": program, "CHANGELOG.md": "# Changelog\n\n## " + version + "\n\n- Tested revision.\n"}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func installLifecycle(t *testing.T, dir, root string) (string, bool, error) {
	t.Helper()
	files := map[string][]byte{}
	items, _ := os.ReadDir(dir)
	for _, i := range items {
		b, err := os.ReadFile(filepath.Join(dir, i.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[i.Name()] = b
	}
	pkg, digest, err := PackFiles(files, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	return InstallVersioned(root, "lifecycle", pkg, Marker{Name: "dev.test/lifecycle", Digest: digest}, "generated pointer")
}

func TestLifecycleRefusesMutableVersionsAndRetainsRollback(t *testing.T) {
	root := t.TempDir()
	v1 := lifecyclePackage(t, "0.1.0", "print(1)\n")
	path, _, err := installLifecycle(t, v1, root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ContentDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(v1, "main.py"), []byte("print(2)\n"), 0o644)
	if _, _, err := installLifecycle(t, v1, root); err == nil || !strings.Contains(err.Error(), "changed under version") {
		t.Fatalf("mutable version accepted: %v", err)
	}
	after, _ := ContentDigest(path)
	if after != before {
		t.Fatal("failed save mutated installed version")
	}
	v2 := lifecyclePackage(t, "0.2.0", "print(2)\n")
	if _, _, err := installLifecycle(t, v2, root); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(VersionHistoryDir(root, "dev.test", "lifecycle"), "0.1.0")
	retained, _ := ContentDigest(old)
	if retained != before {
		t.Fatal("previous exact package was not retained")
	}
	if _, same, err := installLifecycle(t, v2, root); err != nil || !same {
		t.Fatalf("identical revision not idempotent: %v %v", same, err)
	}
	v0 := lifecyclePackage(t, "0.0.9", "print(0)\n")
	if _, _, err := installLifecycle(t, v0, root); err == nil || !strings.Contains(err.Error(), "regresses") {
		t.Fatalf("downgrade accepted: %v", err)
	}
}

func TestLifecycleRequiresVersionedChangelogBeforeMutation(t *testing.T) {
	for _, body := range []string{"", "## 0.0.9\n- Wrong version\n", "## 0.1.0\n\n"} {
		dir := lifecyclePackage(t, "0.1.0", "print(1)\n")
		os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(body), 0o644)
		if _, err := CheckLifecycle(dir, ""); err == nil {
			t.Fatalf("missing change accepted: %q", body)
		}
	}
	dir := lifecyclePackage(t, "latest", "print(1)\n")
	if _, err := CheckLifecycle(dir, ""); err == nil {
		t.Fatal("non-semantic version accepted")
	}
}
