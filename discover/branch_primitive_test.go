package discover

import (
	"fmt"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/codegen"
	"github.com/Telara-Labs/TAP-Runtime/discover/primitive"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestBundleGraphKeepsFifteenCausalOptionalFollowUps(t *testing.T) {
	const count = 15
	f := primitive.Family{ID: "pf_many", ExecutionCount: count * 2, SessionCount: count * 2}
	by := map[string]*trace.Session{}
	var members []primitive.Primitive
	for branch := 0; branch < count; branch++ {
		op := fmt.Sprintf("option_%02d", branch)
		p := primitive.Primitive{ID: fmt.Sprintf("pr_%012x", branch+1), Steps: []string{"mcp:issue_create", "mcp:" + op}, StepEffects: []string{"write", "write"}, ExecutionCount: 2, SessionCount: 2,
			Bindings: []primitive.Binding{{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: ".key", Label: primitive.Explicit}}}
		for use := 0; use < 2; use++ {
			sid := fmt.Sprintf("s%d_%d", branch, use)
			key := fmt.Sprintf("NEW-%d-%d", branch, use)
			s := &trace.Session{Client: "claude-code", ID: sid, Calls: []trace.Call{
				{Tool: "mcp:issue_create", MCPServer: "test", MCPTool: "issue_create", Args: map[string]string{"summary": fmt.Sprintf("issue %d", use)}, Output: `{"key":"` + key + `"}`, Outcome: trace.OutcomeOK},
				{Tool: "mcp:" + op, MCPServer: "test", MCPTool: op, Args: map[string]string{"issue_key": key, "body": fmt.Sprintf("text %d", use)}, Output: `{"ok":true}`, Outcome: trace.OutcomeOK},
			}}
			by["claude-code\x00"+sid] = s
			p.Executions = append(p.Executions, primitive.Execution{Client: "claude-code", Session: sid,
				Calls:    []primitive.CallRef{{Step: 1, Index: 0}, {Step: 2, Index: 1}},
				Observed: []primitive.Observed{{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: ".key", Label: primitive.Explicit}}})
		}
		members = append(members, p)
		f.Members = append(f.Members, p.ID)
		f.FollowUps = append(f.FollowUps, primitive.FollowUp{Steps: []string{"mcp:" + op}, Runs: 2, Optional: true, Members: []string{p.ID}})
	}
	g, reason := bundleGraph(f, members, by)
	if g == nil {
		t.Fatalf("bundle graph: %s", reason)
	}
	if len(g.Steps) != count+1 || len(g.Inputs) != count+1 {
		t.Fatalf("lost follow-ups: steps=%d inputs=%+v", len(g.Steps), g.Inputs)
	}
	for _, in := range g.Inputs[1:] {
		if !in.Optional || !in.List || in.Type != "object" || len(in.Fields) != 1 {
			t.Fatalf("follow-up is not an optional typed list: %+v", in)
		}
	}
	if _, err := codegen.GenerateProgramPackage(g); err != nil {
		t.Fatalf("fifteen-follow-up program did not compile: %v", err)
	}
	var sessions []trace.Session
	for _, s := range by {
		sessions = append(sessions, *s)
	}
	res := primitive.Result{Families: []primitive.Family{f}, Primitives: members}
	planPrimitiveFamilies(&res, sessions)
	if res.Families[0].APIMode != "optional_followups" || len(res.Families[0].APIInputs) != count || res.Families[0].RelationshipConfidence != 100 {
		t.Fatalf("review did not expose compiled optional paths: %+v", res.Families[0])
	}
	for _, fu := range res.Families[0].FollowUps {
		if fu.APIMode != "exact_chain" || fu.RelationshipScore != 100 {
			t.Fatalf("compiled continuation marked %q: %+v", fu.APIMode, fu)
		}
	}
}
