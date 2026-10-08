package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/author"
)

// authoredDraft is a package an agent wrote outside the collection, with the
// AUTHORING.json the tap-author skill has it write.
func authoredDraft(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "release-check")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	contract := map[string]author.BriefField{}
	for _, f := range author.ContractFields {
		contract[f] = author.BriefField{Value: "the " + f, EstablishedBy: "the earlier request and its calls"}
	}
	a := author.Authoring{Kind: "tap.authoring/v1", Name: "release-check", Publisher: "local.me", Author: "host-agent",
		Agent: "codex", Selection: author.SelectedTask, Sources: []string{"src_0123456789ab"}, BriefDigest: "sha256:00",
		Contract: contract, Interface: json.RawMessage(`{"args":[]}`)}
	b, _ := json.Marshal(a)
	files := map[string]string{
		"AUTHORING.json": string(b),
		"primitive.yaml": "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: local.me, name: release-check, version: 0.1.0, description: Check a release candidate}\nexecution: {entrypoint: main.sh}\n",
		"main.sh":        "echo saved-and-ran\n",
		"CHANGELOG.md":   "## 0.1.0\n\n- Initial authored release check.\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func saveHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// Found testing Codex: its shell could not write the collection (read-only
// file system), so a primitive it wrote could never be saved. tap_save saves
// it from the server, after the person agrees, and tap_search then finds it.
func TestSaveToolSavesAfterThePersonAgrees(t *testing.T) {
	saveHome(t)
	c := startServer(t, true, accept)
	// Saving and searching read one collection, as outside tests.
	userConfigDir = os.UserConfigDir
	draft := authoredDraft(t)
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": draft}})
	if res["isError"] == true || !strings.Contains(toolText(t, res), "saved") {
		t.Fatalf("save = %#v", res)
	}
	found := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "release candidate"}}))
	if m, _ := found["matches"].([]any); len(m) != 1 {
		t.Fatalf("saved primitive not found: %#v", found)
	}
}

func TestSaveToolSavesNothingWithoutAYes(t *testing.T) {
	saveHome(t)
	c := startServer(t, true, decline)
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": authoredDraft(t)}})
	if res["isError"] != true || !strings.Contains(toolText(t, res), "declined") {
		t.Fatalf("declined save = %#v", res)
	}
	found := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "release candidate"}}))
	if m, _ := found["matches"].([]any); len(m) != 0 {
		t.Fatalf("a declined save was saved: %#v", found)
	}
}

func TestSaveToolNeedsAuthoringAndAnAbsolutePath(t *testing.T) {
	saveHome(t)
	c := startServer(t, true, accept)
	draft := authoredDraft(t)
	os.Remove(filepath.Join(draft, "AUTHORING.json"))
	for _, pkg := range []string{draft, "relative/dir"} {
		res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": pkg}})
		if res["isError"] != true {
			t.Fatalf("%s saved: %#v", pkg, res)
		}
	}
}

// A save that would fail is refused before the person is asked.
func TestSaveToolChecksAuthoringBeforeAsking(t *testing.T) {
	saveHome(t)
	asked := false
	c := startServer(t, true, func(map[string]any) map[string]any {
		asked = true
		return map[string]any{"action": "accept", "content": map[string]any{"approve": true}}
	})
	draft := authoredDraft(t)
	if err := os.WriteFile(filepath.Join(draft, "AUTHORING.json"), []byte(`{"kind":"tap.authoring/v1","sources":["codex/abc/0"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": draft}})
	if res["isError"] != true || asked || !strings.Contains(toolText(t, res), "opaque refs") {
		t.Fatalf("asked=%v res=%#v", asked, res)
	}
}
