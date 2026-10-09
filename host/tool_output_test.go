package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReadableInspectionFlowForMCPClients(t *testing.T) {
	oldHistory := readHistory
	readHistory = func(string) []pastRequest { return nil }
	t.Cleanup(func() { readHistory = oldHistory })
	for _, name := range []string{"claude-code", "codex-mcp-client", "gemini-cli-mcp-client", "unknown-client"} {
		t.Run(name, func(t *testing.T) {
			previous := testClientName
			testClientName = name
			defer func() { testClientName = previous }()
			c := startServer(t, true, accept)
			// Text-only clients on older protocol versions get the same payload.
			c.call("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"elicitation": map[string]any{}}, "clientInfo": map[string]any{"name": name, "version": "test"}})
			identity := c.stagePackage(writePackage(t, inputManifest, "echo actual-result\n"))
			search := c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "release-check"}})
			text := toolText(t, search)
			if !strings.Contains(text, identity["ref"].(string)) || !strings.Contains(text, identity["digest"].(string)) || search["structuredContent"] != nil {
				t.Fatalf("search lost identity or duplicated payload: %#v", search)
			}
			loaded := c.call("tools/call", map[string]any{"name": "tap_load", "arguments": identity})
			text = toolText(t, loaded)
			for _, need := range []string{"Loaded example.test/release-check@1.0.0", "candidate (required)", "base_tag (required)", "one element", "args[0]", "tap_run", "detail=true", identity["digest"].(string)} {
				if !strings.Contains(text, need) {
					t.Fatalf("load lost %q: %s", need, text)
				}
			}
			if strings.Contains(text, `\"`) || strings.Contains(text, `: null`) || loaded["structuredContent"] != nil {
				t.Fatalf("load is escaped or duplicated: %#v", loaded)
			}
			run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"ref": identity["ref"], "digest": identity["digest"], "args": []string{`{"candidate":"abc","base_tag":"v1"}`}}})
			if run["isError"] == true || !strings.Contains(toolText(t, run), "actual-result") {
				t.Fatalf("readable load is not runnable: %#v", run)
			}
			id := regexp.MustCompile(`\[run ([^]]+)\]`).FindStringSubmatch(toolText(t, run))
			if len(id) != 2 {
				t.Fatal("missing run receipt")
			}
			for _, tool := range []string{"tap_status", "tap_evidence"} {
				res := c.call("tools/call", map[string]any{"name": tool, "arguments": map[string]any{"run_id": id[1]}})
				text := toolText(t, res)
				if res["isError"] == true || !strings.Contains(text, "Run "+id[1]+" — finished") || !strings.Contains(text, identity["digest"].(string)) || strings.Contains(text, "actual-result") {
					t.Fatalf("inspection lost provenance or leaked results: %#v", res)
				}
				full := toolObject(t, c.callDetail("tools/call", map[string]any{"name": tool, "arguments": map[string]any{"run_id": id[1]}}))
				if full["state"] != "finished" || full["package_digest"] != identity["digest"] {
					t.Fatalf("JSON compatibility lost: %#v", full)
				}
			}
			if len(c.asked) != 0 {
				t.Fatal("formatting introduced an action approval")
			}
		})
	}
}

func TestReadableEncodingAndSizeErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		detail  bool
		message string
	}{
		{"invalid JSON value", map[string]any{"bad": make(chan int)}, false, "could not be encoded"},
		{"bounded JSON", map[string]any{"description": strings.Repeat("x", 64<<10)}, true, "too large"},
		{"bounded text", map[string]any{"matches": []any{map[string]any{"ref": strings.Repeat("x", (64<<10)-55)}}}, false, "request detail=true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			s := &server{out: &out}
			id := json.RawMessage("1")
			s.toolOutput(&id, "tap_search", test.value, test.detail)
			var response struct {
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Result["isError"] != true || !strings.Contains(toolText(t, response.Result), test.message) {
				t.Fatalf("error suppressed or payload silently shortened (response bytes=%d, isError=%v)", out.Len(), response.Result["isError"])
			}
		})
	}
}

func TestReadableLoadRetainsInputConstraintsAndWriteWarnings(t *testing.T) {
	value := map[string]any{
		"ref": "test/task@1.0.0", "digest": strings.Repeat("a", 64),
		"interface": map[string]any{"inputSchema": map[string]any{
			"type": "object", "required": []any{"candidate"}, "additionalProperties": false,
			"properties": map[string]any{"candidate": map[string]any{"type": "string", "enum": []any{"a", "b"}, "default": nil}},
			"allOf":      []any{map[string]any{"if": map[string]any{"required": []any{"candidate"}}, "then": map[string]any{"required": []any{"approval"}}}},
		}},
		"tools":    []any{map[string]any{"alias": "publish", "effect": "write", "capability": "release.publish"}},
		"commands": []any{map[string]any{"command": "git", "args": []any{}, "effect": "write"}},
		"files":    []any{map[string]any{"path": "report.txt", "access": "write"}},
		"fetch":    []any{map[string]any{"origin": "https://example.test", "methods": []any{"POST"}}},
		"connection_preview": map[string]any{"status": "available", "truncated": true, "connections": []any{
			map[string]any{"alias": "publish", "status": "resolved", "server": "telara", "tool": "gateway", "base_effect": "write", "base_approval_required": true, "requires_runtime_check": true, "operation": "release.publish"},
			map[string]any{"alias": "optional", "status": "ambiguous", "optional": true},
		}},
	}
	text := readableToolOutput("tap_load", value)
	for _, need := range []string{`"additionalProperties":false`, `"default":null`, `"enum":["a","b"]`, `"allOf"`, `"approval"`, `"args":[]`, "publish [write]", "report.txt", "POST", "approval required", "runtime check required", "operation release.publish", "optional: ambiguous; optional", "Preview truncated", "execution rechecks"} {
		if !strings.Contains(text, need) {
			t.Fatalf("formatter hid %q: %s", need, text)
		}
	}
}

func TestGalleryLoadResponseBudget(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(repoRoot, "examples", "*", "primitive.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatal("missing example manifests", err)
	}
	var compact, complete int
	for _, path := range paths {
		entry, err := readCatalogEntry(filepath.Dir(path), "test")
		if err != nil {
			t.Fatal(path, err)
		}
		m := entry.Manifest
		value := map[string]any{"ref": entry.Ref, "digest": entry.Digest, "description": entry.Description, "interface": m.Interface, "capabilities": m.Capabilities, "tools": m.Tools, "commands": m.Commands, "files": m.Files, "fetch": m.Fetch, "connection_preview": emptyPreview("inventory_unavailable")}
		full, _ := json.Marshal(value)
		var object map[string]any
		json.Unmarshal(full, &object)
		short := readableToolOutput("tap_load", object)
		// Measure the actual MCP wire string too: JSON text escaping is part
		// of the clutter and cost seen in the user's screenshot.
		shortWire, _ := json.Marshal(short)
		fullWire, _ := json.Marshal(string(full))
		compact += len(shortWire)
		complete += len(fullWire)
		t.Logf("%s: readable %d bytes; complete %d bytes", entry.Ref, len(shortWire), len(fullWire))
	}
	if compact >= complete {
		t.Fatalf("readable gallery grew: %d >= %d", compact, complete)
	}
	t.Logf("gallery total: %d -> %d MCP text bytes (%.1f%% less); bytes are a size proxy, not a model-token count", complete, compact, 100*(1-float64(compact)/float64(complete)))
}

func TestReadableNoMatchAndDetailValidation(t *testing.T) {
	c := startServer(t, false, accept)
	for _, detail := range []any{false, true, "invalid"} {
		res := c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "no-such-output-fixture", "detail": detail}})
		switch detail {
		case false:
			if !strings.Contains(toolText(t, res), "No matching TAP primitive.") || !strings.Contains(toolText(t, res), "Note:") {
				t.Fatalf("no-match note lost: %#v", res)
			}
		case true:
			if toolObject(t, res)["note"] == nil {
				t.Fatalf("JSON no-match note lost: %#v", res)
			}
		default:
			if res["isError"] != true {
				t.Fatalf("invalid detail argument accepted: %#v", res)
			}
		}
	}
}
