package history

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// R6 readers.

func TestWindsurfReaderPrefersTheFullestCopy(t *testing.T) {
	ss, err := Windsurf{Dirs: []string{"testdata/windsurf/windsurf-transcripts", "testdata/windsurf/tap-archive"}}.Read(time.Time{})
	if err != nil || len(ss) != 2 {
		t.Fatalf("%d sessions (one pruned by Windsurf, kept by TAP), %v", len(ss), err)
	}
	var full trace.Session
	for _, s := range ss {
		if s.ID == "traj-1" {
			full = s
		}
	}
	// The transcript records no results: calls have no outcome.
	checkCalls(t, full, []wantCall{
		{"mcp:search_issues", "tracker", "search_issues", 0, trace.OutcomeUnknown},
		{"shell", "", "", 0, trace.OutcomeUnknown},
		{"mcp:get_issue", "tracker", "get_issue", 1, trace.OutcomeUnknown},
	})
}

// A stand-in for the amp CLI: list and export answer from the fixtures.
func fakeAmp(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in amp is a sh program")
	}
	dir := t.TempDir()
	abs, _ := filepath.Abs("testdata/amp")
	script := "#!/bin/sh\ncase \"$2\" in\n  list) cat '" + abs + "/threads-list.json' ;;\n  export) cat '" + abs + "/'\"$3\"'.export.json' ;;\nesac\n"
	p := filepath.Join(dir, "amp")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAmpReaderExportsThroughTheCLI(t *testing.T) {
	ss, st, err := Amp{Bin: fakeAmp(t), Dir: filepath.Join(t.TempDir(), "none")}.ReadWithStats(time.Time{})
	// The listing's second id is not a thread id and is never passed to the
	// CLI.
	if err != nil || len(ss) != 1 || st.UnreadableFiles != 0 {
		t.Fatalf("%d sessions %+v %v", len(ss), st, err)
	}
	checkCalls(t, ss[0], []wantCall{
		{"mcp:search_issues", "tracker", "search_issues", 0, trace.OutcomeOK},
		{"shell", "", "", 0, trace.OutcomeOK},
		{"mcp:get_issue", "tracker", "get_issue", 0, trace.OutcomeFailed},
	})
	// No amp program, no legacy folder: nothing, and no error.
	if ss, err := (Amp{Bin: "", Dir: t.TempDir()}).Read(time.Time{}); err != nil || (len(ss) != 0 && !ampOnPath()) {
		t.Fatalf("%d %v", len(ss), err)
	}
}

func ampOnPath() bool { _, err := os.Stat("/usr/local/bin/amp"); return err == nil }

func TestAiderReader(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "code", "work")
	os.MkdirAll(proj, 0o755)
	b, _ := os.ReadFile("testdata/aider/work/.aider.chat.history.md")
	os.WriteFile(filepath.Join(proj, ".aider.chat.history.md"), b, 0o644)
	// Only the project above is read: folders past six levels, hidden
	// folders and dependency trees never hold a project's history.
	for _, dir := range [][]string{{"a", "b", "c", "d", "e", "f", "g"}, {".hidden", "x"}, {"code", "node_modules", "pkg"}, {"Library", "x"}} {
		d := filepath.Join(append([]string{home}, dir...)...)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, ".aider.chat.history.md"), b, 0o644)
	}
	ss, err := Aider{Home: home}.Read(time.Time{})
	if err != nil || len(ss) != 2 {
		t.Fatalf("%d chats (two in the real history), %v", len(ss), err)
	}
	if c := ss[0].Calls[0]; c.Tool != "shell" || c.Command != "wc -l notes.txt" {
		t.Errorf("/run call %+v", c)
	}
	if c := ss[1].Calls[0]; c.Tool != "edit" || c.Args["file_path"] != "notes.txt" || c.Outcome != trace.OutcomeOK {
		t.Errorf("edit call %+v", c)
	}
	if len(ss[1].Requests) != 1 || ss[1].Requests[0] != "Append a second line with the word world to notes.txt" {
		t.Errorf("requests %q", ss[1].Requests)
	}
}

// Aider writes its history into the project folder, and projects
// often sit 4 or more levels below home (~/Desktop/Projects/<org>/<repo>).
// Histories 4 and 6 levels down are read.
func TestAiderReadsProjectsDeepUnderHome(t *testing.T) {
	home := t.TempDir()
	b, _ := os.ReadFile("testdata/aider/work/.aider.chat.history.md")
	for _, dir := range [][]string{{"Desktop", "Projects", "Telara", "repo"}, {"a", "b", "c", "d", "e", "f"}} {
		d := filepath.Join(append([]string{home}, dir...)...)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, ".aider.chat.history.md"), b, 0o644)
	}
	ss, err := Aider{Home: home}.Read(time.Time{})
	if err != nil || len(ss) != 4 {
		t.Fatalf("%d chats (two per history), %v", len(ss), err)
	}
}
