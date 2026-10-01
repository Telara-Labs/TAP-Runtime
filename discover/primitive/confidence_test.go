package primitive

import (
	"fmt"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func dim(c Confidence, name string) Dimension {
	for _, d := range c.Dimensions {
		if d.Name == name {
			return d
		}
	}
	return Dimension{}
}

// Many strong bindings and one unresolved selection: the overall score shows
// the weak point and the flow needs a decision.
func TestOneWeakClaimIsNotAveragedAway(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("PIPE-%d1", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"check the build"},
			call("mcp:pipelines_list", map[string]string{"project": "web"}, `{"pipelines":[{"id":"`+id+`"},{"id":"PIPE-999"}]}`, 0, 0),
			call("mcp:jobs_list", map[string]string{"pipeline_id": id}, `{"jobs":[]}`, 0, time.Second)))
	}
	c := find(t, Discover(ss, nil), "mcp:pipelines_list", "mcp:jobs_list").Confidence
	if c.Overall != 25 || dim(c, "transformations").Score != 25 || c.Readiness != "needs_decision" || len(c.Requirements) == 0 {
		t.Fatalf("weak selection hidden: %+v", c)
	}
}

// A structured binding seen in many executions is corroborated, never
// established by repetition alone; the flow is a candidate.
func TestRepetitionAloneDoesNotEstablish(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000003%d", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"start the work"},
			call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, time.Second)))
	}
	p := find(t, Discover(ss, nil), "mcp:task_create", "mcp:task_checkpoint")
	var bind *Claim
	for i, cl := range p.Confidence.Claims {
		if cl.Subject == "step 2 task_id" {
			bind = &p.Confidence.Claims[i]
		}
	}
	if bind == nil || bind.Score != 75 || p.Confidence.Readiness != "candidate" || p.Confidence.Overall != 50 {
		t.Fatalf("binding %+v overall %d readiness %s", bind, p.Confidence.Overall, p.Confidence.Readiness)
	}
}

// Without call times the boundary claim is missing evidence, scored 0.
func TestMissingTimesScoreZero(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000004%d", i)
		a := call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0)
		b := call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, time.Second)
		a.Time, b.Time = time.Time{}, time.Time{}
		ss = append(ss, session(fmt.Sprint("s", i), []string{"start the work"}, a, b))
	}
	c := find(t, Discover(ss, nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	if d := dim(c, "boundaries"); !d.Applicable || d.Score != 0 || c.Overall != 0 {
		t.Fatalf("missing times not scored as missing: %+v", c)
	}
}

// A long gap inside an execution is flagged for review, not cut.
func TestLongGapNeedsReview(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000005%d", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"start the work"},
			call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, 25*time.Minute)))
	}
	c := find(t, Discover(ss, nil), "mcp:task_create", "mcp:task_checkpoint").Confidence
	if dim(c, "boundaries").Score != 25 || c.Readiness != "needs_decision" {
		t.Fatalf("long gap not flagged: %+v", c)
	}
}
