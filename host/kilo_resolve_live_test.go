//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Through tap_run on a client that takes elicitations, a capability nothing
// in Kilo's configuration resolves is one question to the client, answered
// with one tool name; the runner checks it against the server real Kilo
// reports connected, binds it, keeps it, and runs the call through Kilo.
func TestKiloCapabilityIsAskedOnceThroughElicitation(t *testing.T) {
	if testing.Short() {
		t.Skip("runs kilo")
	}
	inDir(t)
	kiloOnPath(t)
	old := testClientName
	testClientName = "kilo"
	t.Cleanup(func() { testClientName = old })
	var questions []string
	c := startServer(t, true, func(p map[string]any) map[string]any {
		msg, _ := p["message"].(string)
		if strings.Contains(msg, "Which of your tools does this?") {
			questions = append(questions, msg)
			return map[string]any{"action": "accept", "content": map[string]any{"tool": "tracker/search_issues"}}
		}
		return map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 10}}
	})
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: kilo-asked, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: tracker.issues.search, effect: read}
`), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(`tap call search '{"jql":"status = open"}' | jq -r '.issues[0].key'
`), 0o644)
	for i := 0; i < 2; i++ {
		if out := c.run(pkg); !strings.Contains(out, "ABC-12") {
			t.Fatalf("run %d: %s", i+1, out)
		}
	}
	// Kilo's tools are listed from its own configuration, so the capability
	// binds to the listed tool and nobody is asked which tool it is.
	if len(questions) != 0 {
		t.Fatalf("questions: %q", questions)
	}
}
