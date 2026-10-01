package primitive

import (
	"fmt"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func claim(c Confidence, subject string) *Claim {
	for i := range c.Claims {
		if c.Claims[i].Subject == subject {
			return &c.Claims[i]
		}
	}
	return nil
}

// createCheckpoint is n runs of create then checkpoint, the ID taken from
// the created task's result.
func createCheckpoint(n int, gap time.Duration) []trace.Session {
	var ss []trace.Session
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-0000000%05d", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"start the work"},
			call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, gap)))
	}
	return ss
}

// A consistent binding is fully consistent however often it was seen; the
// primitive's score rises with the runs behind it.
func TestMoreRunsRaiseTheScore(t *testing.T) {
	few := find(t, Discover(createCheckpoint(2, time.Second), nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	many := find(t, Discover(createCheckpoint(40, time.Second), nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	a, b := claim(few, "step 2 task_id"), claim(many, "step 2 task_id")
	if a == nil || b == nil || a.Score != 100 || b.Score != 100 || a.Support != "2/2" {
		t.Fatalf("a consistent binding is not fully consistent: few %+v many %+v", a, b)
	}
	if few.Overall >= many.Overall || many.Overall < 90 {
		t.Fatalf("run count does not discount: few %d many %d", few.Overall, many.Overall)
	}
	if many.Readiness != "candidate" {
		t.Fatalf("consistent flow needs a decision: %+v", many.NeedsReview)
	}
}

// A value typed in some runs and taken from the step in others is two ways
// to supply one input, not a contradiction.
func TestTypedOrTakenIsNotAContradiction(t *testing.T) {
	ss := createCheckpoint(6, time.Second)
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-0000000%05d", i)
		ss[i].Requests = []string{"start the work on " + id}
	}
	c := find(t, Discover(ss, nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	if c.Readiness != "candidate" || claim(c, "step 2 task_id").Score < 50 {
		t.Fatalf("typed values were scored as contradictions: %+v", c)
	}
}

// One item taken from a returned list: a fixed position is a codeable rule,
// different positions are unresolved.
func TestSelectionConsistencyIsCalculated(t *testing.T) {
	run := func(pick []int) Confidence {
		var ss []trace.Session
		for i, at := range pick {
			ids := []string{fmt.Sprintf("PIPE-%d1", i), fmt.Sprintf("PIPE-%d2", i)}
			ss = append(ss, session(fmt.Sprint("s", i), []string{"check the build"},
				call("mcp:pipelines_list", map[string]string{"project": "web"}, `{"pipelines":[{"id":"`+ids[0]+`"},{"id":"`+ids[1]+`"}]}`, 0, 0),
				call("mcp:jobs_list", map[string]string{"pipeline_id": ids[at]}, `{"jobs":[]}`, 0, time.Second)))
		}
		return find(t, Discover(ss, nil), "mcp:pipelines_list", "mcp:jobs_list").Confidence
	}
	fixed := run([]int{0, 0, 0, 0, 0, 0})
	mixed := run([]int{0, 1, 0, 1, 1, 0})
	if fixed.Readiness != "candidate" || len(fixed.Requirements) != 0 {
		t.Fatalf("a fixed first item is unresolved: %+v", fixed)
	}
	if mixed.Readiness != "needs_decision" || len(mixed.Requirements) == 0 || mixed.Overall >= fixed.Overall {
		t.Fatalf("mixed positions not flagged: %+v", mixed)
	}
}

// Long gaps are noted, not scored, until the cutoff is validated.
func TestLongGapIsANoteNotAPenalty(t *testing.T) {
	quick := find(t, Discover(createCheckpoint(6, time.Second), nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	slow := find(t, Discover(createCheckpoint(6, 25*time.Minute), nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	if slow.Overall != quick.Overall || len(slow.Notes) == 0 {
		t.Fatalf("quick %d slow %d notes %v", quick.Overall, slow.Overall, slow.Notes)
	}
}

func TestWilsonBounds(t *testing.T) {
	if w := wilson(1, 2); w > 0.4 {
		t.Fatalf("2 of 2 = %.2f, too sure", w)
	}
	if w := wilson(1, 90); w < 0.95 {
		t.Fatalf("90 of 90 = %.2f, too unsure", w)
	}
	if wilson(0.5, 0) != 0 {
		t.Fatal("no runs must score 0")
	}
}
