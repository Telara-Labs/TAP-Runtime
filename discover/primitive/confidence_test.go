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

// A consistent binding is fully consistent however often it was seen, and
// the run count is not folded into the score (it is shown on its own).
func TestRunCountIsNotFoldedIntoTheScore(t *testing.T) {
	few := find(t, Discover(createCheckpoint(2, time.Second), nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	many := find(t, Discover(createCheckpoint(40, time.Second), nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	a, b := claim(few, "step 2 task_id"), claim(many, "step 2 task_id")
	if a == nil || b == nil || a.Score != 100 || b.Score != 100 || a.Support != "2/2" {
		t.Fatalf("a consistent binding is not fully consistent: few %+v many %+v", a, b)
	}
	if few.Overall != many.Overall || many.Readiness != "candidate" {
		t.Fatalf("few %d many %d (%s)", few.Overall, many.Overall, many.Readiness)
	}
}

// A value only mentioned in earlier text has no proven source: by rule it is
// a caller input, not a penalty.
func TestTextMentionIsAnInputNotAPenalty(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("TENG-%d3", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"look around"},
			call("mcp:issue_create", map[string]string{"summary": "a b"}, `{"key":"KEY-`+fmt.Sprint(i)+`1"}`, 0, 0),
			call("mcp:issue_link", map[string]string{"inward": "KEY-" + fmt.Sprint(i) + "1", "outward": key}, `{"ok":true}`, 0, time.Second)))
		ss[i].Calls[0].Output = `{"key":"KEY-` + fmt.Sprint(i) + `1","note":"see ` + key + ` later"}`
		ss[i].Calls[0].OutIDs, ss[i].Calls[0].OutCtx, ss[i].Calls[0].OutPaths = trace.OutputRefsPaths(ss[i].Calls[0].Output)
	}
	c := find(t, Discover(ss, nil), "mcp:issue_create", "mcp:issue_link").Confidence
	if c.Overall < 90 || c.Readiness != "candidate" {
		t.Fatalf("a text mention was penalized: %+v", c)
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
