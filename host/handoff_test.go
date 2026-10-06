package main

import (
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Codex stopped waiting for tap_run at ~30 s. A run past the deadline is
// handed back with a handle, keeps running, and tap_result returns its
// output.
func TestASlowRunIsHandedOffAndItsResultFetched(t *testing.T) {
	c := startServer(t, true, accept)
	identity := c.stagePackage(writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: slow, version: 1.0.0, description: A slow primitive}
execution: {entrypoint: main.sh}
`, "echo finished-output\n"))
	old := handoffAfter
	handoffAfter = 10 * time.Millisecond // compiling the interpreter alone takes longer
	t.Cleanup(func() { handoffAfter = old })

	first := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	text := toolText(t, first)
	key := regexp.MustCompile(`run "(held-[^"]+)"`).FindStringSubmatch(text)
	if first["isError"] == true || len(key) != 2 || !strings.Contains(text, "tap_result") {
		t.Fatalf("no handoff: %#v", first)
	}
	handoffAfter = time.Minute
	result := c.call("tools/call", map[string]any{"name": "tap_result", "arguments": map[string]any{"run": key[1]}})
	if out := toolText(t, result); result["isError"] == true || !strings.Contains(out, "finished-output") || !strings.Contains(out, "[run ") {
		t.Fatalf("result = %#v", result)
	}
	again := c.call("tools/call", map[string]any{"name": "tap_result", "arguments": map[string]any{"run": key[1]}})
	if again["isError"] != true {
		t.Fatalf("a fetched result was returned twice: %#v", again)
	}
}

func TestTapResultForAnUnknownRunSaysSo(t *testing.T) {
	c := startServer(t, true, accept)
	r := c.call("tools/call", map[string]any{"name": "tap_result", "arguments": map[string]any{"run": "held-0-0"}})
	if r["isError"] != true || !strings.Contains(toolText(t, r), "no run") {
		t.Fatalf("unknown run = %#v", r)
	}
}

func TestAPromptAfterHandoffIsBounded(t *testing.T) {
	old := handoffPromptWait
	handoffPromptWait = 10 * time.Millisecond
	t.Cleanup(func() { handoffPromptWait = old })
	var handed atomic.Bool
	handed.Store(true)
	never := make(chan struct{})
	a := boundedAfterHandoff(func(Ask) Grant { <-never; return Grant{OK: true} }, &handed)
	if g := a(Ask{}); g.OK {
		t.Fatal("an unanswered prompt after handoff was a yes")
	}
}
