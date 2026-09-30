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
	if r.Failed != CheckReplays {
		t.Fatalf("failed = %q", r.Failed)
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
