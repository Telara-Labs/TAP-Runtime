package history

import (
	"bytes"
	"encoding/gob"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

type tailer func(string, *TailState, ReadOptions) (trace.Session, TailState, error)

var tailCases = []struct {
	name  string
	path  string
	tail  tailer
	whole func(string, ReadOptions) (trace.Session, error)
}{
	{"claude conversation", convClaude, TailClaudeFile, ReadClaudeFileWith},
	{"claude subagent", "testdata/conversation/claude/proj/c1/subagents/agent-a1.jsonl", TailClaudeFile, ReadClaudeFileWith},
	{"claude", "testdata/claude/proj/s1.jsonl", TailClaudeFile, ReadClaudeFileWith},
	{"codex conversation", convCodex, TailCodexFile, ReadCodexFileWith},
	{"codex", "testdata/codex/2026/09/27/rollout-2026-09-27T10-00-00-c1.jsonl", TailCodexFile, ReadCodexFileWith},
	{"codex 0.147", "testdata/codex-0147/sessions/2026/10/04/rollout-2026-10-04T14-12-02-01a1081d-a3fc-7340-8317-617de7c8f7c3.jsonl", TailCodexFile, ReadCodexFileWith},
}

// regob passes a state through encoding/gob, as a caller keeping it would.
func regob(t *testing.T, st TailState) *TailState {
	t.Helper()
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(st); err != nil {
		t.Fatal(err)
	}
	var out TailState
	if err := gob.NewDecoder(&b).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// Tailing a transcript in two reads, split at any line and resumed from a
// stored checkpoint, gives the session one whole read gives.
func TestTailFromACheckpointEqualsAWholeRead(t *testing.T) {
	o := ReadOptions{Conversation: true, Full: true, KeepEmpty: true}
	for _, tc := range tailCases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(src, []byte("\n")) {
				src = append(src, '\n')
			}
			// The file name names the session (and a subagent's parent).
			path := filepath.Join(t.TempDir(), tc.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, src, 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := tc.whole(path, o)
			if err != nil {
				t.Fatal(err)
			}
			want.SourceDigest = ""
			cuts := []int{0}
			for i, c := range src {
				if c == '\n' {
					cuts = append(cuts, i+1)
				}
			}
			for _, cut := range cuts {
				// The first read sees the lines before cut and half of the
				// next one, still being written.
				half := cut + (len(src)-cut)/2
				if err := os.WriteFile(path, src[:half], 0o600); err != nil {
					t.Fatal(err)
				}
				_, st, err := tc.tail(path, nil, o)
				if err != nil {
					t.Fatal(err)
				}
				if want := int64(bytes.LastIndexByte(src[:half], '\n') + 1); st.Offset != want {
					t.Fatalf("cut %d: offset %d, want %d", cut, st.Offset, want)
				}
				if err := os.WriteFile(path, src, 0o600); err != nil {
					t.Fatal(err)
				}
				got, _, err := tc.tail(path, regob(t, st), o)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("cut at %d: tailed session differs from a whole read:\n%+v\nwant\n%+v", cut, got, want)
				}
			}
		})
	}
}

// A file replaced or cut since the checkpoint is read again from its start.
func TestTailRestartsOnAReplacedFile(t *testing.T) {
	o := ReadOptions{Conversation: true}
	dir := t.TempDir()
	path := filepath.Join(dir, "p", "s.jsonl")
	writeJSONL(t, path, []map[string]any{
		{"type": "user", "timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"role": "user", "content": "first version of the session"}},
		{"type": "assistant", "timestamp": "2026-10-01T10:00:01Z", "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "text", "text": "old answer"}}}},
	})
	_, st, err := TailClaudeFile(path, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	for name, lines := range map[string][]map[string]any{
		"replaced": {
			{"type": "user", "timestamp": "2026-10-02T10:00:00Z", "message": map[string]any{"role": "user", "content": "another session entirely, with a longer first line"}},
			{"type": "assistant", "timestamp": "2026-10-02T10:00:01Z", "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "text", "text": "new answer"}}}},
			{"type": "assistant", "timestamp": "2026-10-02T10:00:02Z", "message": map[string]any{"id": "n", "content": []any{map[string]any{"type": "text", "text": "and more"}}}},
		},
		"cut": {
			{"type": "user", "timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"role": "user", "content": "first version of the session"}},
		},
	} {
		writeJSONL(t, path, lines)
		got, _, err := TailClaudeFile(path, &st, o)
		if err != nil {
			t.Fatal(err)
		}
		want, err := ReadClaudeFileWith(path, o)
		if err != nil {
			t.Fatal(err)
		}
		want.SourceDigest = ""
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

// What a tail keeps between reads is redacted: a result recorded ahead of
// the call's output, and an exec cell's source, are not kept as written.
func TestTailStateKeepsNoCredential(t *testing.T) {
	tok := fakeGitHubToken()
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	writeJSONL(t, path, []map[string]any{
		{"timestamp": "2026-10-01T10:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "x"}},
		{"timestamp": "2026-10-01T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "deploy with " + tok}}}},
		{"timestamp": "2026-10-01T10:00:02Z", "type": "response_item", "payload": map[string]any{"type": "function_call", "namespace": "mcp__srv__", "name": "get", "arguments": `{"token":"` + tok + `"}`, "call_id": "c1"}},
		{"timestamp": "2026-10-01T10:00:03Z", "type": "event_msg", "payload": map[string]any{"type": "mcp_tool_call_end", "call_id": "c1", "result": map[string]any{"Ok": map[string]any{"content": []any{map[string]any{"type": "text", "text": "key " + tok}}}}}},
		{"timestamp": "2026-10-01T10:00:04Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "input": `const r = await tools.get({token: "` + tok + `"}); text(r);`, "call_id": "c2"}},
	})
	_, st, err := TailCodexFile(path, nil, ReadOptions{Full: true, Conversation: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Codex.MCPEnds) != 1 || len(st.Codex.Exec) != 1 {
		t.Fatalf("state = %+v", st.Codex)
	}
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), tok[:10]) {
		t.Fatal("the encoded tail state holds the credential")
	}
}
