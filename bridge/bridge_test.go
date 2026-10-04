package bridge

import (
	"os"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

func TestClaudeEffect(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want bind.Effect
	}{
		{"read only", map[string]any{"readOnly": true}, bind.Read},
		{"destructive", map[string]any{"destructive": true, "openWorld": true}, bind.Destructive},
		{"destructive wins over read only", map[string]any{"destructive": true, "readOnly": true}, bind.Destructive},
		// Gmail's create_draft arrives like this. It is a write, and nothing says so.
		{"empty is unknown, never read", map[string]any{}, bind.Unknown},
		{"open world alone says nothing about writing", map[string]any{"openWorld": true}, bind.Unknown},
		{"absent", nil, bind.Unknown},
	}
	for _, c := range cases {
		if got := claudeEffect(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCodexEffect(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want bind.Effect
	}{
		{"read only", map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false}, bind.Read},
		{"stated not read only is a write", map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true}, bind.Write},
		{"destructive", map[string]any{"readOnlyHint": false, "destructiveHint": true}, bind.Destructive},
		{"nothing stated", map[string]any{}, bind.Unknown},
	}
	for _, c := range cases {
		if got := codexEffect(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCodexBridgeThreadDelegatesApprovalToTAP(t *testing.T) {
	params := codexBridgeThreadStartParams()
	if params["ephemeral"] != true {
		t.Fatalf("bridge thread is not ephemeral: %#v", params)
	}
	if params["approvalPolicy"] != "never" {
		t.Fatalf("bridge thread must not add a second MCP approval gate: %#v", params)
	}
	if params["cwd"] != os.TempDir() {
		t.Fatalf("bridge thread cwd = %#v, want temp dir", params["cwd"])
	}
}

func TestClaudeName(t *testing.T) {
	cases := map[[2]string]string{
		{"claude.ai Gmail", "search_threads"}:             "mcp__claude_ai_Gmail__search_threads",
		{"telara", "telara_task_list"}:                    "mcp__telara__telara_task_list",
		{"plugin:playwright:playwright", "browser_close"}: "mcp__plugin_playwright_playwright__browser_close",
	}
	for in, want := range cases {
		if got := claudeName(in[0], in[1]); got != want {
			t.Errorf("claudeName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestRuleCovers(t *testing.T) {
	const q = "mcp__claude_ai_Gmail__send_message"
	cases := []struct {
		rule string
		want bool
	}{
		{"mcp__claude_ai_Gmail__send_message", true},
		{"mcp__claude_ai_Gmail__*", true},
		{"mcp__claude_ai_Gmail", true},
		{"mcp__*", true},
		{"mcp__claude_ai_Gmail__send", false},
		{"mcp__claude_ai_Gmail__search_threads", false},
		{"mcp__claude_ai_Google_Drive", false},
		{"mcp__claude_ai_G", false},
		{"Bash", false},
	}
	for _, c := range cases {
		if got := ruleCovers(c.rule, q); got != c.want {
			t.Errorf("ruleCovers(%q) = %v, want %v", c.rule, got, c.want)
		}
	}
}

func TestTested(t *testing.T) {
	if !Tested("claude-code", "2.1.284") {
		t.Error("the version this runner was run against is not listed")
	}
	if Tested("claude-code", "9.9.9") || Tested("cursor", "1.0") {
		t.Error("an unlisted client or version reads as tested")
	}
}
