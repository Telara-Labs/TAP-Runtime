package primitive

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

var t0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

var callSeq int

// call builds a recorded call with its result parsed the way the readers
// parse one.
func call(tool string, args map[string]string, out string, req int, at time.Duration) trace.Call {
	c := trace.Call{Tool: tool, Args: args, Output: out, Request: req, Time: t0.Add(at), Outcome: trace.OutcomeOK}
	callSeq++
	c.ID = fmt.Sprintf("toolu_%04d", callSeq)
	c.OutIDs, c.OutCtx, c.OutPaths = trace.OutputRefsPaths(out)
	return c
}

func shell(cmd, out string, req int, at time.Duration) trace.Call {
	c := call("shell", nil, out, req, at)
	c.Command = cmd
	return c
}

func session(id string, requests []string, calls ...trace.Call) trace.Session {
	return trace.Session{Client: "claude-code", ID: id, Requests: requests, Calls: calls}
}

func find(t *testing.T, res Result, steps ...string) *Primitive {
	t.Helper()
	for i := range res.Primitives {
		p := &res.Primitives[i]
		if strings.Join(p.Steps, " > ") == strings.Join(steps, " > ") {
			return p
		}
	}
	var got []string
	for _, p := range res.Primitives {
		got = append(got, strings.Join(p.Steps, " > "))
	}
	t.Fatalf("no primitive %v among %v", steps, got)
	return nil
}

func binding(p *Primitive, step int, arg string) *Binding {
	for i := range p.Bindings {
		if p.Bindings[i].Step == step && p.Bindings[i].Arg == arg {
			return &p.Bindings[i]
		}
	}
	return nil
}

// A created task's ID consumed by the immediate checkpoint is an explicit
// result binding, and the pair is one eligible flow.
func TestCreatedIDFeedingImmediateCheckpointIsExplicit(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000000%d", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"start the work"},
			call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, time.Second)))
	}
	p := find(t, Discover(ss, nil), "mcp:task_create", "mcp:task_checkpoint")
	b := binding(p, 2, "task_id")
	if b == nil || b.Source != "step" || b.From != 1 || b.Label != Explicit || b.Selector == "" {
		t.Fatalf("task_id binding = %+v", b)
	}
	if p.ExecutionCount != 2 || len(p.Executions) != 2 || p.Executions[0].Calls[0].ID == "" {
		t.Fatalf("executions not indexed with call IDs: %+v", p.Executions)
	}
}

// A grep line that merely mentions an ID does not prove grep supplied it.
func TestTextMentionIsNotAProvenEdge(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("TENG-%d", 40+i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"look around"},
			shell("grep -rn TODO notes", "notes/a.md:12: see "+key+" for details", 0, 0),
			call("mcp:issue_get", map[string]string{"issue_key": key}, `{"key":"`+key+`"}`, 0, time.Second)))
	}
	for _, p := range Discover(ss, nil).Primitives {
		for _, e := range p.Edges {
			t.Fatalf("a text mention became an edge: %v %v", p.Steps, e)
		}
	}
}

// A value produced in an earlier request is not linked across that
// boundary: there it is the caller's input.
func TestEarlierRequestIsAnExecutionBoundary(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000001%d", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"create it", "later, checkpoint"},
			call("mcp:task_create", map[string]string{"goal": "g"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "m"}, `{"ok":true}`, 1, 25*time.Minute),
			call("mcp:task_complete", map[string]string{"task_id": id}, `{"ok":true}`, 1, 26*time.Minute)))
	}
	res := Discover(ss, nil)
	for _, p := range res.Primitives {
		if p.Steps[0] == "mcp:task_create" && len(p.Steps) > 1 {
			t.Fatalf("linked across a request boundary: %v", p.Steps)
		}
	}
}

// A path one command prints and the next consumes is a real binding.
func TestShellPathOutputIsABinding(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		path := fmt.Sprintf("reports/run%d.csv", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"summarize the newest report"},
			shell("ls -t reports", path+"\nreports/old.csv", 0, 0),
			shell("wc -l "+path, "12 "+path, 0, time.Second)))
	}
	p := find(t, Discover(ss, nil), "sh:ls", "sh:wc")
	if len(p.Edges) != 1 || !strings.HasPrefix(p.Edges[0], "1>2:") {
		t.Fatalf("path binding lost: %+v", p.Bindings)
	}
}

// Items of one returned collection, each fetched, are one loop in one
// execution: two runs count two, not four.
func TestCollectionIterationIsOneExecution(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		a, b := fmt.Sprintf("POD-%d001", i), fmt.Sprintf("POD-%d002", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"collect the logs"},
			call("mcp:pods_list", map[string]string{"ns": "web"}, `{"items":[{"id":"`+a+`"},{"id":"`+b+`"}]}`, 0, 0),
			call("mcp:pod_logs", map[string]string{"id": a}, "line", 0, time.Second),
			call("mcp:pod_logs", map[string]string{"id": b}, "line", 0, 2*time.Second)))
	}
	p := find(t, Discover(ss, nil), "mcp:pods_list", "mcp:pod_logs")
	if len(p.Loops) != 1 || p.Loops[0] != 2 || p.ExecutionCount != 2 {
		t.Fatalf("loop %v executions %d", p.Loops, p.ExecutionCount)
	}
}

// One item taken from a returned list, with no loop, needs a selection rule.
func TestSelectionFromListIsUnresolved(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("PIPE-%d1", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"check the build"},
			call("mcp:pipelines_list", map[string]string{"project": "web"}, `{"pipelines":[{"id":"`+id+`"},{"id":"PIPE-999"}]}`, 0, 0),
			call("mcp:jobs_list", map[string]string{"pipeline_id": id}, `{"jobs":[]}`, 0, time.Second)))
	}
	p := find(t, Discover(ss, nil), "mcp:pipelines_list", "mcp:jobs_list")
	if !strings.Contains(strings.Join(p.Unresolved, "\n"), "selection rule unknown") {
		t.Fatalf("selection not flagged: %v", p.Unresolved)
	}
}

// Two creates feeding the two roles of a link stay distinct steps.
func TestTwoCreatesKeepDistinctRoles(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		x, y := fmt.Sprintf("KEY-%d1", i), fmt.Sprintf("KEY-%d2", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"file two linked issues"},
			call("mcp:issue_create", map[string]string{"summary": "a b"}, `{"key":"`+x+`"}`, 0, 0),
			call("mcp:issue_create", map[string]string{"summary": "c d"}, `{"key":"`+y+`"}`, 0, time.Second),
			call("mcp:issue_link", map[string]string{"inward": x, "outward": y}, `{"ok":true}`, 0, 2*time.Second)))
	}
	p := find(t, Discover(ss, nil), "mcp:issue_create", "mcp:issue_create", "mcp:issue_link")
	in, out := binding(p, 3, "inward"), binding(p, 3, "outward")
	if in == nil || out == nil || in.From == out.From || in.From == 0 || out.From == 0 {
		t.Fatalf("roles collapsed: inward %+v outward %+v", in, out)
	}
}

// A value the user typed is the caller's input, whatever echoes it later.
func TestRequestValueIsAnInput(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("TENG-%d7", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"comment on " + key},
			call("mcp:issue_get", map[string]string{"issue_key": key}, `{"key":"`+key+`","id":"77`+fmt.Sprint(i)+`01"}`, 0, 0),
			call("mcp:issue_comment", map[string]string{"issue_key": key, "body": "done here"}, `{"ok":true}`, 0, time.Second)))
	}
	for _, p := range Discover(ss, nil).Primitives {
		if b := binding(&p, 2, "issue_key"); b != nil && b.Source == "step" {
			t.Fatalf("the request's value was credited to a step: %+v", b)
		}
	}
}

// Two operations on the same item are a sequence, not an iteration.
func TestSameItemTwiceIsNotALoop(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("KEY-%d9", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"file it and move it along"},
			call("mcp:issue_create", map[string]string{"summary": "a b"}, `{"key":"`+key+`"}`, 0, 0),
			call("mcp:issue_transition", map[string]string{"issue_key": key, "to": "doing"}, `{"ok":true}`, 0, time.Second),
			call("mcp:issue_transition", map[string]string{"issue_key": key, "to": "done"}, `{"ok":true}`, 0, 2*time.Second)))
	}
	for _, p := range Discover(ss, nil).Primitives {
		if len(p.Loops) > 0 {
			t.Fatalf("repeated operations on one item were read as a loop: %v loops %v", p.Steps, p.Loops)
		}
	}
}
