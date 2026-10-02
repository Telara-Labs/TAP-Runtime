package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
)

// isolatedHome points HOME and the user config dir at a temp dir, so the
// TAP collection (<config>/tap/primitives) is inside it on every OS.
func isolatedHome(t *testing.T) (home, collection string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	coll, err := pack.CollectionDir()
	if err != nil || !strings.HasPrefix(coll, home) {
		t.Fatalf("collection %s outside the temp home: %v", coll, err)
	}
	return home, coll
}

// writeClaudeChain records n Claude Code sessions that each create an issue
// and comment on the key it returned.
func writeClaudeChain(t *testing.T, home string, n int) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-work")
	os.MkdirAll(dir, 0o755)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("KEY-%d4", i)
		lines := []map[string]any{
			{"type": "user", "timestamp": fmt.Sprintf("2026-09-0%dT09:00:00Z", i+1), "message": map[string]any{"content": "note it on the ticket"}},
			{"type": "assistant", "timestamp": fmt.Sprintf("2026-09-0%dT09:00:01Z", i+1), "message": map[string]any{"id": fmt.Sprint("m", i, "a"), "content": []any{
				map[string]any{"type": "tool_use", "id": fmt.Sprint("a", i), "name": "mcp__jira__issue_create", "input": map[string]any{"summary": fmt.Sprint("follow up ", i)}}}}},
			{"type": "user", "timestamp": fmt.Sprintf("2026-09-0%dT09:00:02Z", i+1), "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprint("a", i), "content": `{"key":"` + key + `"}`}}}},
			{"type": "assistant", "timestamp": fmt.Sprintf("2026-09-0%dT09:00:03Z", i+1), "message": map[string]any{"id": fmt.Sprint("m", i, "b"), "content": []any{
				map[string]any{"type": "tool_use", "id": fmt.Sprint("b", i), "name": "mcp__jira__issue_comment", "input": map[string]any{"issue_key": key, "body": fmt.Sprint("note ", i)}}}}},
			{"type": "user", "timestamp": fmt.Sprintf("2026-09-0%dT09:00:04Z", i+1), "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprint("b", i), "content": `{"ok":true}`}}}},
		}
		var b bytes.Buffer
		for _, l := range lines {
			j, _ := json.Marshal(l)
			b.Write(j)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i)), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// End to end for P2 (TENG-3109): `tap discover --all` reads the history,
// accepts the recurring chain, installs its package once into the TAP
// collection, and writes a pointer only into the detected agent that can
// run it.
func TestDiscoverInstallsOnceAndPointsDetectedAgents(t *testing.T) {
	home, coll := isolatedHome(t)
	writeClaudeChain(t, home, 3)
	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"--all"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errOut.String(), out.String())
	}
	entries, _ := os.ReadDir(coll)
	if len(entries) != 1 {
		t.Fatalf("collection holds %d entries:\n%s", len(entries), out.String())
	}
	name := entries[0].Name()
	pkg := filepath.Join(coll, name)
	if _, err := pack.ReadMarker(pkg); err != nil {
		t.Fatalf("collection entry is not a saved primitive: %v", err)
	}
	pointer := filepath.Join(home, ".claude", "skills", name)
	ref, ok := pack.IsPointer(pointer)
	if !ok || !strings.HasPrefix(ref, "local/") && !strings.Contains(ref, "/"+name+"@") {
		t.Fatalf("no Claude Code pointer at %s", pointer)
	}
	if _, err := os.Stat(filepath.Join(pointer, pack.SavedMarker)); err == nil {
		t.Fatal("the pointer holds the package")
	}
	// Codex is not installed under this home: no pointer, no folder.
	if _, err := os.Stat(filepath.Join(home, ".codex")); err == nil {
		t.Fatal("wrote into an agent that is not installed")
	}
	if !strings.Contains(out.String(), "claude-code: written "+pointer) {
		t.Fatalf("the menu does not report the pointer:\n%s", out.String())
	}
}

// End to end for the migration: a primitive saved as a full package in
// ~/.claude/skills before the collection moves into it, leaving a pointer.
func TestDiscoverMigrateSaved(t *testing.T) {
	home, coll := isolatedHome(t)
	writeClaudeChain(t, home, 3)
	// Save the old way: straight into the skills folder.
	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"--all", "--save-client", "none"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	entries, _ := os.ReadDir(coll)
	if len(entries) != 1 {
		t.Fatalf("%d entries", len(entries))
	}
	name := entries[0].Name()
	old := filepath.Join(home, ".claude", "skills", name)
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(coll, name), old); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := discover.Command([]string{"migrate-saved"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "moved "+old) {
		t.Fatalf("report: %s", out.String())
	}
	if _, err := pack.ReadMarker(filepath.Join(coll, name)); err != nil {
		t.Fatal("not in the collection after migration")
	}
	if _, ok := pack.IsPointer(old); !ok {
		t.Fatal("no pointer left behind")
	}
	out.Reset()
	discover.Command([]string{"migrate-saved"}, strings.NewReader(""), &out, &errOut)
	if strings.TrimSpace(out.String()) != "nothing to migrate" {
		t.Fatalf("second run: %s", out.String())
	}
}

// A save also migrates primitives saved the old way (TENG-3109): after the
// next save, an old full package in ~/.claude/skills is in the collection
// and its folder is a pointer.
func TestSaveMigratesOldSaves(t *testing.T) {
	home, coll := isolatedHome(t)
	writeClaudeChain(t, home, 3)
	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"--all", "--save-client", "none"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	entries, _ := os.ReadDir(coll)
	name := entries[0].Name()
	// Put it back where saves used to go, under another name, as an old save.
	old := filepath.Join(home, ".claude", "skills", "older-save")
	os.MkdirAll(filepath.Dir(old), 0o755)
	if err := os.Rename(filepath.Join(coll, name), old); err != nil {
		t.Fatal(err)
	}
	// Forget the earlier acceptance so the menu installs again.
	os.RemoveAll(filepath.Join(home, ".tap"))
	out.Reset()
	if code := discover.Command([]string{"--all"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if _, err := pack.ReadMarker(filepath.Join(coll, "older-save")); err != nil {
		t.Fatalf("the old save was not migrated:\n%s", out.String())
	}
	if _, ok := pack.IsPointer(old); !ok {
		t.Fatal("no pointer left where the old save was")
	}
	if !strings.Contains(out.String(), "migrated") {
		t.Fatalf("the save does not report the migration:\n%s", out.String())
	}
}

// End to end for TENG-3125: a plain VS Code install is not taken for a
// Copilot user, so it gets no pointer; once Copilot Chat has kept state, the
// save points VS Code's Copilot at the primitive too.
func TestPointersFollowCopilotChatDetection(t *testing.T) {
	home, _ := isolatedHome(t)
	writeClaudeChain(t, home, 3)
	user := filepath.Join(home, "Library", "Application Support", "Code", "User")
	if runtimeGOOS() != "darwin" {
		user = filepath.Join(home, ".config", "Code", "User")
	}
	os.MkdirAll(filepath.Join(user, "globalStorage", "ms-python.python"), 0o755)
	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"--all"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot")); err == nil || strings.Contains(out.String(), "vscode-copilot") {
		t.Fatalf("plain VS Code got a Copilot pointer:\n%s", out.String())
	}
	os.MkdirAll(filepath.Join(user, "globalStorage", "github.copilot-chat"), 0o755)
	os.RemoveAll(filepath.Join(home, ".tap"))
	out.Reset()
	if code := discover.Command([]string{"--all"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "vscode-copilot: written "+filepath.Join(home, ".copilot", "skills")) {
		t.Fatalf("no Copilot pointer once Copilot Chat is there:\n%s", out.String())
	}
}

func runtimeGOOS() string { return runtime.GOOS }
