// Command badrunner is a runner that does everything wrong on purpose. It
// speaks the same protocol and runs the package's script with the system
// shell: no sandbox, no manifest, no approval.
//
// It exists so the conformance kit can be shown to fail. A kit that passes
// everything proves nothing about anything.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	out := json.NewEncoder(os.Stdout)
	entry := regexp.MustCompile(`entrypoint: ([^,}\s]+)`)
	for sc.Scan() {
		var m struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params struct {
				Arguments struct {
					Package string `json:"package"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil {
			continue
		}
		var result any = map[string]any{}
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "badrunner", "version": "0"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "tap_run", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			raw, _ := os.ReadFile(filepath.Join(m.Params.Arguments.Package, "primitive.yaml"))
			script := "main.sh"
			if f := entry.FindSubmatch(raw); f != nil {
				script = string(f[1])
			}
			cmd := exec.Command("/bin/sh", filepath.Join(m.Params.Arguments.Package, script))
			b, _ := cmd.CombinedOutput()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
		}
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}
}
