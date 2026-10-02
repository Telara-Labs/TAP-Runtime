package primitive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// writeCodexTranscript stores a session as Codex rollout lines
// (function_call / function_call_output keyed by call_id), where Codex keeps
// them: ~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl.
func writeCodexTranscript(t *testing.T, home string, s trace.Session) {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "27")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var lines []string
	add := func(v any) {
		b, _ := json.Marshal(v)
		lines = append(lines, string(b))
	}
	add(map[string]any{"type": "session_meta", "payload": map[string]any{"id": s.ID}})
	for _, c := range s.Calls {
		args, _ := json.Marshal(c.Args)
		add(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": c.Tool, "arguments": string(args), "call_id": c.ID}})
		add(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": c.ID, "output": c.Output}})
	}
	path := filepath.Join(dir, "rollout-2026-09-27T10-00-00-"+s.ID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A handoff cites transcript lines for Codex sessions too, not only Claude
// Code ones (TENG-3108: findTranscript followed one client).
func TestHandoffLocatesCodexTranscriptLines(t *testing.T) {
	home := t.TempDir()
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000003%d", i)
		s := session(fmt.Sprintf("019a0000-0000-7000-8000-00000000000%d", i), []string{"start the work"},
			call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, time.Second))
		s.Client = "codex"
		for j := range s.Calls {
			s.Calls[j].Client, s.Calls[j].Session = "codex", s.ID
		}
		writeCodexTranscript(t, home, s)
		ss = append(ss, s)
	}
	p := find(t, Discover(ss, nil), "mcp:task_create", "mcp:task_checkpoint")
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := WriteHandoff(dir, home, *p, ss, Skill{Source: "skill/SKILL.md", Content: []byte("# skill")}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "EVIDENCE-INDEX.json"))
	var idx EvidenceIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Available != 3 {
		t.Fatalf("%d of %d executions found a transcript", idx.Available, idx.Total)
	}
	for _, ex := range idx.Executions {
		for _, c := range ex.Calls {
			if c.Call == nil || c.Result == nil || c.Result.Line != c.Call.Line+1 {
				t.Fatalf("call %s located at %+v / %+v", c.CallID, c.Call, c.Result)
			}
		}
	}
}
