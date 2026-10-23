package history

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestCacheAuditsCompleteSourceBeforeTruncation(t *testing.T) {
	for _, text := range []string{"DB_PASSWORD=abc ls", strings.Repeat("x", 3990) + " " + fakeGitHubToken()} {
		t.Run(text[:8], func(t *testing.T) {
			cache := useTempCache(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "p", "s.jsonl")
			writeJSONL(t, path, []map[string]any{
				{"type": "user", "timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"content": text}},
				{"type": "assistant", "timestamp": "2026-10-01T10:00:01Z", "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "tool_use", "id": "c", "name": "Bash", "input": map[string]any{"command": "ls"}}}}},
			})
			os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
			r := ClaudeCode{Dir: dir}
			a, err := r.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := r.Read(time.Time{})
			if err != nil || !reflect.DeepEqual(a, b) {
				t.Fatal("cache policy changed returned sessions", err)
			}
			if blobs, _ := filepath.Glob(filepath.Join(cache, "claude-code", "*.gob")); len(blobs) != 0 {
				t.Fatal("unsafe source cached", blobs)
			}
		})
	}
}

func TestInvalidatedCacheRemovesOldUnitBlobs(t *testing.T) {
	dir := useTempCache(t)
	c := openUnitCache("old")
	key := "s"
	c.put(key, "fp", []trace.Session{{Client: "claude-code", ID: "s", Requests: []string{"safe"}}})
	c.save()
	path := c.unitFile(key)
	if err := os.WriteFile(path, []byte(fakeGitHubToken()), 0600); err != nil {
		t.Fatal(err)
	}
	var cf cacheFile
	fh, _ := os.Open(c.path)
	gob.NewDecoder(fh).Decode(&cf)
	fh.Close()
	cf.Binary = "obsolete"
	if err := writePrivate(dir, c.path, cf); err != nil {
		t.Fatal(err)
	}
	next := openUnitCache("old")
	next.put(key, "fp", []trace.Session{{Requests: []string{fakeGitHubToken()}}})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("obsolete secret blob survives", err)
	}
}

func TestOptionsOmitOpaqueJSAndRedactQuotedNestedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-s.jsonl")
	writeJSONL(t, path, []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "s"}},
		{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "input": `await tools.put({config: {'password':'tiny', nested: [{token: 'small'}]}, other: derive('hidden')})`, "call_id": "c"}},
	})
	s, st, err := TailCodexFile(path, nil, ReadOptions{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Calls) != 1 {
		t.Fatal("missing literal call")
	}
	var b bytes.Buffer
	gob.NewEncoder(&b).Encode(st)
	for _, secret := range []string{"tiny", "small", "hidden"} {
		if strings.Contains(b.String(), secret) {
			t.Fatal("JS credential/expression retained", secret)
		}
	}
	if !strings.Contains(s.Calls[0].FullArgs["config"], "redacted") {
		t.Fatal("literal structure lost", s.Calls[0].FullArgs)
	}
}

func TestOptionsRejectCredentialIdentifiers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-s.jsonl")
	tok := fakeGitHubToken()
	writeJSONL(t, path, []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "s"}},
		{"type": "event_msg", "payload": map[string]any{"type": "mcp_tool_call_end", "call_id": tok, "result": map[string]any{"Err": "x"}}},
		{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "get", "call_id": tok, "arguments": "{}"}},
	})
	_, st, err := TailCodexFile(path, nil, ReadOptions{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	gob.NewEncoder(&b).Encode(st)
	if strings.Contains(b.String(), tok) {
		t.Fatal("identifier survived in checkpoint")
	}
	if len(st.Assembler.Session.Calls) != 0 || len(st.Codex.MCPEnds) != 0 {
		t.Fatal("invalid identifier was accepted")
	}
}

func TestTailRestartsForChangedPrefixSourceAndOptions(t *testing.T) {
	o := ReadOptions{Conversation: true, Full: true}
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	original := `{"type":"user","message":{"content":"old"}}` + "\n" + `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"old"}]}}` + "\n"
	os.WriteFile(path, []byte(original), 0600)
	_, st, err := TailClaudeFile(path, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	replaced := strings.ReplaceAll(original, "old", "new")
	os.WriteFile(path, []byte(replaced), 0600)
	got, _, err := TailClaudeFile(path, &st, o)
	want, _ := ReadClaudeFileWith(path, o)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("equal-length replacement reused stale state", err)
	}
	other := filepath.Join(dir, "other.jsonl")
	os.WriteFile(other, []byte(replaced), 0600)
	got, _, err = TailClaudeFile(other, &st, o)
	want, _ = ReadClaudeFileWith(other, o)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("cross-source state reused", err)
	}
	got, _, err = TailClaudeFile(path, &st, ReadOptions{})
	want, _ = ReadClaudeFileWith(path, ReadOptions{})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("changed options ignored", err)
	}
}

func TestTailRejectsOversizedIncompleteRecord(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 1025)), 64)
	if _, err := readTailLine(r, 1024); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatal("unbounded incomplete record", err)
	}
}
