package history

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// A question answered without a tool call is a session discover leaves out
// and a conversation record keeps.
func TestKeepEmptyKeepsSessionsWithoutCalls(t *testing.T) {
	claude := t.TempDir()
	writeJSONL(t, filepath.Join(claude, "p", "qa.jsonl"), []map[string]any{
		{"type": "user", "timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"role": "user", "content": "what is a goroutine"}},
		{"type": "assistant", "timestamp": "2026-10-01T10:00:01Z", "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "text", "text": "A lightweight thread."}}}},
	})
	writeJSONL(t, filepath.Join(claude, "p", "empty.jsonl"), []map[string]any{
		{"type": "summary", "timestamp": "2026-10-01T09:00:00Z"},
	})
	writeJSONL(t, filepath.Join(claude, "p", "tool.jsonl"), []map[string]any{
		{"type": "user", "timestamp": "2026-10-01T11:00:00Z", "message": map[string]any{"role": "user", "content": "list files"}},
		{"type": "assistant", "timestamp": "2026-10-01T11:00:01Z", "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "tool_use", "id": "t", "name": "Bash", "input": map[string]any{"command": "ls"}}}}},
	})
	codex := t.TempDir()
	writeJSONL(t, filepath.Join(codex, "2026", "10", "01", "rollout-qa.jsonl"), []map[string]any{
		{"timestamp": "2026-10-01T10:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "qa"}},
		{"timestamp": "2026-10-01T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "what is a goroutine"}}}},
		{"timestamp": "2026-10-01T10:00:02Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "A lightweight thread."}}}},
	})
	type eacher interface {
		Each(time.Time, func(trace.Session) error) error
		EachWith(time.Time, ReadOptions, func(trace.Session) error) error
	}
	for _, tc := range []struct {
		name        string
		r           eacher
		plain, keep int
	}{
		{"claude-code", ClaudeCode{Dir: claude}, 1, 2},
		{"codex", Codex{Dir: codex}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := func(read func(func(trace.Session) error) error) (n int) {
				if err := read(func(trace.Session) error { n++; return nil }); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if n := count(func(y func(trace.Session) error) error { return tc.r.Each(time.Time{}, y) }); n != tc.plain {
				t.Errorf("plain read kept %d sessions, want %d", n, tc.plain)
			}
			if n := count(func(y func(trace.Session) error) error { return tc.r.EachWith(time.Time{}, ReadOptions{}, y) }); n != tc.plain {
				t.Errorf("read without KeepEmpty kept %d sessions, want %d", n, tc.plain)
			}
			if n := count(func(y func(trace.Session) error) error {
				return tc.r.EachWith(time.Time{}, ReadOptions{KeepEmpty: true, Conversation: true}, y)
			}); n != tc.keep {
				t.Errorf("KeepEmpty kept %d sessions, want %d", n, tc.keep)
			}
		})
	}
}

// Every bounded field is redacted whole first, and with Full also kept
// whole: a credential cut at a bound cannot survive as a prefix the shape no
// longer matches, and a nested password stays recognisable by its name
// because its JSON is still whole when it is redacted.
func TestFullFieldsAreRedactedBeforeEveryBound(t *testing.T) {
	tok := fakeGitHubToken()
	pad := func(n int) string { return strings.Repeat("z ", n/2) }
	request := pad(4000-10) + tok
	output := pad(600-10) + tok + " tail"
	nested, _ := json.Marshal(map[string]any{"notes": pad(MaxArg), "config": map[string]any{"password": "hunter22x"}})
	longArg := pad(MaxArg-10) + tok

	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude", "p", "s.jsonl")
	writeJSONL(t, claudePath, []map[string]any{
		{"type": "user", "timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"role": "user", "content": request}},
		{"type": "assistant", "timestamp": "2026-10-01T10:00:01Z", "message": map[string]any{"id": "m", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "mcp__srv__put", "input": map[string]any{"body": json.RawMessage(nested), "text": longArg}},
		}}},
		{"type": "user", "timestamp": "2026-10-01T10:00:02Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": output},
		}}},
	})
	codexPath := filepath.Join(dir, "codex", "rollout-s.jsonl")
	argJSON, _ := json.Marshal(map[string]any{"body": json.RawMessage(nested), "text": longArg})
	writeJSONL(t, codexPath, []map[string]any{
		{"timestamp": "2026-10-01T10:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "s"}},
		{"timestamp": "2026-10-01T10:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": request}}}},
		{"timestamp": "2026-10-01T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "function_call", "namespace": "mcp__srv__", "name": "put", "arguments": string(argJSON), "call_id": "c1"}},
		{"timestamp": "2026-10-01T10:00:02Z", "type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "c1", "output": output}},
	})
	for _, tc := range []struct {
		name string
		read func(string, ReadOptions) (trace.Session, error)
		path string
	}{
		{"claude-code", ReadClaudeFileWith, claudePath},
		{"codex", ReadCodexFileWith, codexPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain, err := tc.read(tc.path, ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			full, err := tc.read(tc.path, ReadOptions{Full: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(full.Calls) != 1 || len(full.FullRequests) != 1 {
				t.Fatalf("session = %d calls, %d full requests", len(full.Calls), len(full.FullRequests))
			}
			if plain.FullRequests != nil || plain.Calls[0].FullArgs != nil || plain.Calls[0].FullOutput != "" {
				t.Errorf("whole fields kept without Full")
			}
			c := full.Calls[0]
			bounded := map[string]string{
				"request": full.Requests[0], "output": c.Output,
				"args.body": c.Args["body"], "args.text": c.Args["text"],
			}
			whole := map[string]string{
				"full request": full.FullRequests[0], "full output": c.FullOutput,
				"full args.body": c.FullArgs["body"], "full args.text": c.FullArgs["text"],
			}
			for _, m := range []map[string]string{bounded, whole} {
				for name, v := range m {
					if strings.Contains(v, "ghp_") || strings.Contains(v, "hunter22x") {
						t.Errorf("%s kept a credential or part of one", name)
					}
				}
			}
			if len(c.Args["text"]) > MaxArg || len(full.Requests[0]) > 4000 || len(c.Output) > 600 {
				t.Errorf("bounded fields exceed their bounds")
			}
			if len(c.FullArgs["text"]) <= MaxArg || len(full.FullRequests[0]) <= 4000 || !strings.HasSuffix(c.FullOutput, " tail") {
				t.Errorf("whole fields were cut")
			}
		})
	}
}
