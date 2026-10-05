package main

import (
	"testing"

	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
)

// The discover registry says which agents a saved primitive can run in
// (pointers go there by default, TENG-3109); the runner decides which it can
// borrow connections from. The two must agree (TENG-3108, plan §3.1). VS Code
// lends its tools through the TAP extension's socket, not openBridge.
func TestRegistryBridgeMatchesRunner(t *testing.T) {
	for _, c := range agents.All() {
		lends := c.MCP.Kind == agents.MCPExtension
		for _, name := range append([]string{c.ID}, c.Aliases...) {
			lends = lends || lendsConnections(name)
		}
		if lends != c.Bridge {
			t.Errorf("%s: registry Bridge=%v, runner lends connections=%v", c.ID, c.Bridge, lends)
		}
	}
	if !lendsConnections(clientFor("claude-code")) || !lendsConnections(clientFor("codex-mcp-client")) || !lendsConnections(clientFor("gemini-cli-mcp-client")) || !lendsConnections(clientFor("goose-cli")) {
		t.Fatal("a handshake name of a bridged client does not reach its bridge")
	}
}
