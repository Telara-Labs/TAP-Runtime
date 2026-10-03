// Package mcpfixture is a stdio MCP server for tests that drive a real agent:
// a tracker with search_issues (returns ABC-12 and ABC-13) and get_issue
// (ABC-99 fails). An agent starts the test binary itself as the server; see
// IsServer.
package mcpfixture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Args are the arguments that start the test binary as the server, given
// the name of the test function that calls Serve.
func Args(test string) []string { return []string{"-test.run=^" + test + "$", "--", "tracker"} }

// IsServer reports whether this process was started with Args.
func IsServer() bool {
	n := len(os.Args)
	return n >= 2 && os.Args[n-2] == "--" && os.Args[n-1] == "tracker"
}

// Serve answers MCP requests on in until it closes. Unknown methods,
// including server/discover, get -32601, so a client falls back to
// initialize.
func Serve(in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	reply := func(id any, result any, rpcErr map[string]any) {
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			m["error"] = rpcErr
		} else {
			m["result"] = result
		}
		b, _ := json.Marshal(m)
		out.Write(append(b, '\n'))
	}
	text := func(s string, isErr bool) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isErr}
	}
	str := map[string]any{"type": "string"}
	for sc.Scan() {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "tracker", "version": "1"}}, nil)
		case "tools/list":
			reply(req.ID, map[string]any{"tools": []any{
				map[string]any{"name": "search_issues", "description": "Search issues with a JQL query; returns their keys.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"jql": str}, "required": []string{"jql"}}},
				map[string]any{"name": "get_issue", "description": "Get one issue by key.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"issue_key": str}, "required": []string{"issue_key"}}},
			}}, nil)
		case "tools/call":
			switch key, _ := req.Params.Arguments["issue_key"].(string); {
			case req.Params.Name == "search_issues":
				reply(req.ID, text(`{"issues":[{"key":"ABC-12"},{"key":"ABC-13"}]}`, false), nil)
			case key == "ABC-99":
				reply(req.ID, text("Issue ABC-99 does not exist", true), nil)
			default:
				reply(req.ID, text(fmt.Sprintf(`{"key":%q}`, key), false), nil)
			}
		default:
			reply(req.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
		}
	}
}

// GooseHome writes a Goose configuration under home with the test binary
// as the extension tracker (started with Args(test)) and Goose in approve
// mode, which a bridge must override. Extra YAML is appended at top level.
func GooseHome(home, test, extra string) error {
	dir := home + "/.config/goose"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	args, _ := json.Marshal(Args(test))
	cfg := fmt.Sprintf(`GOOSE_PROVIDER: openai
GOOSE_MODEL: none
GOOSE_MODE: approve
extensions:
  tracker:
    enabled: true
    type: stdio
    name: tracker
    cmd: %q
    args: %s
    timeout: 60
%s`, os.Args[0], args, extra)
	return os.WriteFile(dir+"/config.yaml", []byte(cfg), 0o600)
}
