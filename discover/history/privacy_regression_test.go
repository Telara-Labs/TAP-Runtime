package history

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"encoding/json"
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
		{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "input": `await tools.put({config: {'pass\u0077ord':'tiny', nested: [{token: 'small'}], templ: unknown("template-private")}, other: derive('hidden')})`, "call_id": "c"}},
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
	for _, secret := range []string{"tiny", "small", "hidden", "template-private"} {
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

func TestStoreReadersMarkCompleteArgumentsBeforeBounding(t *testing.T) {
	input := map[string]json.RawMessage{"config": json.RawMessage(`"` + strings.Repeat("x", MaxArg-10) + " " + fakeGitHubToken() + `"`)}
	for _, test := range []struct {
		name string
		tool func(*trace.Call)
	}{
		{"cursor-cli", func(c *trace.Call) { CursorCLITool(c, "Read", input) }},
		{"open-code", func(c *trace.Call) { OpenCodeTool(c, "read", input, nil) }},
		{"goose", func(c *trace.Call) { GooseTool(c, "read", input, "") }},
		{"crush", func(c *trace.Call) { CrushTool(c, "read", input, nil) }},
		{"mcp", func(c *trace.Call) { MCPCall(c, "synthetic", "read", input) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var call trace.Call
			test.tool(&call)
			if !call.CacheUnsafe {
				t.Fatal("complete input contamination lost")
			}
			if !holdsCredential([]trace.Session{{Calls: []trace.Call{call}}}) {
				t.Fatal("contaminated call can enter cache")
			}
		})
	}
	a := NewAssembler("synthetic", "s")
	a.Add(UserText{Text: strings.Repeat("x", 3990) + " " + fakeGitHubToken()})
	if !a.Finish().CacheUnsafe {
		t.Fatal("full user text contamination lost")
	}
}

func TestDuplicateCodexSessionCannotFallbackToCredentialFilename(t *testing.T) {
	dir := t.TempDir()
	lines := []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "s"}},
		{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"ls"}`, "call_id": "c"}},
	}
	writeJSONL(t, filepath.Join(dir, "a.jsonl"), lines)
	writeJSONL(t, filepath.Join(dir, fakeGitHubToken()+".jsonl"), lines)
	if err := (Codex{Dir: dir}).EachWith(time.Time{}, ReadOptions{}, func(s trace.Session) error {
		if strings.Contains(s.ID, fakeGitHubToken()) {
			t.Fatal("fallback identifier contains credential")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestJSLiteralsDecodeEscapedKeysAndOmitTemplates(t *testing.T) {
	for _, src := range []string{`{config:{'pass\u0077ord':'tiny'}}`, `{config:{"pass\x77ord":"tiny"}}`} {
		value := jsLiteralData(src)
		raw, _ := json.Marshal(value)
		clean := redactText(string(raw))
		if strings.Contains(clean, "tiny") {
			t.Fatal("escaped secret property survived", clean)
		}
	}
	value := jsLiteralData("{config: `text ${'template-private'}`} ")
	raw, _ := json.Marshal(value)
	if strings.Contains(string(raw), "template-private") {
		t.Fatal("template source retained")
	}
}

func FuzzJSLiteralData(f *testing.F) {
	for _, seed := range []string{`{config:{'pass\u0077ord':'tiny'}}`, `{value:[1,true,null,{k:'x'}]}`, "{data: `template ${secret}`}"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 65536 {
			return
		}
		value := jsLiteralData(src)
		if _, err := json.Marshal(value); err != nil {
			t.Fatalf("literal parser returned invalid data: %v", err)
		}
	})
}

func TestPlainCodexCacheAuditsDecodedLiteralKeys(t *testing.T) {
	cache := useTempCache(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-s.jsonl")
	writeJSONL(t, path, []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "s"}},
		{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "input": `await tools.put({config:{'pass\u0077ord':'tiny'}})`, "call_id": "c"}},
	})
	os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	s, err := (Codex{Dir: dir}).Read(time.Time{})
	if err != nil || len(s) != 1 {
		t.Fatal("read failed", err)
	}
	if !strings.Contains(s[0].Calls[0].Args["config"], "tiny") {
		t.Fatal("ordinary read representation changed")
	}
	if blobs, _ := filepath.Glob(filepath.Join(cache, "codex", "*.gob")); len(blobs) != 0 {
		t.Fatal("decoded JS secret was cached")
	}
}

func TestCredentialCacheKeysAreNotPersisted(t *testing.T) {
	cache := useTempCache(t)
	c := openUnitCache("synthetic")
	key := fakeGitHubToken()
	c.put(key, "fp", []trace.Session{{ID: "safe"}})
	c.setMark(key, 1)
	c.setDir(key, walkDir{})
	c.setDir("safe", walkDir{Subdirs: []string{key}})
	c.save()
	if cacheHolds(t, cache, key) {
		t.Fatal("cache bookkeeping retained credential key")
	}
}
