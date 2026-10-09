package main

import (
	"strings"
	"testing"
)

const inputManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: release-check, version: 1.0.0, description: Check a release}
execution: {entrypoint: main.sh}
interface:
  inputSchema:
    type: object
    required: [candidate, base_tag]
    properties:
      candidate: {type: string}
      base_tag: {type: string}
`

// The call Claude Code made in the clean-machine test: command-line flags
// for a primitive that reads one JSON object. It is refused before the
// program runs, with what to send instead.
func TestRunRefusesFlagStyleArgsForAnInputSchema(t *testing.T) {
	c := startServer(t, true, accept)
	identity := c.stagePackage(writePackage(t, inputManifest, "echo program-ran\n"))
	call := func(args ...string) map[string]any {
		return c.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"ref": identity["ref"], "digest": identity["digest"], "args": args}})
	}
	bad := call("--candidate", "3c39fceb", "--base_tag", "v19.4.0")
	if bad["isError"] != true || !strings.Contains(toolText(t, bad), `{"candidate": ..., "base_tag": ...}`) || strings.Contains(toolText(t, bad), `\"`) {
		t.Fatalf("flag-style args = %#v", bad)
	}
	missing := call(`{"candidate": "3c39fceb"}`)
	if missing["isError"] != true || !strings.Contains(toolText(t, missing), "lacks required base_tag") {
		t.Fatalf("missing field = %#v", missing)
	}
	ok := call(`{"candidate": "3c39fceb", "base_tag": "v19.4.0"}`)
	if ok["isError"] == true || !strings.Contains(toolText(t, ok), "program-ran") {
		t.Fatalf("JSON object args = %#v", ok)
	}
	loaded := toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_load", "arguments": identity}))
	if hint, _ := loaded["args"].(string); !strings.Contains(hint, "one element") {
		t.Fatalf("tap_load gives no args hint: %#v", loaded)
	}
}

func TestPrimitivesWithoutAnInputSchemaKeepTheirArgs(t *testing.T) {
	if why := checkRunArgs(nil, []string{"a", "b"}); why != "" {
		t.Fatal(why)
	}
}
