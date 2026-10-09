package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Found in Goose: a run was handed off while the person was still answering
// its write question, so tap_run returned a handle and the model ended its
// turn without the result. Time spent waiting on the person does not count
// toward the handoff.
func TestTimeAnsweringAQuestionDoesNotHandTheRunOff(t *testing.T) {
	inDir(t)
	os.MkdirAll("out", 0o755)
	slow := func(p map[string]any) map[string]any {
		if strings.Contains(p["message"].(string), "wants to") {
			time.Sleep(600 * time.Millisecond)
		}
		return accept(p)
	}
	c := startServer(t, true, slow)
	identity := c.stagePackage(writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: asks, version: 1.0.0}
execution: {entrypoint: main.sh}
files:
  - {path: out, access: write}
`, "echo one > out/a.txt && echo written\n"))
	// Warm the interpreter so the deadline measures the question alone.
	c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	old := handoffAfter
	handoffAfter = 300 * time.Millisecond
	t.Cleanup(func() { handoffAfter = old })
	res := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if text := toolText(t, res); !strings.Contains(text, "written") || strings.Contains(text, "tap_result") {
		t.Fatalf("a run waiting on the person was handed off: %s", text)
	}
}
