package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Verify the published CLI, including its approval callback, on every release
// platform. The only effects are three attempted writes to an owned fixture.
func TestPublishedCLIApprovalCeiling(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	tap := npmTap(t, m, v)
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("limit%d", limit), func(t *testing.T) {
			work := filepath.Join(m.home, fmt.Sprintf("owned-ceiling-%d", limit))
			primitive := filepath.Join(work, "ceiling")
			if err := os.MkdirAll(primitive, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.example, name: approval-ceiling, version: 0.1.0}\nexecution: {entrypoint: main.py}\nfiles:\n  - {path: out, access: write}\n"
			script := "for value in ['one', 'two', 'three']:\n    try:\n        tap.write('out/counter.txt', value)\n    except PermissionError:\n        print('ceiling refused')\nprint('finished')\n"
			for name, body := range map[string]string{"primitive.yaml": manifest, "main.py": script} {
				if err := os.WriteFile(filepath.Join(primitive, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want(t, m.run(work, tap, "--approve", "--limit", fmt.Sprint(limit), "ceiling"), 0, "ceiling refused", "finished")
			b, err := os.ReadFile(filepath.Join(work, "out", "counter.txt"))
			if err != nil {
				t.Fatal(err)
			}
			expected := []string{"one", "two"}[limit-1]
			if string(b) != expected {
				t.Fatalf("published CLI limit%d produced %q, want %q", limit, b, expected)
			}
		})
	}
}
