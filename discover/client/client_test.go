package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryIDsAndAliasesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, c := range All() {
		if c.ID == "" || c.Name == "" {
			t.Errorf("%+v: needs an ID and a name", c)
		}
		for _, n := range append([]string{c.ID}, c.Aliases...) {
			if n != strings.ToLower(n) || strings.ContainsAny(n, " ,") {
				t.Errorf("%s: name %q must be lower case with no spaces or commas", c.ID, n)
			}
			if n == "all" || n == "detected" {
				t.Errorf("%s: %q is reserved", c.ID, n)
			}
			if other, dup := seen[n]; dup {
				t.Errorf("%q names both %s and %s", n, other, c.ID)
			}
			seen[n] = c.ID
		}
		if len(c.Markers) == 0 {
			t.Errorf("%s: no detection marker", c.ID)
		}
	}
}

func TestAliasesResolve(t *testing.T) {
	for name, want := range map[string]string{
		"claude": "claude-code", "Claude-Code": "claude-code", " codex ": "codex",
		"gemini": "gemini-cli", "cursor-agent": "cursor-cli", "copilot": "copilot-cli", "vscode": "vscode-copilot",
	} {
		c, ok := Lookup(name)
		if !ok || c.ID != want {
			t.Errorf("Lookup(%q) = %q, %v; want %q", name, c.ID, ok, want)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("an unknown name resolved")
	}
}

func TestDetectedUsesOnlyHome(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".cursor/chats", ".gemini/tmp"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, c := range Detected(home) {
		got = append(got, c.ID)
	}
	if strings.Join(got, ",") != "cursor-cli,gemini-cli" {
		t.Fatalf("detected %v, want cursor-cli and gemini-cli", got)
	}
	if len(Detected(t.TempDir())) != 0 {
		t.Fatal("an empty home detected an agent")
	}
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	os.MkdirAll(filepath.Join(home, ".gemini", "tmp"), 0o755)
	ids := func(cs []Client) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return strings.Join(out, ",")
	}
	// Detected (the default) keeps only agents with the capability.
	for _, list := range []string{"", "detected"} {
		cs, err := Resolve(list, home, CapHistory, HasHistory)
		if err != nil || ids(cs) != "codex" {
			t.Errorf("Resolve(%q) = %s, %v; want codex", list, ids(cs), err)
		}
	}
	cs, err := Resolve("all", home, CapHistory, HasHistory)
	if err != nil || ids(cs) != strings.Join(IDs(HasHistory), ",") {
		t.Errorf("all = %s, %v", ids(cs), err)
	}
	cs, err = Resolve("claude,claude-code,codex", home, CapHistory, HasHistory)
	if err != nil || ids(cs) != "claude-code,codex" {
		t.Errorf("duplicates: %s, %v", ids(cs), err)
	}
	// A named client without the capability is an error that says so.
	if _, err := Resolve("aider", home, CapHistory, HasHistory); err == nil || !strings.Contains(err.Error(), "does not support reading session history") {
		t.Errorf("aider: %v", err)
	}
	// An unknown name lists the registry's clients, never a typed list.
	_, err = Resolve("nope", home, CapHistory, HasHistory)
	if err == nil || !strings.Contains(err.Error(), "claude-code") || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown: %v", err)
	}
}

func TestSkillsDirAndTranscriptPath(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	c, _ := Lookup("claude")
	if d, err := c.SkillsDir(false, home, proj); err != nil || d != filepath.Join(home, ".claude", "skills") {
		t.Errorf("claude global = %s, %v", d, err)
	}
	c, _ = Lookup("codex")
	if d, err := c.SkillsDir(true, home, proj); err != nil || d != filepath.Join(proj, ".agents", "skills") {
		t.Errorf("codex project = %s, %v", d, err)
	}
	c, _ = Lookup("aider")
	if _, err := c.SkillsDir(false, home, proj); err == nil {
		t.Error("aider has no skills folder")
	}
	f := filepath.Join(home, ".claude", "projects", "p", "abc.jsonl")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("{}\n"), 0o644)
	if got := TranscriptPath("claude", "abc", home); got != f {
		t.Errorf("transcript = %q", got)
	}
	for _, s := range []string{"*", "../abc", ""} {
		if got := TranscriptPath("claude", s, home); got != "" {
			t.Errorf("session %q matched %q", s, got)
		}
	}
	if TranscriptPath("cursor", "abc", home) != "" {
		t.Error("cursor keeps sessions in a database")
	}
}

// Every agent the runner can lend connections through has its MCP server
// registered somehow, or the bridge is unreachable.
func TestBridgeClientsCanConnectMCP(t *testing.T) {
	for _, c := range All() {
		if c.Bridge && c.MCP.Kind == MCPNone {
			t.Errorf("%s has a bridge but no way to connect the TAP MCP server", c.ID)
		}
		if c.MCP.Kind == MCPJSONFile && (c.MCP.Path == "" || c.MCP.Key == "") {
			t.Errorf("%s: a JSON MCP config needs a path and key", c.ID)
		}
	}
}
