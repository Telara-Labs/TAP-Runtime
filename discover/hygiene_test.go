package discover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/routine"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
)

func TestAcknowledgementsAndRetriesContinueTheRequest(t *testing.T) {
	var s trace.Session
	s.AddRequest("please move TENG-3054 to done and comment that it shipped")
	for _, ack := range []string{"yes", "Yes, file", "ok, yes", "already approved.", "continue", "please move TENG-3054 to done and comment that it shipped"} {
		s.AddRequest(ack)
	}
	if len(s.Requests) != 1 {
		t.Fatalf("acknowledgements and a re-sent request must continue it: %q", s.Requests)
	}
	for _, next := range []string{"please deploy", "yes, and also check TENG-3055", "fix the gateway/src/main.go panic"} {
		var s2 trace.Session
		s2.AddRequest("first task")
		s2.AddRequest(next)
		if len(s2.Requests) != 2 {
			t.Errorf("%q is a new request", next)
		}
	}
}

func TestCodexContextBlocksYieldTheRealRequest(t *testing.T) {
	var s trace.Session
	s.AddRequest("# In app browser:\n- The user has the in-app browser open.\n- Current URL: https://www.linkedin.com/feed/\n\n## My request for Codex:\nfind the three newest comments\n")
	if len(s.Requests) != 1 || s.Requests[0] != "find the three newest comments" {
		t.Fatalf("requests = %q", s.Requests)
	}
}

func TestNonCommandsAreNotSteps(t *testing.T) {
	for in, want := range map[string]string{
		"# Check the repos\ngit status":                                               "git",
		"for repo in\n  Telara\n  telara-middleware\ndo\n  git -C $repo status\ndone": "git",
		"$repo status": "",
		"*.csv tail":   "",
		"linkedin-prospect-handoff-2026-06-30.md": "",
		"case $x in a) ls ;; esac\nmake":          "make",
	} {
		var got []string
		for _, c := range shellparse.SimpleCommands(in) {
			got = append(got, c[0].Text)
		}
		if strings.Join(got, " ") != want {
			t.Errorf("simpleCommands(%q) programs = %q, want %q", in, got, want)
		}
	}
}

func TestTypingOfQuotedURLsAndNumericFlags(t *testing.T) {
	if got := trace.TypeOf(shellparse.Word{Text: "see https://x.dev for why", Quoted: true}); got != trace.SlotText {
		t.Errorf("a quoted sentence with a URL is text, got %s", got)
	}
	if got := trace.TypeOf(shellparse.Word{Text: "-15"}); got != trace.SlotNumber {
		t.Errorf("tail -15 is a count, got %s", got)
	}
}

func TestReportTextIsValidUTF8(t *testing.T) {
	ss := testkit.RequestSessions(6, func(i int) string { return strings.Repeat("→ déploiement ", 20) }, func(i int) []trace.Call {
		return []trace.Call{testkit.ShellCall("git status --short"), testkit.ShellCall("git diff --stat"), testkit.ShellCall("git log --oneline -3"), testkit.ShellCall("git branch --show-current")}
	})
	o := DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: ss}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	routine.WriteFunnel(&buf, rep, 0, true)
	if !utf8.Valid(buf.Bytes()) {
		t.Fatal("the report cut a character in half")
	}
}

func TestKindsAndMerging(t *testing.T) {
	book := testkit.RequestSessions(6, func(i int) string { return fmt.Sprintf("work on ticket %d", i) }, func(i int) []trace.Call {
		return []trace.Call{{Tool: "mcp:telara_task_list"}, {Tool: "mcp:telara_task_create", Args: map[string]string{"goal": fmt.Sprint("g", i)}}}
	})
	sched := testkit.RequestSessions(6, func(i int) string { return "Automation: hourly monitor\nAutomation ID: m-1" }, func(i int) []trace.Call {
		return []trace.Call{testkit.ShellCall("git fetch --all"), testkit.ShellCall(fmt.Sprintf("git log --oneline -%d", i+2))}
	})
	for i := range sched {
		sched[i].ID = "s" + sched[i].ID
	}
	o := DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: append(book, sched...)}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, r := range rep.Routines {
		kinds[r.Kind] = true
		if r.Statistics != "not_run" || r.Validation != "not_run" || r.ID == "" {
			t.Errorf("routine %s: statistics %q id %q", model.LabelsOf(r.Candidate), r.Statistics, r.ID)
		}
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), `"qualified"`) || strings.Contains(string(b), `"q":`) {
			t.Errorf("a request routine must not claim pattern statistics: %s", b)
		}
	}
	if !kinds["bookkeeping"] || !kinds["scheduled"] {
		t.Fatalf("kinds = %v", kinds)
	}
	rs := []model.Routine{
		{ID: "a", Kind: "user", Family: "fam_1", Decision: "primitive", Candidate: model.Candidate{Steps: []model.StepTemplate{{Label: "sh:git status"}, {Label: "sh:git diff"}}}},
		// Same kind, same steps in the same order: one procedure found twice.
		// (A different order is a different procedure: see C13.)
		{ID: "b", Kind: "user", Family: "fam_1", Decision: "primitive", Candidate: model.Candidate{Steps: []model.StepTemplate{{Label: "sh:git status"}, {Label: "sh:git diff"}}}},
		{ID: "c", Kind: "scheduled", Family: "fam_1", Decision: "primitive", Candidate: model.Candidate{Steps: []model.StepTemplate{{Label: "sh:git status"}, {Label: "sh:git diff"}}}},
	}
	if n := routine.MergeDuplicates(rs); n != 1 || rs[1].MergedInto != "a" || rs[2].MergedInto != "" {
		t.Fatalf("merged %d: %+v", n, rs)
	}
}
