package history

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// readsOf records the units each cache read from their source, not from the
// cache, while a test runs.
func readsOf(t *testing.T) func(cache string) []string {
	t.Helper()
	var mu sync.Mutex
	reads := map[string][]string{}
	unitReadHook = func(cache, unit string) {
		mu.Lock()
		reads[cache] = append(reads[cache], unit)
		mu.Unlock()
	}
	t.Cleanup(func() { unitReadHook = nil })
	return func(cache string) []string {
		mu.Lock()
		defer mu.Unlock()
		out := reads[cache]
		reads[cache] = nil
		sort.Strings(out)
		return out
	}
}

func sqlite(t *testing.T, db, sql string) string {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	out, err := exec.Command(bin, db, sql).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v %s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cursorID is a 36-character conversation ID, as Cursor's keys hold.
func cursorID(n int) string { return fmt.Sprintf("%08d-0000-4000-8000-000000000000", n) }

// buildCursorStore writes a store with one conversation per entry of convs,
// each holding the given shell commands as tool bubbles.
func buildCursorStore(t *testing.T, db string, convs [][]string) {
	t.Helper()
	sql := "CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);\n"
	for n, cmds := range convs {
		id := cursorID(n + 1)
		var headers []map[string]any
		for i := range cmds {
			headers = append(headers, map[string]any{"bubbleId": fmt.Sprintf("b%d", i), "type": 2})
		}
		conv, _ := json.Marshal(map[string]any{"createdAt": 1790000000000 + int64(n)*1000, "fullConversationHeadersOnly": headers})
		sql += fmt.Sprintf("INSERT INTO cursorDiskKV VALUES ('composerData:%s', '%s');\n", id, conv)
		for i, cmd := range cmds {
			sql += cursorBubbleSQL(id, i, cmd)
		}
	}
	sqlite(t, db, sql)
}

func cursorBubbleSQL(conv string, i int, cmd string) string {
	params, _ := json.Marshal(map[string]string{"command": cmd})
	b, _ := json.Marshal(map[string]any{"createdAt": "2026-09-21T10:00:00Z", "toolFormerData": map[string]any{
		"name": "run_terminal_command_v2", "rawArgs": "", "params": string(params), "status": "completed", "result": `{"output":"ok"}`}})
	return fmt.Sprintf("INSERT INTO cursorDiskKV VALUES ('bubbleId:%s:b%d', '%s');\n", conv, i, strings.ReplaceAll(string(b), "'", "''"))
}

// cacheCase is one reader over its recorded fixture, and the cache it uses.
type cacheCase struct {
	cache string
	build func(t *testing.T) trace.Reader
}

func cacheCases() []cacheCase {
	return []cacheCase{
		{"claude-code", func(*testing.T) trace.Reader { return ClaudeCode{Dir: "testdata/claude"} }},
		{"codex", func(*testing.T) trace.Reader { return Codex{Dir: "testdata/codex"} }},
		{"gemini-cli", func(*testing.T) trace.Reader { return GeminiCLI{Dir: "testdata/gemini-cli/tmp"} }},
		{"qwen-code", func(*testing.T) trace.Reader { return QwenCode{Dir: "testdata/qwen-code/projects"} }},
		{"vscode-copilot", func(*testing.T) trace.Reader { return VSCodeCopilot{User: "testdata/vscode-copilot/User"} }},
		{"copilot-cli", func(*testing.T) trace.Reader {
			return CopilotCLI{Dir: "testdata/copilot-cli/session-state", Configs: []string{"testdata/copilot-cli/mcp-config.json"}}
		}},
		{"antigravity", func(t *testing.T) trace.Reader { return Antigravity{Dir: installAntigravity(t)} }},
		{"windsurf", func(*testing.T) trace.Reader {
			return Windsurf{Dirs: []string{"testdata/windsurf/windsurf-transcripts", "testdata/windsurf/tap-archive"}}
		}},
		{"continue", func(*testing.T) trace.Reader {
			return Continue{Dir: "testdata/continue/sessions", Configs: []string{"testdata/continue/config.yaml"}}
		}},
		{"cline-cli", func(*testing.T) trace.Reader {
			return ClineCLI{Dir: "testdata/cline-cli/sessions", Configs: []string{"testdata/cline-cli/cline_mcp_settings.json"}}
		}},
		{"cline-tasks", func(*testing.T) trace.Reader { return extTasks("cline", "saoudrizwan.claude-dev") }},
		{"cursor-cli", func(t *testing.T) trace.Reader {
			dir, bin := buildCursorCLIStores(t)
			return CursorCLI{Dir: dir, SQLite3: bin}
		}},
		{"opencode", func(t *testing.T) trace.Reader {
			db := filepath.Join(t.TempDir(), "opencode.db")
			buildDB(t, "testdata/opencode/opencode.sql", db)
			return OpenCodeDB{ID: "opencode", DB: db, Configs: []string{"testdata/opencode/opencode.json"}}
		}},
		{"kilo", func(t *testing.T) trace.Reader {
			db := filepath.Join(t.TempDir(), "kilo.db")
			buildDB(t, "testdata/kilo/kilo.sql", db)
			return OpenCodeDB{ID: "kilo", DB: db, Configs: []string{"testdata/kilo/kilo.json"}}
		}},
		{"goose", func(t *testing.T) trace.Reader {
			dir := t.TempDir()
			buildDB(t, "testdata/goose/sessions.sql", filepath.Join(dir, "sessions.db"))
			return Goose{Dir: dir}
		}},
		{"crush", func(t *testing.T) trace.Reader { return crushFixture(t) }},
		{"zed", func(t *testing.T) trace.Reader {
			if _, err := exec.LookPath("zstd"); err != nil {
				t.Skip("zstd is not installed")
			}
			dir := t.TempDir()
			buildDB(t, "testdata/zed/threads/threads.sql", filepath.Join(dir, "threads.db"))
			return Zed{Dir: dir}
		}},
		{"aider", func(t *testing.T) trace.Reader {
			home := t.TempDir()
			proj := filepath.Join(home, "code", "work")
			os.MkdirAll(proj, 0o755)
			b, _ := os.ReadFile("testdata/aider/work/.aider.chat.history.md")
			os.WriteFile(filepath.Join(proj, ".aider.chat.history.md"), b, 0o644)
			return Aider{Home: home}
		}},
		{"cursor", func(t *testing.T) trace.Reader {
			db := filepath.Join(t.TempDir(), "state.vscdb")
			buildCursorStore(t, db, [][]string{{"ls", "pwd"}, {"date"}, {"whoami", "id"}})
			return Cursor{DB: db}
		}},
	}
}

func crushFixture(t *testing.T) Crush {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	buildDB(t, "testdata/crush/work/.crush/crush.sql", filepath.Join(work, ".crush", "crush.db"))
	b, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"path": work, "data_dir": filepath.Join(work, ".crush")}}})
	os.WriteFile(filepath.Join(dir, "projects.json"), b, 0o644)
	return Crush{Dir: dir, Configs: []string{"testdata/crush/crush.json"}}
}

// Every reader returns exactly what a fresh read returns, source digests
// included, when it fills its cache and when it reads from it; and a repeat
// read of unchanged history reads nothing from the source.
func TestEveryReaderReadsTheSameFromItsCache(t *testing.T) {
	for _, tc := range cacheCases() {
		t.Run(tc.cache, func(t *testing.T) {
			r := tc.build(t)
			fresh, err := r.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(fresh) == 0 {
				t.Fatal("the fixture has no sessions")
			}
			useTempCache(t)
			reads := readsOf(t)
			first, err := r.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(reads(tc.cache)) == 0 {
				t.Fatalf("nothing was read through cache %q", tc.cache)
			}
			second, err := r.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			got := reads(tc.cache)
			// Cursor always reads the conversation holding the newest row
			// again (TestCursorRereadsTheConversationHoldingTheNewestRow).
			if tc.cache == "cursor" && len(got) == 1 && got[0] == cursorID(3) {
				got = nil
			}
			if len(got) != 0 {
				t.Errorf("unchanged history was read again: %v", got)
			}
			if !reflect.DeepEqual(first, fresh) {
				t.Errorf("filling the cache changed the result:\ncached %+v\nfresh  %+v", first, fresh)
			}
			if !reflect.DeepEqual(second, fresh) {
				t.Errorf("the cached result differs from a fresh read:\ncached %+v\nfresh  %+v", second, fresh)
			}
		})
	}
}

// A changed Cursor record is found from the key index alone: the changed
// conversation is read again, an untouched one is not, and the result is a
// fresh read's.
func TestCursorRereadsOnlyTheConversationThatChanged(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.vscdb")
	buildCursorStore(t, db, [][]string{{"ls", "pwd"}, {"date"}, {"whoami"}, {"id"}, {"uname"}, {"hostname"}})
	r := Cursor{DB: db}
	useTempCache(t)
	reads := readsOf(t)
	if _, err := r.Read(time.Time{}); err != nil {
		t.Fatal(err)
	}
	reads("cursor")
	// Rewrite conversation 2's bubble the way Cursor does: the row is
	// replaced and takes a new row ID.
	sqlite(t, db, cursorBubbleSQL(cursorID(2), 0, "date -u"))
	got, err := r.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	read := reads("cursor")
	if !contains(read, cursorID(2)) || contains(read, cursorID(1)) {
		t.Errorf("read %v; want conversation 2 and not conversation 1", read)
	}
	UseCache("")
	fresh, _ := r.Read(time.Time{})
	if !reflect.DeepEqual(got, fresh) {
		t.Errorf("result after the change differs from a fresh read")
	}
	if c := got[1].Calls[0].Command; c != "date -u" {
		t.Errorf("conversation 2 command %q, want the rewritten one", c)
	}
}

// Rewriting the store's newest row can give the new row the ID the old one
// had; the conversation holding the highest row ID the last run saw is
// therefore always read again.
func TestCursorRereadsTheConversationHoldingTheNewestRow(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.vscdb")
	buildCursorStore(t, db, [][]string{{"ls"}, {"date"}})
	useTempCache(t)
	reads := readsOf(t)
	Cursor{DB: db}.Read(time.Time{})
	reads("cursor")
	Cursor{DB: db}.Read(time.Time{})
	if read := reads("cursor"); len(read) != 1 || read[0] != cursorID(2) {
		t.Errorf("read %v; want only the conversation holding the newest row", read)
	}
}

// Every Cursor query is restricted by the listed conversations; a query the
// rewrite missed would read the whole store on every run.
func TestCursorScopedQueriesReadOnlyTheListedConversations(t *testing.T) {
	for _, sql := range []string{CursorComposerSQL, CursorBubbleSQL, CursorInlineSQL, CursorUserSQL, CursorInlineUserSQL} {
		scoped := cursorScoped(sql, []string{cursorID(1)})
		if !strings.Contains(scoped, "ids.id") || strings.Contains(scoped, "c.key >= 'bubbleId:' AND") || strings.Contains(scoped, "c.key >= 'composerData:' AND") {
			t.Errorf("not restricted to the listed conversations:\n%s", scoped)
		}
	}
}

// A changed or added session of a database store is read; the others come
// from the cache.
func TestDatabaseStoresRereadOnlyChangedSessions(t *testing.T) {
	t.Run("opencode", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "opencode.db")
		buildDB(t, "testdata/opencode/opencode.sql", db)
		r := OpenCodeDB{ID: "opencode", DB: db, Configs: []string{"testdata/opencode/opencode.json"}}
		ids := strings.Split(sqlite(t, db, "SELECT id FROM session ORDER BY id"), "\n")
		if len(ids) < 2 {
			t.Fatalf("fixture sessions %v", ids)
		}
		checkOnlyChanged(t, r, "opencode", ids[0], func() {
			// A running tool's part is updated in place: same row, new time.
			sqlite(t, db, "UPDATE part SET time_updated = time_updated + 1 WHERE rowid = (SELECT max(rowid) FROM part WHERE session_id = '"+ids[0]+"')")
		})
	})
	t.Run("goose", func(t *testing.T) {
		dir := t.TempDir()
		db := filepath.Join(dir, "sessions.db")
		buildDB(t, "testdata/goose/sessions.sql", db)
		checkOnlyChanged(t, Goose{Dir: dir}, "goose", "s2", func() {
			sqlite(t, db, "INSERT INTO sessions(id, working_dir) VALUES ('s2', '/w'); INSERT INTO messages(message_id, session_id, role, content_json, created_timestamp) SELECT message_id || '-2', 's2', role, content_json, created_timestamp FROM messages")
		})
	})
	t.Run("crush", func(t *testing.T) {
		r := crushFixture(t)
		db := filepath.Join(r.Dir, "work", ".crush", "crush.db")
		checkOnlyChanged(t, r, "crush", "s2", func() {
			sqlite(t, db, "INSERT INTO sessions(id, title, updated_at, created_at) VALUES ('s2', 't', 1, 1); INSERT INTO messages(id, session_id, role, parts, created_at, updated_at) SELECT id || '-2', 's2', role, parts, created_at, updated_at FROM messages WHERE session_id != 's2'")
		})
	})
	t.Run("zed", func(t *testing.T) {
		if _, err := exec.LookPath("zstd"); err != nil {
			t.Skip("zstd is not installed")
		}
		dir := t.TempDir()
		db := filepath.Join(dir, "threads.db")
		buildDB(t, "testdata/zed/threads/threads.sql", db)
		checkOnlyChanged(t, Zed{Dir: dir}, "zed", "thread-1", func() {
			sqlite(t, db, "UPDATE threads SET updated_at = '2026-10-01T00:00:00Z' WHERE id = 'thread-1'")
		})
	})
}

// checkOnlyChanged fills r's cache, applies change, and requires the next
// read to read exactly the unit want (key scope\x00unit) and to equal a
// fresh read.
func checkOnlyChanged(t *testing.T, r trace.Reader, cache, want string, change func()) {
	t.Helper()
	useTempCache(t)
	reads := readsOf(t)
	if _, err := r.Read(time.Time{}); err != nil {
		t.Fatal(err)
	}
	reads(cache)
	change()
	got, err := r.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if read := reads(cache); len(read) != 1 || read[0] != want {
		t.Errorf("read %v; want only %s", read, want)
	}
	UseCache("")
	fresh, _ := r.Read(time.Time{})
	if !reflect.DeepEqual(got, fresh) {
		t.Errorf("result after the change differs from a fresh read")
	}
}

// A cache that disagrees with the source (a fingerprint that missed a
// change) is caught by the spot-check: the reader reads everything again,
// returns what a fresh read returns, and says so.
func TestSpotCheckRebuildsACacheThatDisagrees(t *testing.T) {
	for _, tc := range []cacheCase{cacheCases()[0], cacheCases()[len(cacheCases())-1]} {
		t.Run(tc.cache, func(t *testing.T) {
			r := tc.build(t)
			fresh, _ := r.Read(time.Time{})
			useTempCache(t)
			TakeNotices()
			r.Read(time.Time{})
			c := openUnitCache(tc.cache)
			for k, u := range c.units {
				for i := range u.Sessions {
					u.Sessions[i].Requests = append(u.Sessions[i].Requests, "not in the source")
				}
				c.units[k] = u
			}
			c.dirty = true
			c.save()
			spotChecks = 1000
			got, err := r.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, fresh) {
				t.Errorf("served the disagreeing cache")
			}
			if n := TakeNotices(); len(n) != 1 || !strings.Contains(n[0], tc.cache) {
				t.Errorf("notices %q", n)
			}
		})
	}
}

// A file the agent is still writing is read but not cached.
func TestARecentlyWrittenFileIsNotCached(t *testing.T) {
	useTempCache(t)
	activeWindow = time.Hour
	reads := readsOf(t)
	file := copyFixture(t, fixtureParsers[0].file, t.TempDir())
	for i := 0; i < 2; i++ {
		ParseFiles([]string{file}, "claude-code", nil, parseClaude)
	}
	if n := len(reads("claude-code")); n != 2 {
		t.Errorf("read %d times; a file written just now is read every time", n)
	}
}

// The remembered search finds what a plain walk finds, as folders and
// histories come and go, while reading only the folders that changed.
func TestAiderCachedSearchMatchesAPlainWalk(t *testing.T) {
	home := t.TempDir()
	put := func(parts ...string) {
		d := filepath.Join(append([]string{home}, parts...)...)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, ".aider.chat.history.md"), []byte("x"), 0o644)
	}
	put("Desktop", "Projects", "a")
	put(".hidden", "b")
	put("a", "b", "c", "d", "e", "f", "g")
	useTempCache(t)
	c := openUnitCache("aider-walk")
	check := func(step string) {
		t.Helper()
		if got, want := aiderHistoriesCached(home, 6, c), aiderHistories(home, 6); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: cached search %v, plain walk %v", step, got, want)
		}
	}
	check("first search")
	put("Desktop", "Projects", "new")
	check("after a new project")
	os.RemoveAll(filepath.Join(home, "Desktop", "Projects", "a"))
	check("after a project was removed")
	put("node_modules", "x")
	check("after a skipped folder appeared")
	os.Remove(filepath.Join(home, "Desktop", "Projects", "new", ".aider.chat.history.md"))
	check("after a history was removed")
	if len(c.dirs) == 0 {
		t.Error("the search remembered no folders")
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
