package main

import (
	"os/exec"
	"strings"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// Each agent's own shell setting is read: a primitive that runs a read-only
// program may run unasked only where the agent runs shell commands unasked.
func TestEachAgentsOwnShellPermissionIsRead(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		client, file, body string
		want               bool
	}{
		{"opencode", ".config/opencode/opencode.json", `{"mcp":{}}`, true},
		{"opencode", ".config/opencode/opencode.json", `{"permission":{"bash":"ask"}}`, false},
		{"opencode", ".config/opencode/opencode.json", `{"permission":{"bash":{"git log*":"allow","*":"ask"}}}`, false},
		{"opencode", ".config/opencode/opencode.json", `{"permission":{"bash":{"*":"allow"}}}`, true},
		{"opencode", ".config/opencode/opencode.json", `{"permission":"ask"}`, false},
		{"kilo", ".config/kilo/kilo.json", `{"permission":{"bash":"allow","webfetch":"ask"}}`, true},
		{"crush", ".config/crush/crush.json", `{"permissions":{"allowed_tools":["bash"]}}`, true},
		{"crush", ".config/crush/crush.json", `{"permissions":{"allowed_tools":["fetch"]}}`, false},
		{"goose-cli", ".config/goose/config.yaml", "GOOSE_MODE: auto\n", true},
		{"goose-cli", ".config/goose/config.yaml", "GOOSE_MODE: smart_approve\n", false},
		{"gemini-cli-mcp-client", ".gemini/settings.json", `{}`, false},
		{"gemini-cli-mcp-client", ".gemini/settings.json", `{"tools":{"allowed":["run_shell_command"]}}`, true},
	}
	for _, c := range cases {
		writeHomeFile(t, home, c.file, c.body)
		if got, why := agentDoesUnasked(c.client, home, unaskedShell, 0); got != c.want {
			t.Errorf("%s %s: %v (%s), want %v", c.client, c.body, got, why, c.want)
		}
	}
	if got, _ := agentDoesUnasked("test-client", home, unaskedShell, 0); got {
		t.Error("an unknown agent was allowed shell commands")
	}
}

// Gemini CLI started in yolo mode runs every tool unasked; TAP finds its
// command line among this server's ancestors, past a wrapper script.
func TestGeminiYoloIsReadFromItsCommandLine(t *testing.T) {
	home := t.TempDir()
	writeHomeFile(t, home, ".gemini/settings.json", `{}`)
	old := processArgs
	t.Cleanup(func() { processArgs = old })
	tree := func(gemini []string) func(int) ([]string, int, bool) {
		return func(pid int) ([]string, int, bool) {
			if pid == 1000 {
				return gemini, 1, true
			}
			return []string{"/bin/bash", "/home/me/tapwrap"}, 1000, true
		}
	}
	processArgs = tree([]string{"node", "/usr/lib/node_modules/@google/gemini-cli/bundle/gemini.js", "--approval-mode", "yolo", "-p", "x"})
	if got, why := agentDoesUnasked("gemini-cli-mcp-client", home, unaskedShell, 999); !got {
		t.Fatalf("yolo not seen: %s", why)
	}
	processArgs = tree([]string{"node", "/usr/bin/gemini", "-p", "x"})
	if got, _ := agentDoesUnasked("gemini-cli-mcp-client", home, unaskedWeb, 999); got {
		t.Fatal("Gemini CLI without yolo was taken to fetch unasked")
	}
}

// A primitive that runs git log runs in OpenCode, which cannot show a prompt,
// because OpenCode itself runs shell commands unasked by default. Set to ask,
// it is refused, and the refusal names the agent's own setting.
func TestAReadOnlyProgramRunsWhereTheAgentRunsShellUnasked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	inDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHomeFile(t, home, ".config/opencode/opencode.json", `{"mcp":{}}`)
	old := testClientName
	testClientName = "opencode"
	t.Cleanup(func() { testClientName = old })
	c := startServer(t, false, nil)
	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: git-version, version: 1.0.0}\nexecution: {entrypoint: main.sh}\ncommands:\n  - {command: git, args: [--version], effect: read}\n", "git --version\n")
	if out := c.run(pkg); !strings.Contains(out, "git version") {
		t.Fatalf("read-only git primitive did not run: %s", out)
	}
	writeHomeFile(t, home, ".config/opencode/opencode.json", `{"permission":{"bash":"ask"}}`)
	if out := c.run(pkg); strings.Contains(out, "git version") || !strings.Contains(out, "permission.bash") {
		t.Fatalf("ran, or did not name the agent's setting, although OpenCode asks before shell commands: %s", out)
	}
}

// A program declared read that runs other code still needs approval: the
// runner counts git -c as destructive whatever the manifest says.
func TestAReadDeclarationDoesNotLetARunnerOfOtherCodeRunUnasked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	inDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHomeFile(t, home, ".config/opencode/opencode.json", `{"mcp":{}}`)
	old := testClientName
	testClientName = "opencode"
	t.Cleanup(func() { testClientName = old })
	c := startServer(t, false, nil)
	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: git-alias, version: 1.0.0}\nexecution: {entrypoint: main.sh}\ncommands:\n  - {command: git, args: [\"*\"], effect: read}\n", "git -c alias.x='!echo ran-other-code' x\n")
	if out := c.run(pkg); strings.Contains(out, "ran-other-code") {
		t.Fatalf("git -c ran unasked: %s", out)
	}
}

// Kilo does not list its tools, so an unpinned required tool can never bind
// there; saving one is refused with the fix.
func TestUnlendableToolsOnKiloNeedPins(t *testing.T) {
	m := &mf.Manifest{Tools: []mf.Tool{{Alias: "shell", Capability: "local.shell", Effect: "read"}}}
	if why := unlendableTools("kilo", m); !strings.Contains(why, "pinned") || !strings.Contains(why, "commands:") {
		t.Fatalf("kilo unpinned: %q", why)
	}
	m.Tools[0].Pin = &mf.Pin{Server: "local", Tool: "shell"}
	if why := unlendableTools("kilo", m); why != "" {
		t.Fatalf("kilo pinned: %q", why)
	}
	if why := unlendableTools("claude", &mf.Manifest{Tools: []mf.Tool{{Alias: "a", Capability: "x.y"}}}); why != "" {
		t.Fatalf("claude: %q", why)
	}
}
