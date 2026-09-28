package bridge

import (
	"os/exec"
	"testing"

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
