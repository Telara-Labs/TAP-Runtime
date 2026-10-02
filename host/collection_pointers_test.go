package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	agents "gitlab.com/telara-labs/tap-runtime/discover/client"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
)

// End to end for TENG-3109 through the runner's MCP server: discover saves a
// primitive once into the TAP collection and points three agents at it. The
// runner lists it once, under the ref and digest the pointers name, and runs
// it by that identity. A primitive saved the old way (a full package in
// ~/.claude/skills) is listed once before and after migration.
func TestSavedOncePointedEverywhereListedOnceAndRunnable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := startServer(t, true, accept)
	cfg, _ := userConfigDir()
	coll := filepath.Join(cfg, "tap", "primitives")

	files := map[string][]byte{
		"primitive.yaml": []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.local, name: greeting, version: 1.0.0, description: Greet a person}\nexecution: {entrypoint: main.sh}\n"),
		"main.sh":        []byte("echo saved-once-output\n"),
	}
	archive, digest, err := pack.PackFiles(files, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := pack.Install(coll, "greeting", archive, pack.Marker{Name: "dev.local/greeting", Digest: digest, Validation: "not_run"}, "# greeting\n")
	if err != nil {
		t.Fatal(err)
	}
	var targets []pack.Target
	for _, id := range []string{"claude-code", "codex", "gemini-cli"} {
		cl, _ := agents.Lookup(id)
		targets = append(targets, pack.Target{Client: cl})
	}
	ptrs, err := pack.WritePointers(saved, targets, false, home, "")
	if err != nil || len(ptrs) != 3 {
		t.Fatalf("pointers %+v %v", ptrs, err)
	}

	search := func() []any {
		t.Helper()
		res := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "greet"}}))
		m, _ := res["matches"].([]any)
		return m
	}
	matches := search()
	if len(matches) != 1 {
		t.Fatalf("listed %d times with pointers in three agents: %#v", len(matches), matches)
	}
	match := matches[0].(map[string]any)
	// What each pointer tells its agent is exactly what the runner accepts.
	for _, p := range ptrs {
		b, _ := os.ReadFile(filepath.Join(p.Path, "SKILL.md"))
		named := regexp.MustCompile(`ref:\s+(\S+)\n\s+digest:\s+(\S+)`).FindStringSubmatch(string(b))
		if len(named) != 3 || named[1] != match["ref"] || named[2] != match["digest"] {
			t.Fatalf("%s pointer names %v; the runner lists %v %v", p.Client, named, match["ref"], match["digest"])
		}
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"ref": match["ref"], "digest": match["digest"]}})
	if run["isError"] == true || !strings.Contains(toolText(t, run), "saved-once-output") {
		t.Fatalf("run by the pointer's identity: %#v", run)
	}

	// The old way: a full package in ~/.claude/skills, found once, then
	// migrated into the collection and still found once.
	oldFiles := map[string][]byte{
		"primitive.yaml": []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.local, name: greet-old, version: 1.0.0, description: Greet the old way}\nexecution: {entrypoint: main.sh}\n"),
		"main.sh":        []byte("echo old\n"),
	}
	oldArchive, oldDigest, _ := pack.PackFiles(oldFiles, func(string) bool { return false })
	if _, _, err := pack.Install(filepath.Join(home, ".claude", "skills"), "greet-old", oldArchive, pack.Marker{Name: "dev.local/greet-old", Digest: oldDigest, Validation: "not_run"}, "# greet-old\n"); err != nil {
		t.Fatal(err)
	}
	if n := len(search()); n != 2 {
		t.Fatalf("before migration: %d matches, want 2", n)
	}
	moved, err := pack.MigrateSaved(home, "", coll)
	if err != nil || len(moved) != 1 || moved[0].Mode != "moved" {
		t.Fatalf("migrate %+v %v", moved, err)
	}
	if n := len(search()); n != 2 {
		t.Fatalf("after migration: %d matches, want 2", n)
	}
}
