package main

import (
	"encoding/json"
	"testing"
)

// Every tool the server lists has an object input schema whose required is
// an array, never null: Claude Code rejects the whole tool list otherwise
// (found by `claude mcp list` during).
func TestEveryToolSchemaIsStrictJSONSchema(t *testing.T) {
	c := startServer(t, true, accept)
	res := c.call("tools/list", map[string]any{})
	tools, _ := res["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("no tools: %v", res)
	}
	for _, x := range tools {
		tool := x.(map[string]any)
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Errorf("%v: schema type %v", tool["name"], schema["type"])
		}
		if req, ok := schema["required"]; ok {
			if _, isArray := req.([]any); !isArray {
				b, _ := json.Marshal(req)
				t.Errorf("%v: required is %s, not an array", tool["name"], b)
			}
		}
	}
}
