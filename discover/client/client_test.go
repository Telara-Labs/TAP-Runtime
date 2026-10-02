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
		if err != nil || ids(cs) != "codex,gemini-cli" {
			t.Errorf("Resolve(%q) = %s, %v; want codex,gemini-cli", list, ids(cs), err)
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
	if _, err := Resolve("aider", home, CapLaunch, HasLaunch); err == nil || !strings.Contains(err.Error(), "does not support starting from the command line") {
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

// Every entry says where its facts were checked (TENG-3125).
func TestEveryEntryNamesItsSource(t *testing.T) {
	for _, c := range All() {
		if len(c.Source) < 20 {
			t.Errorf("%s: no source", c.ID)
		}
	}
}

// VS Code counts as Copilot only where Copilot Chat has kept state; a
// plain VS Code install is not a Copilot user.
func TestVSCodeIsCopilotOnlyWithCopilotChat(t *testing.T) {
	home := t.TempDir()
	user := filepath.Join(home, "Library", "Application Support", "Code", "User")
	os.MkdirAll(filepath.Join(user, "globalStorage", "ms-python.python"), 0o755)
	c, _ := Lookup("vscode-copilot")
	if c.InstalledUnder(home) {
		t.Fatal("VS Code without Copilot Chat detected as Copilot")
	}
	os.MkdirAll(filepath.Join(user, "globalStorage", "github.copilot-chat"), 0o755)
	if !c.InstalledUnder(home) {
		t.Fatal("Copilot Chat state not detected")
	}
}

// Windows locations resolve through the existing APPDATA / LOCALAPPDATA
// variables, or their default place under home.
func TestWindowsAppDataMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("APPDATA", "")
	if got := MarkerPath("$APPDATA/Cursor", home); got != filepath.Join(home, "AppData", "Roaming", "Cursor") {
		t.Errorf("default APPDATA: %s", got)
	}
	roaming := t.TempDir()
	t.Setenv("APPDATA", roaming)
	if got := MarkerPath("$APPDATA/Cursor", home); got != filepath.Join(roaming, "Cursor") {
		t.Errorf("APPDATA: %s", got)
	}
	os.MkdirAll(filepath.Join(roaming, "Cursor"), 0o755)
	c, _ := Lookup("cursor")
	if !c.InstalledUnder(home) {
		t.Error("Cursor under APPDATA not detected")
	}
	if got := MarkerPath(".claude", home); got != filepath.Join(home, ".claude") {
		t.Errorf("home marker: %s", got)
	}
}

// Connected reads each agent's own configuration: JSON servers, a Codex
// TOML table, or the installed TAP extension for VS Code.
func TestConnectedReadsEachAgentsConfig(t *testing.T) {
	home := t.TempDir()
	at := func(rel, body string) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	is := func(id string) bool { c, _ := Lookup(id); return c.Connected(home, "tap") }
	for _, id := range []string{"claude-code", "codex", "cursor", "copilot-cli", "vscode-copilot", "aider"} {
		if is(id) {
			t.Errorf("%s connected with no configuration", id)
		}
	}
	at(".claude.json", `{"mcpServers":{"other":{},"tap":{"command":"tap"}}}`)
	at(".codex/config.toml", "[mcp_servers.telara]\ncommand = \"x\"\n\n[mcp_servers.\"tap\"]\ncommand = \"tap\"\n")
	at(".cursor/mcp.json", `{"mcpServers":{"tapx":{}}}`)
	at(".copilot/mcp-config.json", `{"mcpServers":{"tap":{"type":"local"}}}`)
	at(".vscode/extensions/telara-labs.tap-vscode-0.1.3/package.json", "{}")
	for id, want := range map[string]bool{"claude-code": true, "codex": true, "cursor": false, "copilot-cli": true, "vscode-copilot": true} {
		if got := is(id); got != want {
			t.Errorf("%s connected = %v, want %v", id, got, want)
		}
	}
	at(".cursor/mcp.json", `not json`)
	if is("cursor") {
		t.Error("a malformed file reads as connected")
	}
	// Goose: an extension in config.yaml, beside scalar settings.
	at(".config/goose/config.yaml", "GOOSE_PROVIDER: openai\nextensions:\n  developer:\n    type: builtin\n")
	if is("goose") {
		t.Error("goose connected with no tap extension")
	}
	at(".config/goose/config.yaml", "GOOSE_PROVIDER: openai\nextensions:\n  tap:\n    type: stdio\n    cmd: tap\n")
	if !is("goose") {
		t.Error("goose's tap extension not read")
	}
}
