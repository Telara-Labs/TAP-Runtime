package discover

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// requestSessions builds n sessions, each one request running the calls
// make(i) returns, a few days apart.
func requestSessions(n int, text func(i int) string, calls func(i int) []Call) []Session {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []Session
	for i := 0; i < n; i++ {
		s := Session{Client: "fake", ID: fmt.Sprintf("f%02d", i), Start: t0.AddDate(0, 0, 4*i)}
		s.addRequest(text(i))
		for _, c := range calls(i) {
			c.Request, c.Time = 0, s.Start
			s.Calls = append(s.Calls, c)
		}
		out = append(out, s)
	}
	return out
}

func sh(cmd string) Call { return Call{Tool: "shell", Command: cmd} }

func firstRoutine(t *testing.T, ss []Session) *Routine {
	t.Helper()
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: ss}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Routines) == 0 {
		t.Fatalf("no routine: %+v", rep.Funnel)
	}
	return &rep.Routines[0]
}

func TestPipelinesAndCdAreReplayedAsRecorded(t *testing.T) {
	ss := requestSessions(8, func(i int) string { return "test the package" }, func(i int) []Call {
		dir := fmt.Sprintf("services/svc%d", i)
		return []Call{
			sh("cd " + dir + " && go test ./... -count=1 2>&1 | tail -20"),
			sh("git status --short"),
		}
	})
	r := firstRoutine(t, ss)
	sh := string(r.Draft().Files["main.sh"])
	if !strings.Contains(sh, `cd "${1}" && go test ./... -count=1 2>&1 | tail -20`) {
		t.Fatalf("the pipeline and cd must be one recorded line with the directory as input:\n%s", sh)
	}
	if strings.Contains(sh, "\ngo test") || strings.Contains(sh, "\ntail -20") {
		t.Fatalf("a pipeline was split into separate commands:\n%s", sh)
	}
}

func TestDifferingHeredocBodiesNeedAuthoring(t *testing.T) {
	ss := requestSessions(8, func(i int) string { return "count the rows" }, func(i int) []Call {
		return []Call{
			sh("git status --short"),
			sh(fmt.Sprintf("python3 - <<'PY'\nimport csv\nprint(%d * len(list(csv.reader(open('data.csv')))))\nPY", i)),
		}
	})
	r := firstRoutine(t, ss)
	d := r.Draft()
	if d.HumanSteps != 1 {
		t.Fatalf("a heredoc whose body differs every run must be an authoring step: %+v", d.Steps)
	}
	// Judgment inside the procedure: not a useful procedure (plan section 3,
	// item 6), not "needs authoring".
	if r.Suitability != SuitInsufficient || !strings.Contains(r.Why, "judgment_step") {
		t.Fatalf("suitability %q (%s)", r.Suitability, r.Why)
	}
}

func TestDraftFollowsARecordedOrder(t *testing.T) {
	// Five runs do status, diff, log; three do log, status, diff. The draft
	// must be one of those orders, never a mix.
	ss := requestSessions(8, func(i int) string { return "what changed" }, func(i int) []Call {
		if i < 5 {
			return []Call{sh("git status --short"), sh("git diff --stat"), sh("git log --oneline -3")}
		}
		return []Call{sh("git log --oneline -3"), sh("git status --short"), sh("git diff --stat")}
	})
	r := firstRoutine(t, ss)
	got := labelsOf(r.Candidate)
	if got != "sh:git status → sh:git diff → sh:git log" {
		t.Fatalf("draft order = %s", got)
	}
	if r.Consistency != 5.0/8 {
		t.Fatalf("consistency = %v, want 5/8", r.Consistency)
	}
}

func TestDistinctCallsWithTheSameLabelAreKept(t *testing.T) {
	ss := requestSessions(8, func(i int) string { return "compare the two configs" }, func(i int) []Call {
		return []Call{
			{Tool: "Read", Args: map[string]string{"file_path": "config/a.yaml"}},
			{Tool: "Read", Args: map[string]string{"file_path": "config/b.yaml"}},
			sh("git diff --stat"),
		}
	})
	r := firstRoutine(t, ss)
	if got := labelsOf(r.Candidate); got != "Read → Read → sh:git diff" {
		t.Fatalf("two reads of different files are two steps: %s", got)
	}
}

func TestFailedRunsAreNotEvidence(t *testing.T) {
	ss := requestSessions(10, func(i int) string { return "ship it" }, func(i int) []Call {
		status := sh("git status --short")
		push := sh(fmt.Sprintf("git push origin feature-%d", i))
		status.Outcome, push.Outcome = OutcomeOK, OutcomeOK
		if i >= 4 {
			push.Outcome = OutcomeFailed // six of ten pushes failed
		}
		return []Call{status, push}
	})
	r := firstRoutine(t, ss)
	if r.Runs != 10 || r.FailedRuns != 6 || len(r.Draft().Inputs) != 1 {
		t.Fatalf("runs %d failed %d", r.Runs, r.FailedRuns)
	}
	if r.Consistency != 0.4 {
		t.Fatalf("only the four successful runs count: consistency = %v", r.Consistency)
	}
}

func TestAValueFromAnEarlierOutputIsTakenFromIt(t *testing.T) {
	// Every run read the thread the search returned, and the id always sat
	// after the same text: the draft takes it from the search's output.
	ss := requestSessions(8, func(i int) string { return "reply to the intro email" }, func(i int) []Call {
		thread := fmt.Sprintf("18c%013x", 0xabc0+i)
		search := Call{Tool: "mcp:gmail_search_emails", Args: map[string]string{"query": "intro"}, Outcome: OutcomeOK}
		search.OutIDs, search.OutCtx, search.OutPaths = outputRefsPaths(fmt.Sprintf(`{"count":%d,"threads":[{"id":"%s","subject":"hi"}]}`, 3+i, thread))
		read := Call{Tool: "mcp:gmail_read_email_thread", Args: map[string]string{"thread_id": thread}, Outcome: OutcomeOK}
		return []Call{search, read}
	})
	r := firstRoutine(t, ss)
	d := r.Draft()
	if len(d.Inputs) != 1 || d.Inputs[0].DerivedFrom != 1 || d.Inputs[0].Extract == "" || d.Inputs[0].Position != 0 {
		t.Fatalf("the thread id came from step 1's output after the same text in every run: %+v", d.Inputs)
	}
	sh := string(d.Files["main.sh"])
	for _, want := range []string{
		`out1=$(tap call gmail_search_emails`,
		`printf '%s\n' "$out1"`,
		`gmail_read_email_thread_thread_id=$(printf '%s\n' "$out1" | jq -er '.threads[0].id | select(type == "string" or type == "number")')`,
		`--arg a1 "${gmail_read_email_thread_thread_id}"`,
	} {
		if !strings.Contains(sh, want) {
			t.Fatalf("main.sh lacks %q:\n%s", want, sh)
		}
	}
	if r.Decision != "primitive" || d.Derived != 0 || d.Extracted != 1 {
		t.Fatalf("decision %q derived %d extracted %d (%s)", r.Decision, d.Derived, d.Extracted, r.Why)
	}
	if _, ok := d.Files["primitive.yaml"]; !ok || strings.Contains(string(d.Files["primitive.yaml"]), "gmail_read_email_thread_thread_id") {
		t.Fatalf("an extracted value is not the caller's argument:\n%s", d.Files["primitive.yaml"])
	}
}

func TestAValueWithNoCommonAnchorNeedsAuthoring(t *testing.T) {
	ss := requestSessions(8, func(i int) string { return "reply to the intro email" }, func(i int) []Call {
		thread := fmt.Sprintf("18c%013x", 0xabc0+i)
		search := Call{Tool: "mcp:gmail_search_emails", Args: map[string]string{"query": "intro"}, Outcome: OutcomeOK}
		// The id sits after different text each run.
		search.OutIDs, search.OutCtx = outputRefs(fmt.Sprintf("result %d: %s", i, thread)[len("result "):])
		search.OutIDs, search.OutCtx = outputRefs(strings.Repeat("xy", i) + " " + thread)
		read := Call{Tool: "mcp:gmail_read_email_thread", Args: map[string]string{"thread_id": thread}, Outcome: OutcomeOK}
		return []Call{search, read}
	})
	r := firstRoutine(t, ss)
	d := r.Draft()
	if len(d.Inputs) != 1 || d.Inputs[0].DerivedFrom != 1 || d.Inputs[0].Extract != "" {
		t.Fatalf("inputs: %+v", d.Inputs)
	}
	if r.Decision != "needs_authoring" || !strings.Contains(string(d.Files["README.md"]), "step 1's output supplied it") {
		t.Fatalf("decision %q; README:\n%s", r.Decision, d.Files["README.md"])
	}
}

func TestTenOrMoreArgumentsAreBraced(t *testing.T) {
	ss := requestSessions(6, func(i int) string { return "file the report" }, func(i int) []Call {
		args := map[string]string{}
		for k := 0; k < 11; k++ {
			args[fmt.Sprintf("f%02d", k)] = fmt.Sprintf("v%d-%d", i, k)
		}
		return []Call{{Tool: "mcp:telara_tool_search", Args: map[string]string{"query": "x"}}, {Tool: "mcp:report_file", Args: args}}
	})
	sh := string(firstRoutine(t, ss).Draft().Files["main.sh"])
	if strings.Contains(sh, `"$10"`) || !strings.Contains(sh, `"${11}"`) {
		t.Fatalf("$10 is $1 followed by 0 in the shell:\n%s", sh)
	}
}

func TestAStepRepeatedWithDifferentValuesIsALoop(t *testing.T) {
	ss := requestSessions(9, func(i int) string { return "compare the configs" }, func(i int) []Call {
		var cs []Call
		for f := 0; f < 2+i%3; f++ { // 2, 3 or 4 files per run
			cs = append(cs, Call{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("config/%d-%d.yaml", i, f)}})
		}
		return append(cs, sh("git diff --stat"))
	})
	r := firstRoutine(t, ss)
	if labelsOf(r.Candidate) != "Read → sh:git diff" || len(r.Loops) != 1 || r.Loops[0] != "Read" {
		t.Fatalf("labels %s loops %v", labelsOf(r.Candidate), r.Loops)
	}
	// The files read were not given by the request: a loop over an unknown
	// collection is not a loop to write. The request is one stated task, so
	// the program abstains rather than calling it an investigation.
	if r.Suitability == SuitUseful || !strings.Contains(r.Why, ":Read") {
		t.Fatalf("suitability %q why %q", r.Suitability, r.Why)
	}
}

func TestFunnelCountsMatchDecisions(t *testing.T) {
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: append(requestCorpus(), credCorpus()...)}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	prims, authoring, removed := 0, 0, 0
	for _, r := range rep.Routines {
		if r.MergedInto != "" {
			continue
		}
		switch r.Decision {
		case "primitive":
			prims++
		case "needs_authoring":
			authoring++
		case "removed":
			removed++
		default:
			t.Fatalf("routine without a decision: %s", labelsOf(r.Candidate))
		}
	}
	sumRemoved := 0
	for _, n := range rep.Funnel.Removed {
		sumRemoved += n
	}
	if prims != rep.Funnel.Primitives || authoring != rep.Funnel.NeedsAuthoring || removed != sumRemoved || len(rep.Primitives()) != prims {
		t.Fatalf("funnel %+v vs %d/%d/%d", rep.Funnel, prims, authoring, removed)
	}
}
