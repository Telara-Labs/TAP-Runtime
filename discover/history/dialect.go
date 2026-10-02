package history

import (
	"encoding/json"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Tool-name dialects (plan §3.4.1, L4): how an agent records which MCP server
// and tool a call went to. Every dialect ends in MCPCall, so one MCP call
// gets the same Tool, MCPServer and MCPTool whichever agent recorded it.

// MCPCall fills c as a call to tool on server with args.
func MCPCall(c *trace.Call, server, tool string, args map[string]json.RawMessage) {
	c.Tool, c.Args, c.RawArgs = "mcp:"+tool, Flatten(args), RawKeys(args)
	c.MCPServer, c.MCPTool = server, tool
}

// DoubleUnderscore decodes a tool named the Anthropic way: "Bash" is the
// shell, "mcp__<server>__<tool>" an MCP tool, anything else a built-in.
func DoubleUnderscore(name string, input map[string]json.RawMessage) (tool, command string, args map[string]string, raw map[string]bool, server, mcpTool string) {
	switch {
	case name == "Bash":
		return "shell", RawString(input["command"]), nil, nil, "", ""
	case strings.HasPrefix(name, "mcp__"):
		server, mcpTool, _ = strings.Cut(strings.TrimPrefix(name, "mcp__"), "__")
		return "mcp:" + LastSegment(name), "", Flatten(input), RawKeys(input), server, mcpTool
	}
	return name, "", Flatten(input), RawKeys(input), "", ""
}

// Dispatcher decodes a generic tool whose arguments name the server, the
// tool and the tool's own arguments (Cursor's CallMcpTool, Antigravity's
// call_mcp_tool). The call takes the identity of the operation it names, the
// rule the runner applies to dispatchers (TENG-3054). ok is false when the
// arguments do not name both.
func Dispatcher(c *trace.Call, input map[string]json.RawMessage, serverKey, toolKey, argsKey string) (ok bool) {
	server, tool := RawString(input[serverKey]), RawString(input[toolKey])
	if server == "" || tool == "" || strings.HasPrefix(server, "{") || strings.HasPrefix(tool, "{") {
		return false
	}
	var args map[string]json.RawMessage
	raw := input[argsKey]
	var str string
	if json.Unmarshal(raw, &str) == nil {
		raw = json.RawMessage(str) // arguments recorded as a JSON string
	}
	if json.Unmarshal(raw, &args) != nil {
		args = map[string]json.RawMessage{}
	}
	MCPCall(c, server, tool, args)
	return true
}

// Without drops an agent's own bookkeeping keys from a call's arguments.
func Without(input map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(input))
	for k, v := range input {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// ResultText is a tool result as text: a JSON string as itself, anything
// else as its JSON.
func ResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return RawString(raw)
}

// Envelope returns the text inside <tag>…</tag>, for agents that wrap the
// person's message in metadata; ok is false when the tag is absent.
func Envelope(text, tag string) (string, bool) {
	open, close := "<"+tag+">", "</"+tag+">"
	i := strings.Index(text, open)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(open):]
	if j := strings.Index(rest, close); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest), true
}
