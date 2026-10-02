package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover"
	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// installCursorCLIFixtures rebuilds the Cursor CLI fixture stores under
// home/.cursor/chats.
func installCursorCLIFixtures(t *testing.T, home string) string {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	sqls, _ := filepath.Glob("../history/testdata/cursor-cli/*/*/store.sql")
	for _, f := range sqls {
		rel, _ := filepath.Rel("../history/testdata/cursor-cli", filepath.Dir(f))
		db := filepath.Join(home, ".cursor", "chats", rel, "store.db")
		os.MkdirAll(filepath.Dir(db), 0o755)
		sql, _ := os.ReadFile(f)
		cmd := exec.Command(bin, db)
		cmd.Stdin = bytes.NewReader(sql)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	return bin
}

func execOf(s trace.Session, idx ...int) primitive.Execution {
	ex := primitive.Execution{ID: s.Client + "-" + s.ID[:8], Client: s.Client, Session: s.ID}
	for i, n := range idx {
		c := s.Calls[n]
		ex.Calls = append(ex.Calls, primitive.CallRef{Step: i + 1, Index: n, ID: c.ID, Op: c.Tool, OK: true, Time: c.Time.Format(time.RFC3339)})
	}
	return ex
}

func evidence(t *testing.T, dir, id string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"evidence", dir, id}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("evidence %s: exit %d: %s", id, code, errOut.String())
	}
	return out.String()
}

// End to end for TENG-3122: a handoff cites the exact source of every call
// and result read from Antigravity (transcript lines placed by step index)
// and from the Cursor CLI (records of its content-addressed store), and
// `tap discover evidence` verifies them and reports a changed source.
func TestHandoffCitesAntigravityAndCursorCLISources(t *testing.T) {
	home := t.TempDir()
	bin := installCursorCLIFixtures(t, home)
	copyTree(t, "../history/testdata/antigravity/brain", filepath.Join(home, ".gemini", "antigravity", "brain"))
	ag, err := history.Antigravity{Dir: filepath.Join(home, ".gemini", "antigravity", "brain")}.Read(time.Time{})
	if err != nil || len(ag) != 1 {
		t.Fatal(err)
	}
	cc, err := history.CursorCLI{Dir: filepath.Join(home, ".cursor", "chats")}.Read(time.Time{})
	if err != nil || len(cc) != 2 {
		t.Fatal(err)
	}
	// Antigravity calls 3 and 4 (two shell calls with results); Cursor CLI
	// calls 5 and 6 (two Jira lookups through CallMcpTool).
	p := primitive.Primitive{ID: "p1", Steps: []string{"a", "b"}, Executions: []primitive.Execution{execOf(ag[0], 3, 4), execOf(cc[0], 5, 6)}}
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := primitive.WriteHandoff(dir, home, p, append(ag, cc...), primitive.Skill{Source: "s", Content: []byte("#")}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "EVIDENCE-INDEX.json"))
	var idx primitive.EvidenceIndex
	json.Unmarshal(b, &idx)
	if idx.Available != 2 {
		t.Fatalf("%d of 2 executions found their source: %s", idx.Available, b)
	}
	for _, ex := range idx.Executions {
		for _, c := range ex.Calls {
			if c.Call == nil || c.Result == nil {
				t.Fatalf("%s call %s not placed: %+v %+v", ex.Client, c.CallID, c.Call, c.Result)
			}
			if ex.Client == "cursor-cli" && (c.Call.Record == "" || c.Result.Record == "") {
				t.Fatalf("cursor-cli locator is not a record: %+v", c)
			}
			if ex.Client == "antigravity" && (c.Call.Line == 0 || c.Result.Line != c.Call.Line+1) {
				t.Fatalf("antigravity result is not the next step: %+v", c)
			}
		}
	}
	agID, ccID := idx.Executions[0].ID, idx.Executions[1].ID
	for _, id := range []string{agID, ccID} {
		if out := evidence(t, dir, id); strings.Contains(out, "STALE") || strings.Count(out, "(verified)") != 4 {
			t.Fatalf("%s:\n%s", id, out)
		}
	}
	excerpt, _ := os.ReadFile(filepath.Join(dir, "evidence", "invocation-002.md"))
	if !strings.Contains(string(excerpt), "record ") || !strings.Contains(string(excerpt), "Result:") {
		t.Fatalf("cursor-cli excerpt:\n%s", excerpt)
	}

	// A step appended to the conversation leaves the citations valid; a
	// rewritten step and a changed store record are reported, never trusted.
	tr := idx.Executions[0].Transcript
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"step_index":500,"type":"USER_INPUT","content":"more"}` + "\n")
	f.Close()
	if out := evidence(t, dir, agID); strings.Contains(out, "STALE") {
		t.Fatalf("append broke the citations:\n%s", out)
	}
	lines := strings.Split(string(must(os.ReadFile(tr))), "\n")
	lines[idx.Executions[0].Calls[0].Call.Line-1] = `{"step_index":25,"type":"PLANNER_RESPONSE"}`
	os.WriteFile(tr, []byte(strings.Join(lines, "\n")), 0o644)
	if out := evidence(t, dir, agID); !strings.Contains(out, "STALE") {
		t.Fatalf("a rewritten step was trusted:\n%s", out)
	}
	rec := idx.Executions[1].Calls[0].Call.Record
	if out, err := exec.Command(bin, idx.Executions[1].Transcript, `UPDATE blobs SET data = '{"role":"assistant"}' WHERE id = '`+rec+`'`).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out := evidence(t, dir, ccID); !strings.Contains(out, "STALE (record") {
		t.Fatalf("a changed record was trusted:\n%s", out)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
