package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// Found testing OpenCode: the agent declared git as a tool, saved the
// package, and every later run was refused because OpenCode cannot lend its
// connections. The save now says so, with the fix.
func TestSaveRefusesToolsTheClientCannotLend(t *testing.T) {
	saveHome(t)
	old := testClientName
	testClientName = "opencode"
	t.Cleanup(func() { testClientName = old })
	c := startServer(t, true, accept)
	draft := authoredDraft(t)
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: local.me, name: release-check, version: 0.1.0, description: Check a release candidate}\nexecution: {entrypoint: main.sh}\ntools:\n  - {alias: git, capability: git.shell, effect: read}\n"
	if err := os.WriteFile(filepath.Join(draft, "primitive.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": draft}})
	if text := toolText(t, res); res["isError"] != true || !strings.Contains(text, "cannot lend its connections") || !strings.Contains(text, "commands:") {
		t.Fatalf("save = %#v", res)
	}
}

func TestUnlendableToolsOnlyNamesRequiredToolsOnClientsThatCannotLend(t *testing.T) {
	m := &mf.Manifest{Tools: []mf.Tool{{Alias: "git", Capability: "git.shell", Effect: "read"}, {Alias: "mail", Capability: "gmail.threads.search", Effect: "read", Optional: true}}}
	if why := unlendableTools("opencode", m); !strings.Contains(why, "git (git.shell)") || strings.Contains(why, "mail") {
		t.Fatalf("opencode: %q", why)
	}
	for _, client := range []string{"claude", "codex", "kilo", "goose", "gemini", "", "unknown"} {
		if why := unlendableTools(client, m); why != "" {
			t.Fatalf("%s: %q", client, why)
		}
	}
	if why := unlendableTools("crush", &mf.Manifest{Tools: []mf.Tool{{Alias: "mail", Optional: true}}}); why != "" {
		t.Fatalf("optional only: %q", why)
	}
}
