package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
)

// copyTree copies the reader fixtures src into dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func reportClients(t *testing.T, args ...string) []string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := discover.Command(append([]string{"report", "--json"}, args...), strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("discover %v exited %d: %s", args, code, errOut.String())
	}
	var rep model.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, out.String())
	}
	var got []string
	for _, c := range rep.Clients {
		if c.Sessions == 0 {
			t.Errorf("%s was read but gave no sessions", c.Client)
		}
		got = append(got, c.Client)
	}
	sort.Strings(got)
	return got
}

// End to end through `tap discover report`: with no --client, discover reads
// every agent installed under HOME (D1, TENG-3108), and an agent that is not
// installed is not read.
func TestDiscoverReadsEveryDetectedAgentByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	copyTree(t, "../history/testdata/claude", filepath.Join(home, ".claude", "projects"))
	if got := reportClients(t); strings.Join(got, ",") != "claude-code" {
		t.Fatalf("only Claude Code installed: read %v", got)
	}
	copyTree(t, "../history/testdata/codex", filepath.Join(home, ".codex", "sessions"))
	if got := reportClients(t); strings.Join(got, ",") != "claude-code,codex" {
		t.Fatalf("Claude Code and Codex installed: read %v", got)
	}
	// An explicit list, by alias, still narrows the read.
	if got := reportClients(t, "--client", "claude"); strings.Join(got, ",") != "claude-code" {
		t.Fatalf("--client claude: read %v", got)
	}
	// A known agent without a reader is refused by name, not as unknown.
	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"report", "--client", "windsurf"}, strings.NewReader(""), &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "does not support reading session history") {
		t.Fatalf("windsurf: exit %d: %s", code, errOut.String())
	}
}

// End to end through the shared assembler (TENG-3111): the whole report,
// from every reader through discovery, is the same bytes on every run.
func TestDiscoverReportIsByteIdenticalAcrossRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	copyTree(t, "../history/testdata/claude", filepath.Join(home, ".claude", "projects"))
	copyTree(t, "../history/testdata/codex", filepath.Join(home, ".codex", "sessions"))
	run := func() string {
		var out, errOut bytes.Buffer
		if code := discover.Command([]string{"report", "--json", "--client", "all"}, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		// Everything but the time the report was made.
		var rep map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		delete(rep, "generated_at")
		b, _ := json.Marshal(rep)
		return string(b)
	}
	first := run()
	for i := 0; i < 3; i++ {
		if run() != first {
			t.Fatalf("run %d differs", i+2)
		}
	}
}

// End to end for R1 (TENG-3112): with the Cursor CLI's and Antigravity's
// stores in their usual places under HOME, `tap discover` detects both and
// reads them with no flags, in the report and in the primitive menu path.
func TestDiscoverReadsCursorCLIAndAntigravity(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
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
	copyTree(t, "../history/testdata/antigravity", filepath.Join(home, ".gemini", "antigravity", "brain"))
	if got := reportClients(t); strings.Join(got, ",") != "cursor-cli,antigravity" && strings.Join(got, ",") != "antigravity,cursor-cli" {
		t.Fatalf("read %v", got)
	}
	// The default command (the primitive menu, as JSON) reads them too.
	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"--json"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var res struct {
		Summary map[string]any `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// Two Cursor CLI sessions (19 and 12 calls) and one Antigravity
	// conversation (14 calls).
	if res.Summary["sessions"] != float64(3) || res.Summary["toolCalls"] != float64(45) {
		t.Fatalf("summary %v", res.Summary)
	}
}
