package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func exampleDirs(t *testing.T) (parent string, kids []string) {
	t.Helper()
	ex := filepath.Join(repoRoot, "examples")
	return filepath.Join(ex, "release-check"), []string{filepath.Join(ex, "api-release-brief"), filepath.Join(ex, "languages", "python")}
}

// The example pins its two children by digest. When either changes, this says
// so, and tap manifest complete refreshes the pin.
func TestReleaseCheckPinsTheExamplesItUses(t *testing.T) {
	parent, kids := exampleDirs(t)
	digest, m, err := packageDigest(parent)
	if err != nil {
		t.Fatal(err)
	}
	if p := m.RunProblems(); len(p) != 0 {
		t.Fatalf("the example cannot run: %v", p)
	}
	if len(m.Uses) != 2 {
		t.Fatalf("the example uses %d primitives, want 2", len(m.Uses))
	}
	var entries []catalogEntry
	for _, k := range kids {
		e, err := readCatalogEntry(k, "test")
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	nodes, err := resolveUses(entries, m, digest)
	if err != nil {
		t.Fatalf("the pins no longer match the examples they name (run tap manifest complete): %v", err)
	}
	for _, n := range nodes {
		if n.Effect != "read" {
			t.Errorf("%s can %s; the example declares it read", n.Alias, n.Effect)
		}
	}
	if len(m.Fetch)+len(m.Files)+len(m.Commands)+len(m.Tools) != 0 {
		t.Fatal("the composed primitive declares authority of its own; it should declare none")
	}
}

func TestReleaseCheckRunsItsTwoChildren(t *testing.T) {
	dir, err := storeDir("")
	if err != nil {
		t.Skip(err)
	}
	if _, _, _, err := obtain(dir, "main.py"); err != nil {
		t.Skipf("the Python interpreter could not be obtained: %v", err)
	}
	work := inDir(t)
	fixture, err := os.ReadFile(filepath.Join(repoRoot, "examples", "languages", "fixtures", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(work, "examples", "languages", "fixtures"), 0o755)
	os.WriteFile(filepath.Join(work, "examples", "languages", "fixtures", "tasks.json"), fixture, 0o600)
	parent, kids := exampleDirs(t)
	var entries []catalogEntry
	for _, k := range kids {
		e, err := readCatalogEntry(k, "test")
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	in, _ := json.Marshal(map[string]string{"owner": "Telara-Labs", "repo": "TAP-Runtime", "ref": "main"})
	res, err := Run(context.Background(), Options{Package: parent, Args: []string{string(in)}, Journal: io.Discard, RunsDir: t.TempDir(), CacheDir: sharedCompileCache(t),
		catalog: func() ([]catalogEntry, error) { return entries, nil }, Approve: approveAll(t)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != 0 {
		if strings.Contains(res.Stderr, "HTTP") || strings.Contains(res.Stderr, "fetch") {
			t.Skipf("GitHub's API could not be read from here: %s", res.Stderr)
		}
		t.Fatalf("exit %d: %s", res.Exit, res.Stderr)
	}
	var out struct {
		Ready     bool     `json:"ready"`
		Reasons   []string `json:"reasons"`
		OpenTasks int      `json:"open_tasks"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		t.Fatalf("%q: %v", res.Stdout, err)
	}
	if out.OpenTasks != 2 || out.Ready || len(res.Children) != 2 {
		t.Fatalf("got %+v with %d children; the fixture has two open tasks, so it cannot be ready", out, len(res.Children))
	}
}
