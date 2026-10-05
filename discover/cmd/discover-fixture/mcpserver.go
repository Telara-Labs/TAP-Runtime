package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// The scripted fixture task needs an MCP server whose
// calls succeed and fail on purpose. `discover-fixture mcp` is one, over
// stdio: search_issues returns issue keys (an id-bearing output), get_issue
// returns an issue, or an error for a key that does not exist.

var fixtureIssues = map[string]string{
	"ABC-12": `{"key":"ABC-12","summary":"Login page times out","status":"Open"}`,
	"ABC-13": `{"key":"ABC-13","summary":"Export drops the last row","status":"Open"}`,
}

func fixtureTools() []any {
	obj := func(props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []any{
		map[string]any{"name": "search_issues", "description": "Search issues with a JQL query; returns their keys.",
			"inputSchema": obj(map[string]any{"jql": map[string]any{"type": "string"}}, "jql"), "annotations": map[string]any{"readOnlyHint": true}},
		map[string]any{"name": "get_issue", "description": "Get one issue by key.",
			"inputSchema": obj(map[string]any{"issue_key": map[string]any{"type": "string"}}, "issue_key"), "annotations": map[string]any{"readOnlyHint": true}},
	}
}

func serveFixtureMCP(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue // notifications need no answer
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "tracker", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": fixtureTools()}
		case "tools/call":
			text, isErr := `{"issues":[{"key":"ABC-12"},{"key":"ABC-13"}]}`, false
			if req.Params.Name == "get_issue" {
				var ok bool
				if text, ok = fixtureIssues[req.Params.Arguments["issue_key"]]; !ok {
					text, isErr = fmt.Sprintf("Issue %s does not exist", req.Params.Arguments["issue_key"]), true
				}
			} else if req.Params.Name != "search_issues" {
				text, isErr = "unknown tool "+req.Params.Name, true
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}
		case "ping":
			result = map[string]any{}
		default:
			// Method not found: a client speaking a newer protocol first
			// (server/discover) falls back to initialize on this answer.
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found: " + req.Method}}); err != nil {
				return err
			}
			continue
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
			return err
		}
	}
	return sc.Err()
}

func fixtureMCPMain() {
	if err := serveFixtureMCP(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "discover-fixture mcp:", err)
		os.Exit(1)
	}
}
