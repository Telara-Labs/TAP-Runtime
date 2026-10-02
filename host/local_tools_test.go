package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("missing tool content: %#v", result)
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("invalid tool content: %#v", result)
	}
	value, _ := part["text"].(string)
	return value
}

func toolObject(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(toolText(t, result)), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestTAPLocalFiveToolFlowAndDigestPin(t *testing.T) {
	c := startServer(t, true, accept)
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: greeting, version: 1.0.0, description: Greet a person}
execution: {entrypoint: main.sh}
`, "echo private-output\n")
	identity := c.stagePackage(pkg)
	search := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "greet"}}))
	matches, _ := search["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("search matches = %#v", search)
	}
	match := matches[0].(map[string]any)
	if match["ref"] != identity["ref"] || match["digest"] != identity["digest"] {
		t.Fatalf("search identity = %#v, want %#v", match, identity)
	}
	loaded := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_load", "arguments": identity}))
	if loaded["description"] != "Greet a person" || loaded["digest"] != identity["digest"] {
		t.Fatalf("loaded = %#v", loaded)
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	output := toolText(t, run)
	if run["isError"] == true || !strings.Contains(output, "private-output") {
		t.Fatalf("run = %#v", run)
	}
	id := regexp.MustCompile(`\[run ([^]]+)\]`).FindStringSubmatch(output)
	if len(id) != 2 {
		t.Fatalf("missing run id: %s", output)
	}
	status := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_status", "arguments": map[string]any{"run_id": id[1]}}))
	if status["state"] != "finished" || status["package_digest"] != identity["digest"] {
		t.Fatalf("status = %#v", status)
	}
	evidence := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_evidence", "arguments": map[string]any{"run_id": id[1], "limit": 10}}))
	encoded, _ := json.Marshal(evidence)
	if evidence["state"] != "finished" || strings.Contains(string(encoded), "private-output") {
		t.Fatalf("evidence exposed output or wrong state: %s", encoded)
	}

	// The model cannot submit an arbitrary path, and a changed package cannot
	// be invoked under the digest it advertised before the edit.
	pathCall := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"package": pkg}})
	if pathCall["isError"] != true || !strings.Contains(toolText(t, pathCall), "not a package path") {
		t.Fatalf("path call = %#v", pathCall)
	}
	sum := sha256.Sum256([]byte(pkg))
	staged := filepath.Join(c.catalogRoot, hex.EncodeToString(sum[:8]), "main.sh")
	if err := os.WriteFile(staged, []byte("echo changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	drift := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if drift["isError"] != true {
		t.Fatalf("changed package was accepted: %#v", drift)
	}
}

func TestRunChecksExpectedDigestBeforeStarting(t *testing.T) {
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: greeting, version: 1.0.0}
execution: {entrypoint: main.sh}
`, "echo should-not-run\n")
	root := t.TempDir()
	_, err := Run(context.Background(), Options{Package: pkg, ExpectedDigest: strings.Repeat("0", 64), RunsDir: root})
	if err == nil || !strings.Contains(err.Error(), "changed since discovery") {
		t.Fatalf("changed package was accepted: %v", err)
	}
	items, err := os.ReadDir(root)
	if err != nil || len(items) != 0 {
		t.Fatalf("digest refusal created a run: %#v, %v", items, err)
	}
}
