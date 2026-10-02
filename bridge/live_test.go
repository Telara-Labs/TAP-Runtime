package bridge

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

// These run against the real clients installed on this machine, through their
// real control channels. They are skipped where a client is not installed.

func TestLiveClaudeInventoryAndDenyRules(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	// The deny rule is given to this one process. No settings file is changed.
	c, err := NewClaude("--disallowedTools", "mcp__telara__telara_task_list")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name, version := c.Client()
	t.Logf("client %s %s, tested=%v", name, version, Tested(name, version))

	inv, err := c.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) == 0 {
		t.Skip("this Claude Code has no connected MCP servers")
	}
	counts := map[bind.Effect]int{}
	for _, tool := range inv {
		counts[tool.Annotated]++
	}
	t.Logf("%d tools: %v", len(inv), counts)

	denied, err := c.Denied(bind.Tool{Server: "telara", Name: "telara_task_list"})
	if err != nil {
		t.Fatal(err)
	}
	if !denied {
		t.Fatalf("a tool named in --disallowedTools is not reported as denied; rules seen: %v", c.deny)
	}
	other, err := c.Denied(bind.Tool{Server: "telara", Name: "telara_knowledge_search"})
	if err != nil {
		t.Fatal(err)
	}
	if other {
		t.Fatal("a tool nobody denied is reported as denied")
	}
}

func TestLiveCodexInventory(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	c, err := NewCodex()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name, version := c.Client()
	t.Logf("client %s %s, tested=%v", name, version, Tested(name, version))
	if version == "" {
		t.Fatal("no version read from codex")
	}
	inv, err := c.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[bind.Effect]int{}
	for _, tool := range inv {
		counts[tool.Annotated]++
	}
	t.Logf("%d tools: %v", len(inv), counts)
	if len(inv) == 0 {
		t.Skip("this Codex has no MCP servers")
	}
}

// TENG-3101: Codex answers config/read with its merged configuration, and the
// rules are read from it. This reads the real one and checks only that the
// read works and is consistent with the tools the same Codex lists.
func TestLiveCodexRulesAreReadable(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	c, err := NewCodex()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rs, err := c.rules()
	if err != nil {
		t.Fatalf("config/read: %v", err)
	}
	t.Logf("%d MCP servers have rules", len(rs))
	for server := range rs {
		if _, err := c.Denied(bind.Tool{Server: server, Name: "x"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Asks(bind.Tool{Server: server, Name: "x"}); err != nil {
			t.Fatal(err)
		}
	}
}

var liveVSCode = flag.Bool("live-vscode", false, "launch a real VS Code with the TAP extension (opens a window, then closes it)")

// TENG-3101: the VS Code ask rule, through the real extension in a real VS
// Code. Run with: go test ./bridge -run LiveVSCode -live-vscode
func TestLiveVSCodeAskRulesThroughTheRealExtension(t *testing.T) {
	code := "/Applications/Visual Studio Code.app/Contents/MacOS/Code"
	if !*liveVSCode {
		t.Skip("pass -live-vscode to open a real VS Code")
	}
	if _, err := os.Stat(code); err != nil {
		t.Skip("VS Code is not at " + code)
	}
	// A short path: VS Code refuses a user-data directory whose socket path is long.
	root, err := os.MkdirTemp("/tmp", "vsc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	os.MkdirAll(filepath.Join(root, "user", "User"), 0o755)
	os.WriteFile(filepath.Join(root, "user", "User", "settings.json"),
		[]byte(`{"chat.tools.eligibleForAutoApproval":{"github/search_issues":false,"runTask":false,"other":true}}`), 0o644)
	ext, _ := filepath.Abs(filepath.Join("..", "vscode"))
	cacheDir := filepath.Join(os.Getenv("HOME"), "Library", "Caches", "tap-runtime", "vscode")
	before, _ := filepath.Glob(filepath.Join(cacheDir, "*.sock"))
	cmd := exec.Command(code, "--user-data-dir", filepath.Join(root, "user"), "--extensions-dir", filepath.Join(root, "exts"),
		"--extensionDevelopmentPath", ext, "--new-window")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); exec.Command("pkill", "-f", filepath.Join(root, "user")).Run() }()
	var v *VSCode
	for i := 0; i < 40 && v == nil; i++ {
		time.Sleep(time.Second)
		socks, _ := filepath.Glob(filepath.Join(cacheDir, "*.sock"))
		for _, s := range socks {
			isNew := true
			for _, b := range before {
				if b == s {
					isNew = false
				}
			}
			if isNew {
				if c, err := NewVSCode(s); err == nil {
					v = c
					break
				}
			}
		}
	}
	if v == nil {
		t.Fatal("the extension's socket did not appear")
	}
	defer v.Close()
	name, version := v.Client()
	t.Logf("client %s %s", name, version)
	inv, err := v.Inventory()
	if err != nil || len(inv) == 0 {
		t.Fatalf("inventory: %d tools, %v", len(inv), err)
	}
	asked := map[string]bool{}
	for _, tool := range inv {
		ask, err := v.Asks(tool)
		if err != nil {
			t.Fatal(err)
		}
		if ask {
			asked[tool.Name] = true
		}
	}
	t.Logf("tools the setting makes ask: %v", asked)
	if !asked["run_task"] {
		t.Errorf("the setting's runTask key did not make run_task ask: %v", asked)
	}
	if asked["get_task_output"] || asked["vscode_listCodeUsages"] {
		t.Errorf("a tool the setting does not name asks: %v", asked)
	}
}
