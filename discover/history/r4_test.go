package history

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/redact"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// R4 readers, on real sessions of each agent (testdata/SYNTHETIC.md
// "Real-run fixtures"): the scripted task ran a shell command, an MCP search
// that returns ids, an MCP call that fails, a call on the returned key, and
// in a second turn one more call.

// buildDB rebuilds a captured store from its SQL.
func buildDB(t *testing.T, sqlFile, db string) {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	os.MkdirAll(filepath.Dir(db), 0o755)
	sql, err := os.ReadFile(sqlFile)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, db)
	cmd.Stdin = strings.NewReader(string(sql))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v %s", sqlFile, err, out)
	}
}

// The scripted task as each agent recorded it: the calls of the main turn
// and of the follow-up.
func scripted(failedIsMarked bool) []wantCall {
	failed := trace.OutcomeFailed
	if !failedIsMarked {
		failed = trace.OutcomeOK // the agent records no failure for it
	}
	return []wantCall{
		{"shell", "", "", 0, trace.OutcomeOK},
		{"mcp:search_issues", "tracker", "search_issues", 0, trace.OutcomeOK},
		{"mcp:get_issue", "tracker", "get_issue", 0, failed},
		{"mcp:get_issue", "tracker", "get_issue", 0, trace.OutcomeOK},
		{"mcp:get_issue", "tracker", "get_issue", 1, trace.OutcomeOK},
	}
}

func onlyWithCalls(ss []trace.Session, n int) *trace.Session {
	for i := range ss {
		if len(ss[i].Calls) == n {
			return &ss[i]
		}
	}
	return nil
}

func TestOpenCodeReader(t *testing.T) {
	db := filepath.Join(t.TempDir(), "opencode.db")
	buildDB(t, "testdata/opencode/opencode.sql", db)
	ss, err := OpenCodeDB{ID: "opencode", DB: db, Configs: []string{"testdata/opencode/opencode.json"}}.Read(time.Time{})
	if err != nil || len(ss) != 2 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	s := onlyWithCalls(ss, 5)
	if s == nil {
		t.Fatalf("no session with the scripted calls")
	}
	if len(s.Requests) != 2 || !strings.HasPrefix(s.Requests[0], "Do these steps") {
		t.Fatalf("requests %q", s.Requests)
	}
	checkCalls(t, *s, scripted(true))
	if s.Calls[0].Command != "wc -l notes.txt" || !s.Calls[1].Measured || len(s.Calls[1].OutIDs) == 0 {
		t.Errorf("command %q measured %v ids %v", s.Calls[0].Command, s.Calls[1].Measured, s.Calls[1].OutIDs)
	}
	// Without the configuration naming the server, the tool keeps its name.
	ss, _ = OpenCodeDB{ID: "opencode", DB: db}.Read(time.Time{})
	if c := onlyWithCalls(ss, 5).Calls[1]; c.Tool != "tracker_search_issues" || c.MCPServer != "" {
		t.Errorf("unconfigured split: %s %s", c.Tool, c.MCPServer)
	}
}

func TestKiloCLIReader(t *testing.T) {
	db := filepath.Join(t.TempDir(), "kilo.db")
	buildDB(t, "testdata/kilo/kilo.sql", db)
	ss, err := OpenCodeDB{ID: "kilo", DB: db, Configs: []string{"testdata/kilo/kilo.json"}}.Read(time.Time{})
	if err != nil || len(ss) != 1 || ss[0].Client != "kilo" {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	checkCalls(t, ss[0], scripted(true))
}

func TestGooseReader(t *testing.T) {
	dir := t.TempDir()
	buildDB(t, "testdata/goose/sessions.sql", filepath.Join(dir, "sessions.db"))
	ss, err := Goose{Dir: dir}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	s := ss[0]
	// Goose injects a <turn-context> user message each turn: not a request.
	if len(s.Requests) != 2 || strings.HasPrefix(s.Requests[1], "<") {
		t.Fatalf("requests %q", s.Requests)
	}
	checkCalls(t, s, scripted(true))
}

func TestCrushReader(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	buildDB(t, "testdata/crush/work/.crush/crush.sql", filepath.Join(work, ".crush", "crush.db"))
	b, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"path": work, "data_dir": filepath.Join(work, ".crush")}}})
	os.WriteFile(filepath.Join(dir, "projects.json"), b, 0o644)
	ss, err := Crush{Dir: dir, Configs: []string{"testdata/crush/crush.json"}}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	// Crush recorded the failed MCP call with is_error false.
	checkCalls(t, ss[0], scripted(false))
}

func TestContinueReader(t *testing.T) {
	ss, err := Continue{Dir: "testdata/continue/sessions", Configs: []string{"testdata/continue/config.yaml"}}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	// Continue keeps no error flag; one MCP server configured names the server.
	checkCalls(t, ss[0], scripted(false))
	if !ss[0].Calls[1].Measured {
		t.Error("usage not read")
	}
	// The result is the tool's own text, not Continue's context-item wrapper.
	if out := ss[0].Calls[2].Output; out != "Issue ABC-99 does not exist" {
		t.Errorf("result %q", out)
	}
	ss, _ = Continue{Dir: "testdata/continue/sessions"}.Read(time.Time{})
	if c := ss[0].Calls[1]; c.Tool != "search_issues" || c.MCPServer != "" {
		t.Errorf("no configuration, no server: %s %s", c.Tool, c.MCPServer)
	}
}

// No fixture carries a home path, the Fireworks key used for the real runs,
// or another credential (each fixture line is checked as written; strings
// were redacted at capture).
func TestAllFixturesAreRedacted(t *testing.T) {
	key := regexp.MustCompile(`fw_[A-Za-z0-9]{16,}`)
	// A fixture captured on this machine would carry its home directory.
	home, _ := os.UserHomeDir()
	n := 0
	err := filepath.WalkDir("testdata", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(b)
		if strings.Contains(s, "/Users/") || key.MatchString(s) || (len(home) > 1 && strings.Contains(s, home)) {
			t.Errorf("%s holds a home path or a key", p)
		}
		for _, line := range strings.Split(s, "\n") {
			if sh := redact.SecretShape(strings.ReplaceAll(line, `\n`, " ")); sh == "private key" || sh == "JWT" || sh == "bearer token" || sh == "model provider key" {
				t.Errorf("%s: %s", p, sh)
			}
		}
		return nil
	})
	if err != nil || n < 20 {
		t.Fatalf("%d fixture files, %v", n, err)
	}
}

// An agent's store in WAL mode, still open in the agent, has its
// schema and rows only in the -wal file. The reader sees them; immutable=1,
// which reads the main file alone, did not.
func TestOpenCodeReaderSeesWhatIsOnlyInTheWAL(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	sql, err := os.ReadFile("testdata/opencode/opencode.sql")
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "opencode.db")
	// The writer stays open, as the agent does, so nothing is checkpointed.
	w := exec.Command(bin, db)
	in, _ := w.StdinPipe()
	out, _ := w.StdoutPipe()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); w.Wait() }()
	io.WriteString(in, "PRAGMA journal_mode=WAL;\nPRAGMA wal_autocheckpoint=0;\n"+string(sql)+"\n.print ready\n")
	sc := bufio.NewScanner(out)
	for sc.Scan() && sc.Text() != "ready" {
	}
	if fi, err := os.Stat(db + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the rows are not in the WAL: %v", err)
	}
	if b, _ := exec.Command(bin, "-readonly", "file:"+db+"?immutable=1", "SELECT count(*) FROM session").CombinedOutput(); !strings.Contains(string(b), "no such table") {
		t.Fatalf("the main file already holds the schema, so this test shows nothing: %s", b)
	}
	ss, err := OpenCodeDB{ID: "opencode", DB: db, Configs: []string{"testdata/opencode/opencode.json"}}.Read(time.Time{})
	if err != nil || onlyWithCalls(ss, 5) == nil {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
}

// A store without the tables a reader queries (one the agent has not set
// up yet) is counted unreadable; the run goes on.
func TestAStoreWithoutItsTablesIsUnreadableNotFatal(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	db := filepath.Join(t.TempDir(), "kilo.db")
	if out, err := exec.Command(bin, db, "CREATE TABLE other(x);").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	ss, st, err := OpenCodeDB{ID: "kilo", DB: db}.ReadWithStats(time.Time{})
	if err != nil || len(ss) != 0 || st.UnreadableFiles != 1 {
		t.Fatalf("%d sessions, %+v, %v", len(ss), st, err)
	}
	dir := t.TempDir()
	exec.Command(bin, filepath.Join(dir, "sessions.db"), "CREATE TABLE other(x);").Run()
	if _, st, err := (Goose{Dir: dir}).ReadWithStats(time.Time{}); err != nil || st.UnreadableFiles != 1 {
		t.Fatalf("goose: %+v %v", st, err)
	}
}
