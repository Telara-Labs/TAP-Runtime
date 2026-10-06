//go:build darwin || linux

package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedUnixOutputIsPrivate(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	tap := npmTap(t, m, v)
	work := t.TempDir()
	primitive := filepath.Join(work, "private-output")
	if err := os.Mkdir(primitive, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.example, name: private-output, version: 0.1.0}\nexecution: {entrypoint: main.py}\nfiles:\n  - {path: out, access: write}\n"
	for name, body := range map[string]string{"primitive.yaml": manifest, "main.py": "tap.write(\"out/deep/note.txt\", \"owned private fixture\")\n"} {
		if err := os.WriteFile(filepath.Join(primitive, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Let the actual published runner create both directories and the output.
	want(t, m.run(work, tap, "--approve", "private-output"), 0, "RESULT (exit 0)")
	output := filepath.Join(work, "out", "deep", "note.txt")
	b, err := os.ReadFile(output)
	if err != nil || string(b) != "owned private fixture" {
		t.Fatalf("same-owner output read: %q, %v", b, err)
	}
	for path, mode := range map[string]os.FileMode{
		output:                     0o600,
		filepath.Dir(output):       0o700,
		filepath.Join(work, "out"): 0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("published output permissions for %s: %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
}
