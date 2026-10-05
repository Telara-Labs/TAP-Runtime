package primitive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// writeTranscript stores a session's calls and results as Claude Code
// transcript lines, so a handoff can locate them.
func writeTranscript(t *testing.T, home string, s trace.Session) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var lines []string
	add := func(v any) {
		b, _ := json.Marshal(v)
		lines = append(lines, string(b))
	}
	add(map[string]any{"type": "user", "message": map[string]any{"content": s.Requests[0]}})
	for _, c := range s.Calls {
		add(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": c.ID, "name": c.Tool, "input": c.Args}}}})
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": c.ID, "content": c.Output}}}})
	}
	path := filepath.Join(dir, s.ID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandoffIndexesEveryExecutionAndDetectsStaleLines(t *testing.T) {
	home := t.TempDir()
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000002%d", i)
		ss = append(ss, session(fmt.Sprintf("sess-%d", i), []string{"start the work"},
			call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, time.Second)))
	}
	for _, s := range ss {
		writeTranscript(t, home, s)
	}
	p := find(t, Discover(ss, nil), "mcp:task_create", "mcp:task_checkpoint")
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := WriteHandoff(dir, home, *p, ss, Skill{Source: "skill/SKILL.md", Content: []byte("# skill")}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"HANDOFF.md", "REFINE-PROMPT.md", "skill/SKILL.md", "QUESTIONS.md", "program-graph.json", "EVIDENCE-INDEX.json", "evidence/invocation-001.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "EVIDENCE-INDEX.json"))
	var idx EvidenceIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Total != len(p.Executions) || idx.Total != 3 || idx.Available != 3 {
		t.Fatalf("index covers %d/%d of %d executions", idx.Available, idx.Total, len(p.Executions))
	}
	for _, ex := range idx.Executions {
		for _, c := range ex.Calls {
			if c.Call == nil || c.Result == nil {
				t.Fatalf("call %s not located", c.CallID)
			}
		}
	}
	ex := idx.Executions[0]
	var out bytes.Buffer
	if err := Resolve(dir, ex.ID, &out); err != nil || strings.Contains(out.String(), "STALE") || !strings.Contains(out.String(), "verified") {
		t.Fatalf("resolve: %v\n%s", err, out.String())
	}
	// An appended record leaves existing locators valid.
	f, _ := os.OpenFile(ex.Transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"user","message":{"content":"more"}}` + "\n")
	f.Close()
	out.Reset()
	if err := Resolve(dir, ex.ID, &out); err != nil || strings.Contains(out.String(), "STALE") {
		t.Fatalf("append broke locators:\n%s", out.String())
	}
	// A rewritten line is reported stale, never silently trusted.
	data, _ := os.ReadFile(ex.Transcript)
	lines := strings.Split(string(data), "\n")
	lines[ex.Calls[0].Call.Line-1] = `{"type":"assistant","message":{"content":"rewritten"}}`
	os.WriteFile(ex.Transcript, []byte(strings.Join(lines, "\n")), 0o600)
	out.Reset()
	if err := Resolve(dir, ex.ID, &out); err != nil || !strings.Contains(out.String(), "STALE") {
		t.Fatalf("rewrite not detected:\n%s", out.String())
	}
	excerpt, _ := os.ReadFile(filepath.Join(dir, "evidence", "invocation-001.md"))
	if !strings.Contains(string(excerpt), "start the work") || !strings.Contains(string(excerpt), "task_id") {
		t.Fatalf("excerpt lacks the request or arguments:\n%s", excerpt)
	}
}
