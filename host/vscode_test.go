package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A primitive run through the TAP extension for VS Code: the tool binds by
// name and schema from the editor's own list, the call goes to the editor,
// and the program gets its answer.
func TestRunThroughVSCode(t *testing.T) {
	dir, err := os.MkdirTemp("", "tapvs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "x.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var called []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req map[string]any
					json.Unmarshal(sc.Bytes(), &req)
					res := map[string]any{"id": req["id"]}
					switch req["op"] {
					case "hello":
						res["version"] = "1.138.0"
					case "tools":
						res["tools"] = []any{
							map[string]any{"name": "mcp_github_search_issues", "tags": []any{"mcp:github"},
								"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
							map[string]any{"name": "mcp_tap_tap_run", "tags": []any{}},
						}
					case "call":
						b, _ := json.Marshal(req["input"])
						called = append(called, req["name"].(string)+" "+string(b))
						res["text"] = `{"total_count":7}`
					}
					b, _ := json.Marshal(res)
					c.Write(append(b, '\n'))
				}
			}(c)
		}
	}()

	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: open-issues, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: issues, capability: github.issues.search, effect: read}
`, `tap call issues '{"q":"is:open"}' | jq -r '"open issues: " + (.total_count|tostring)'
`)
	res, err := Run(t.Context(), Options{Package: pkg, Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
		VSCodeSocket: sock, Approve: clientApprovesCalls(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "open issues: 7") || res.Ran != 1 {
		t.Fatalf("stdout %q, stderr %q, ran %d", res.Stdout, res.Stderr, res.Ran)
	}
	if len(called) != 1 || called[0] != `mcp_github_search_issues {"q":"is:open"}` {
		t.Errorf("the call sent to the editor: %v", called)
	}
}
