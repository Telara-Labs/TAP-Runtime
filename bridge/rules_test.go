package bridge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// The person's own deny and ask rules, read per client. The shapes
// are those the clients answer with: Codex's mcp_servers table as
// `codex app-server` returns it from config/read (Codex 0.147.0), Gemini CLI's
// settings.json, and Claude Code's list_permission_rules.

func TestCodexRulesDenyADisabledServerAToolOutsideEnabledToolsAndOneInDisabledTools(t *testing.T) {
	rs := codexRulesFrom(map[string]any{"mcp_servers": map[string]any{
		"off":     map[string]any{"enabled": false},
		"allowed": map[string]any{"enabled": true, "enabled_tools": []any{"search"}},
		"blocked": map[string]any{"disabled_tools": []any{"delete"}},
		"open":    map[string]any{"enabled": true},
	}})
	for _, c := range []struct {
		server, tool string
		denied       bool
	}{
		{"off", "anything", true},
		{"allowed", "search", false},
		{"allowed", "delete", true},
		{"blocked", "delete", true},
		{"blocked", "search", false},
		{"open", "anything", false},
		{"unknown server", "x", false},
	} {
		if got := rs[c.server].denies(c.tool); got != c.denied && c.server != "unknown server" {
			t.Errorf("%s / %s: denied = %v, want %v", c.server, c.tool, got, c.denied)
		}
	}
}

func TestCodexPromptApprovalModeIsAnAskRuleAndOtherModesAreNot(t *testing.T) {
	rs := codexRulesFrom(map[string]any{"mcp_servers": map[string]any{
		"s": map[string]any{"tools": map[string]any{
			"asks":  map[string]any{"approval_mode": "prompt"},
			"auto":  map[string]any{"approval_mode": "approve"},
			"write": map[string]any{"approval_mode": "writes"},
		}},
	}})
	if !rs["s"].alwaysPrompts["asks"] || rs["s"].alwaysPrompts["auto"] || rs["s"].alwaysPrompts["write"] {
		t.Fatalf("got %+v", rs["s"].alwaysPrompts)
	}
}

func TestGeminiSettingsExcludeAndIncludeTools(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.json")
	project := filepath.Join(dir, "project.json")
	os.WriteFile(user, []byte(`{"mcpServers":{"gmail":{"excludeTools":["delete_draft"]},"jira":{"includeTools":["search","get","edit"]}}}`), 0o644)
	os.WriteFile(project, []byte(`{"mcpServers":{"gmail":{"excludeTools":["send"]},"jira":{"includeTools":["search"]}}}`), 0o644)
	denied := GeminiRules(user, project, filepath.Join(dir, "missing.json"))
	for _, c := range []struct {
		server, tool string
		want         bool
	}{
		{"gmail", "delete_draft", true}, // the user's rule
		{"gmail", "send", true},         // the project's rule adds to it
		{"gmail", "search", false},
		{"jira", "search", false},
		{"jira", "get", true}, // the project narrowed the allow list
		{"jira", "edit", true},
		{"slack", "post", false},
	} {
		if got := denied(bind.Tool{Server: c.server, Name: c.tool}); got != c.want {
			t.Errorf("%s / %s: denied = %v, want %v", c.server, c.tool, got, c.want)
		}
	}
}

func TestClaudeRulesAreSplitIntoDenyAndAsk(t *testing.T) {
	deny, ask := splitRules([]any{
		map[string]any{"behavior": "deny", "rule": "mcp__gmail__delete_draft"},
		map[string]any{"behavior": "ask", "rule": "mcp__gmail"},
		map[string]any{"behavior": "allow", "rule": "mcp__jira"},
		map[string]any{"behavior": "ask"},
	})
	if len(deny) != 1 || deny[0] != "mcp__gmail__delete_draft" || len(ask) != 1 || ask[0] != "mcp__gmail" {
		t.Fatalf("deny=%v ask=%v", deny, ask)
	}
	if !ruleCovers(ask[0], "mcp__gmail__search_threads") {
		t.Fatal("a server-wide ask rule did not cover its tool")
	}
}

// Found testing a real Claude Code with two servers named probe-a and probe-b: a
// hyphen was turned into an underscore, so the call went to a server that does
// not exist ("MCP server not connected: probe_b").
func TestClaudeKeepsHyphensInAServerName(t *testing.T) {
	for server, want := range map[string]string{
		"probe-b":          "mcp__probe-b__search_items",
		"claude-in-chrome": "mcp__claude-in-chrome__search_items",
		"claude.ai Gmail":  "mcp__claude_ai_Gmail__search_items",
		"under_score":      "mcp__under_score__search_items",
	} {
		if got := claudeName(server, "search_items"); got != want {
			t.Errorf("%q: %q, want %q", server, got, want)
		}
	}
}
