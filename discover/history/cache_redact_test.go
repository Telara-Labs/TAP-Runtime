package history

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// cacheHolds reports whether any file under dir contains s.
func cacheHolds(t *testing.T, dir, s string) bool {
	t.Helper()
	found := false
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), s) {
			found = true
		}
		return nil
	})
	return found
}

func cacheFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func secretSession(t *testing.T, path, tok string) {
	t.Helper()
	writeJSONL(t, path, []map[string]any{
		{"type": "user", "timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"role": "user", "content": "call the api"}},
		{"type": "assistant", "timestamp": "2026-10-01T10:00:01Z", "message": map[string]any{"id": "m", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "curl -H 'Authorization: Bearer " + tok + "' https://api.example.com"}},
		}}},
		{"type": "user", "timestamp": "2026-10-01T10:00:02Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		}}},
	})
}

func cleanSession(t *testing.T, path string) {
	t.Helper()
	writeJSONL(t, path, []map[string]any{
		{"type": "user", "timestamp": "2026-10-01T09:00:00Z", "message": map[string]any{"role": "user", "content": "list files"}},
		{"type": "assistant", "timestamp": "2026-10-01T09:00:01Z", "message": map[string]any{"id": "m", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}}},
	})
}

// A synthetic credential in a session never reaches the cache: the unit
// holding it is parsed afresh on every read, and what discover reads is the
// same either way.
func TestCacheNeverHoldsACredential(t *testing.T) {
	cache := useTempCache(t)
	reads := readsOf(t)
	tok := fakeGitHubToken()
	dir := t.TempDir()
	secret, clean := filepath.Join(dir, "p", "secret.jsonl"), filepath.Join(dir, "p", "clean.jsonl")
	secretSession(t, secret, tok)
	cleanSession(t, clean)
	r := ClaudeCode{Dir: dir}
	first, err := r.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reads("claude-code"); len(got) != 2 {
		t.Fatalf("first read read %v", got)
	}
	second, err := r.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reads("claude-code"); !reflect.DeepEqual(got, []string{secret}) {
		t.Fatalf("second read read %v, want only the unit holding a credential", got)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("a cached read differs from a fresh one")
	}
	if !strings.Contains(first[len(first)-1].Calls[0].Command+first[0].Calls[0].Command, tok) {
		t.Fatal("discover's own read no longer sees the command as written")
	}
	if cacheHolds(t, cache, tok[:10]) || cacheHolds(t, cache, "Bearer") {
		t.Fatal("the cache holds the credential")
	}
	if !cacheHolds(t, cache, "list files") {
		t.Fatal("the clean unit was not cached")
	}

	// A unit cached while clean and then given a credential leaves the
	// cache.
	before := cacheFiles(t, cache)
	secretSession(t, clean, tok)
	os.Chtimes(clean, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))
	if _, err := r.Read(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if cacheHolds(t, cache, tok[:10]) || cacheHolds(t, cache, "list files") {
		t.Fatal("the cache kept the unit's earlier copy or its credential")
	}
	if after := cacheFiles(t, cache); after != before-1 {
		t.Fatalf("cache files %d -> %d, want the unit's file removed", before, after)
	}
}

// A read with options, and any read with the cache turned off, writes
// nothing to disk.
func TestReadsWithoutTheCacheWriteNothing(t *testing.T) {
	dir := t.TempDir()
	cleanSession(t, filepath.Join(dir, "p", "clean.jsonl"))
	codex := t.TempDir()
	writeJSONL(t, filepath.Join(codex, "rollout-x.jsonl"), []map[string]any{
		{"timestamp": "2026-10-01T10:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "x"}},
		{"timestamp": "2026-10-01T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"ls"}`, "call_id": "c"}},
	})
	count := func(each func(func(trace.Session) error) error) int {
		n := 0
		if err := each(func(trace.Session) error { n++; return nil }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("options", func(t *testing.T) {
		cache := useTempCache(t)
		o := ReadOptions{Conversation: true, Full: true, KeepEmpty: true, Subagents: true}
		n := count(func(y func(trace.Session) error) error { return ClaudeCode{Dir: dir}.EachWith(time.Time{}, o, y) })
		n += count(func(y func(trace.Session) error) error { return Codex{Dir: codex}.EachWith(time.Time{}, o, y) })
		if n != 2 {
			t.Fatalf("read %d sessions", n)
		}
		if f := cacheFiles(t, cache); f != 0 {
			t.Fatalf("a read with options wrote %d cache files", f)
		}
	})
	t.Run("cache off", func(t *testing.T) {
		cache := useTempCache(t)
		UseCache("")
		if n := count(func(y func(trace.Session) error) error { return ClaudeCode{Dir: dir}.Each(time.Time{}, y) }); n != 1 {
			t.Fatalf("read %d sessions", n)
		}
		if f := cacheFiles(t, cache); f != 0 {
			t.Fatalf("a read with the cache off wrote %d cache files", f)
		}
	})
}
