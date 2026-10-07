package history

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// fixtureParsers are the cached parsers with a recorded session file each.
var fixtureParsers = []struct {
	cache string
	file  string
	parse FileParser
}{
	{"claude-code", "testdata/claude/proj/s1.jsonl", parseClaude},
	{"codex", "testdata/codex/2026/09/27/rollout-2026-09-27T10-00-00-c1.jsonl", parseCodex},
	{"gemini-cli", "testdata/gemini-cli/tmp/proj/chats/session-2026-10-01T10-00-5f2c.jsonl", parseGemini},
	{"qwen-code", "testdata/qwen-code/projects/-tmp-proj/chats/7a1d0000-0000-4000-8000-000000000002.jsonl", parseQwen},
	{"vscode-copilot", "testdata/vscode-copilot/User/workspaceStorage/ws1/chatSessions/9c3e0000-0000-4000-8000-000000000003.jsonl", parseVSCode},
}

// useTempCache points the parse cache at a fresh directory for one test.
func useTempCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	UseCache(dir)
	t.Cleanup(func() { UseCache("") })
	return dir
}

// copyFixture copies a fixture into dir, so a test can change it.
func copyFixture(t *testing.T, src, dir string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// One read gives the parse and the digest: the digest is the file's sha256,
// as FileDigest computes it separately.
func TestParseFileDigestsTheBytesItParses(t *testing.T) {
	for _, fx := range fixtureParsers {
		s, err := ParseFile(fx.file, fx.parse)
		if err != nil {
			t.Fatalf("%s: %v", fx.cache, err)
		}
		if want := FileDigest(fx.file); s.SourceDigest != want || want == "" {
			t.Errorf("%s: digest %q, want %q", fx.cache, s.SourceDigest, want)
		}
	}
}

// A parser that stops early still gets the whole file's digest.
func TestParseFileDigestsWhatTheParserLeftUnread(t *testing.T) {
	file := fixtureParsers[0].file
	s, err := ParseFile(file, func(string, io.Reader) (trace.Session, error) { return trace.Session{ID: "x"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if s.SourceDigest != FileDigest(file) {
		t.Errorf("digest %q, want the whole file's %q", s.SourceDigest, FileDigest(file))
	}
}

// A session served from the cache is exactly the one the parser returned:
// same calls, same nil and empty fields, same digest.
func TestCachedSessionEqualsAFreshParse(t *testing.T) {
	for _, fx := range fixtureParsers {
		useTempCache(t)
		dir := t.TempDir()
		file := copyFixture(t, fx.file, dir)
		fresh, err := ParseFile(file, fx.parse)
		if err != nil {
			t.Fatalf("%s: %v", fx.cache, err)
		}
		ParseFiles([]string{file}, fx.cache, nil, fx.parse) // writes the cache
		calls := 0
		counting := func(path string, r io.Reader) (trace.Session, error) { calls++; return fx.parse(path, r) }
		got := ParseFiles([]string{file}, fx.cache, nil, counting)
		if calls != 0 {
			t.Errorf("%s: unchanged file parsed again", fx.cache)
		}
		if got[0].Err != nil || !reflect.DeepEqual(got[0].Session, fresh) {
			t.Errorf("%s: cached session differs from a fresh parse:\ncached %+v\nfresh  %+v", fx.cache, got[0].Session, fresh)
		}
	}
}

// A file that changes size or time is parsed again; one that is deleted
// leaves the cache.
func TestCacheReparsesChangedFilesAndForgetsDeletedOnes(t *testing.T) {
	cacheDir := useTempCache(t)
	fx := fixtureParsers[0]
	file := copyFixture(t, fx.file, t.TempDir())
	calls := 0
	counting := func(path string, r io.Reader) (trace.Session, error) { calls++; return fx.parse(path, r) }
	ParseFiles([]string{file}, fx.cache, nil, counting)

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	ParseFiles([]string{file}, fx.cache, nil, counting)
	if calls != 2 {
		t.Fatalf("touched file: %d parses, want 2", calls)
	}
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n")
	f.Close()
	os.Chtimes(file, later, later) // same time, new size
	ParseFiles([]string{file}, fx.cache, nil, counting)
	if calls != 3 {
		t.Fatalf("grown file: %d parses, want 3", calls)
	}

	os.Remove(file)
	ParseFiles(nil, fx.cache, nil, counting)
	if c := openCache(fx.cache); len(c.entries) != 0 {
		t.Errorf("deleted file still cached: %v", c.entries)
	}
	if info, err := os.Stat(filepath.Join(cacheDir, fx.cache+".gob")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode: %v %v, want private", info, err)
	}
}

// Parse failures are not cached: a file that failed once is tried again.
func TestCacheDoesNotKeepFailures(t *testing.T) {
	useTempCache(t)
	file := copyFixture(t, fixtureParsers[0].file, t.TempDir())
	calls := 0
	failing := func(string, io.Reader) (trace.Session, error) { calls++; return trace.Session{}, errors.New("no") }
	for i := 0; i < 2; i++ {
		if r := ParseFiles([]string{file}, "claude-code", nil, failing); r[0].Err == nil {
			t.Fatal("failure not returned")
		}
	}
	if calls != 2 {
		t.Errorf("%d parses, want 2", calls)
	}
}

// A cache written by another build is ignored.
func TestCacheFromAnotherBuildIsIgnored(t *testing.T) {
	useTempCache(t)
	fx := fixtureParsers[0]
	file := copyFixture(t, fx.file, t.TempDir())
	ParseFiles([]string{file}, fx.cache, nil, fx.parse)
	c := openCache(fx.cache)
	c.binary, c.dirty = "another build", true
	c.save()
	if c := openCache(fx.cache); len(c.entries) != 0 {
		t.Errorf("entries from another build were loaded: %d", len(c.entries))
	}
}

// Results come back in file order, every file is parsed once, and no more
// files are parsed at once than the process-wide bound allows, however
// many readers parse together.
func TestParseFilesKeepsOrderAndTheBound(t *testing.T) {
	var files []string
	for i := 0; i < 64; i++ {
		files = append(files, fixtureParsers[0].file)
	}
	var inFlight, peak atomic.Int64
	var mu sync.Mutex
	parsed := map[int]int{}
	n := 0
	parse := func(path string, r io.Reader) (trace.Session, error) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); cur > p && !peak.CompareAndSwap(p, cur); p = peak.Load() {
		}
		time.Sleep(time.Millisecond)
		mu.Lock()
		n++
		id := n
		mu.Unlock()
		return trace.Session{ID: path, Start: time.Unix(int64(id), 0)}, nil
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := ParseFiles(files, "", nil, parse)
			mu.Lock()
			for i, r := range got {
				if r.Session.ID != files[i] {
					t.Errorf("result %d is %s", i, r.Session.ID)
				}
				parsed[int(r.Session.Start.Unix())]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(parsed) != 3*len(files) {
		t.Errorf("%d distinct parses, want %d", len(parsed), 3*len(files))
	}
	if p := peak.Load(); p > int64(cap(parseSlots)) {
		t.Errorf("%d files parsed at once, bound is %d", p, cap(parseSlots))
	}
}
