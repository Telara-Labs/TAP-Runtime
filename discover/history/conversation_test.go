package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

const (
	convClaude = "testdata/conversation/claude/proj/c1.jsonl"
	convCodex  = "testdata/conversation/codex/2026/10/01/rollout-2026-10-01T10-00-00-0199aaaa-0000-7000-8000-000000000001.jsonl"
)

// fakeGitHubToken is built at run time so no credential-shaped literal sits
// in the tree.
func fakeGitHubToken() string { return "ghp_" + strings.Repeat("Q7", 18) }

func TestConversationEventsAreKeptWhenAsked(t *testing.T) {
	cases := []struct {
		name      string
		read      func(string, ReadOptions) (trace.Session, error)
		path      string
		requests  []string
		assistant []trace.AssistantMessage
		meta      []trace.SessionMeta
	}{
		{
			name:     "claude-code",
			read:     ReadClaudeFileWith,
			path:     convClaude,
			requests: []string{"why does the build fail", "now explain the fix in detail"},
			assistant: []trace.AssistantMessage{
				{Request: 0, AfterCall: 0, Turn: "msg:m1", Text: "Let me look at the build log."},
				{Request: 0, AfterCall: 1, Turn: "msg:m2", Text: "The module foo is missing from go.mod.", Final: true},
				{Request: 1, AfterCall: 1, Turn: "msg:m3", Text: "Add the require line."},
				{Request: 1, AfterCall: 1, Turn: "msg:m3", Text: "Then run go mod tidy.", Final: true},
			},
			meta: []trace.SessionMeta{
				{Request: -1, AfterCall: 0, Cwd: "/work/app", Branch: "main", Version: "2.1.0"},
				{Request: 0, AfterCall: 0, Cwd: "/work/app", Branch: "main", Version: "2.1.0", Model: "model-a"},
				{Request: 0, AfterCall: 1, Cwd: "/work/app", Branch: "fix-build", Version: "2.1.0", Model: "model-a"},
				{Request: 1, AfterCall: 1, Cwd: "/work/app", Branch: "fix-build", Version: "2.1.0", Model: "model-b"},
			},
		},
		{
			name:     "codex",
			read:     ReadCodexFileWith,
			path:     convCodex,
			requests: []string{"list the files"},
			assistant: []trace.AssistantMessage{
				{Request: 0, AfterCall: 0, Text: "Listing them now."},
				{Request: 0, AfterCall: 1, Text: "There are two files, a.go and b.go.", Final: true},
			},
			meta: []trace.SessionMeta{
				{Request: -1, AfterCall: 0, Cwd: "/work/app", Branch: "main", Version: "0.150.0", Model: "model-c"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.read(tc.path, ReadOptions{Conversation: true})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.Requests, tc.requests) {
				t.Errorf("requests = %q, want %q", s.Requests, tc.requests)
			}
			if !reflect.DeepEqual(s.Assistant, tc.assistant) {
				t.Errorf("assistant =\n%+v\nwant\n%+v", s.Assistant, tc.assistant)
			}
			if !reflect.DeepEqual(s.Meta, tc.meta) {
				t.Errorf("meta =\n%+v\nwant\n%+v", s.Meta, tc.meta)
			}
		})
	}
}

func TestPlainReadIsUnchangedByConversationEvents(t *testing.T) {
	for _, tc := range []struct {
		path  string
		plain func(string) (trace.Session, error)
		with  func(string, ReadOptions) (trace.Session, error)
	}{
		{convClaude, ReadClaudeFile, ReadClaudeFileWith},
		{convCodex, ReadCodexFile, ReadCodexFileWith},
	} {
		plain, err := tc.plain(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if plain.Assistant != nil || plain.Meta != nil {
			t.Fatalf("%s: a plain read kept conversation events", tc.path)
		}
		with, err := tc.with(tc.path, ReadOptions{Conversation: true})
		if err != nil {
			t.Fatal(err)
		}
		with.Assistant, with.Meta = nil, nil
		if with.SourceDigest != "" {
			t.Fatalf("%s: options read retained a digest of raw text", tc.path)
		}
		plain.SourceDigest = ""
		if !reflect.DeepEqual(plain, with) {
			t.Fatalf("%s: the calls and requests differ between a plain read and a read with options:\n%+v\n%+v", tc.path, plain, with)
		}
	}
}

func TestConversationTextIsRedactedBeforeItIsBounded(t *testing.T) {
	tok := fakeGitHubToken()
	// In the request the token straddles the 4000-byte bound: cutting first
	// would leave a prefix the token shape no longer matches.
	prose := "the key is " + tok + " done"
	request := strings.Repeat("y", 4000-10) + " " + tok
	dir := t.TempDir()
	path := filepath.Join(dir, "red.jsonl")
	lines := []map[string]any{
		{"type": "user", "timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"role": "user", "content": request}},
		{"type": "assistant", "timestamp": "2026-10-01T10:00:01Z", "cwd": "/home/u/" + tok, "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "text", "text": prose}}}},
	}
	writeJSONL(t, path, lines)
	s, err := ReadClaudeFileWith(path, ReadOptions{Conversation: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Assistant) != 1 || len(s.Requests) != 1 || len(s.Meta) != 1 {
		t.Fatalf("session = %+v", s)
	}
	for name, text := range map[string]string{"assistant": s.Assistant[0].Text, "request": s.Requests[0], "cwd": s.Meta[0].Cwd} {
		if strings.Contains(text, tok[:8]) {
			t.Errorf("%s kept part of the token", name)
		}
	}
}

func writeJSONL(t *testing.T, path string, lines []map[string]any) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		j, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}
