// Package bridge borrows a client's MCP connections for the runner.
//
// A bridge holds no credential and reads no configuration. It asks the client,
// over the client's own control channel, what tools it has and asks it to call
// one. How each client is reached, what it names things and how it spells an
// annotation is code in this package; what the user has connected is always
// discovered live.
//
// Every bridge is a stand-in. It is deleted when its client can run a
// primitive itself.
package bridge

import (
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// Bridge is the whole of what the runner needs from a client.
// LateServers is a Bridge whose client connects some servers after it has
// started answering. AwaitLateServers waits, at most d, for them to be listed
// and connected, and reports whether the inventory is worth reading again.
type LateServers interface {
	AwaitLateServers(d time.Duration) bool
}

type Bridge interface {
	// Client names the client and its version, as the client reports them.
	Client() (name, version string)
	// Inventory lists every tool the client can dispatch.
	Inventory() ([]bind.Tool, error)
	// Denied reports whether the user has forbidden their client to use a
	// tool. The runner refuses such a tool.
	Denied(t bind.Tool) (bool, error)
	// Call dispatches one tool through the client's own session and returns
	// its result as text.
	Call(t bind.Tool, args map[string]any) (string, error)
	// HasSchemas reports whether the client gives tool input schemas, so the
	// receipt can say whether a contract could have been checked.
	HasSchemas() bool
	Close()
}

// tested lists the client versions each release of the runner was run
// against. An unlisted version is tried, with a warning.
var tested = map[string][]string{
	"claude-code": {"2.1.284"},
	"codex":       {"0.147.0"},
	"goose":       {"1.53.0"},
	"kilo":        {"7.8.3"},
	"copilot":     {"1.0.94"},
}

// Tested reports whether this runner was run against the client version.
// The MCP bridge is tested against the protocol, not a product: the version it
// reports is the server's, so any is admitted without a warning.
func Tested(name, version string) bool {
	if name == "mcp" {
		return true
	}
	for _, v := range tested[name] {
		if v == version {
			return true
		}
	}
	return false
}

// claudeEffect reads Claude Code's annotation spelling. It sends readOnly,
// destructive and openWorld, and leaves out whatever is false, so a tool with
// neither readOnly nor destructive has had nothing said about it.
func claudeEffect(a map[string]any) bind.Effect {
	if b, _ := a["destructive"].(bool); b {
		return bind.Destructive
	}
	if b, _ := a["readOnly"].(bool); b {
		return bind.Read
	}
	return bind.Unknown
}

// codexEffect reads Codex's spelling. It sends readOnlyHint, destructiveHint
// and openWorldHint, always all three, so readOnlyHint false is a statement
// that the tool writes.
func codexEffect(a map[string]any) bind.Effect {
	if b, _ := a["destructiveHint"].(bool); b {
		return bind.Destructive
	}
	ro, stated := a["readOnlyHint"].(bool)
	switch {
	case !stated:
		return bind.Unknown
	case ro:
		return bind.Read
	}
	return bind.Write
}

// claudeName is the fully-qualified name Claude Code dispatches by:
// mcp__<server>__<tool>, with every character of the server name that is not
// a letter or digit replaced by an underscore.
func claudeName(server, tool string) string {
	b := make([]rune, 0, len(server))
	for _, r := range server {
		// Letters, digits, underscore and hyphen are kept: Claude Code names a
		// tool of a server called claude-in-chrome mcp__claude-in-chrome__x.
		// Everything else, a space or a dot, becomes an underscore.
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b = append(b, r)
		} else {
			b = append(b, '_')
		}
	}
	return "mcp__" + string(b) + "__" + tool
}

// ruleCovers reports whether one permission rule names the tool. A rule is
// the exact tool, the server alone, or the server followed by a wildcard.
func ruleCovers(rule, qualified string) bool {
	if rule == qualified {
		return true
	}
	if strings.HasSuffix(rule, "*") {
		return strings.HasPrefix(qualified, strings.TrimSuffix(rule, "*"))
	}
	// "mcp__server" covers every tool of that server.
	return strings.HasPrefix(qualified, rule+"__")
}
