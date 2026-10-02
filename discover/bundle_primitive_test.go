package discover

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"
	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func causalBundleFixture() (primitive.Family, []primitive.Primitive, []trace.Session) {
	f := primitive.Family{ID: "pf_causal", Head: "mcp:issue_create", ExecutionCount: 4, SessionCount: 4}
	var members []primitive.Primitive
	var sessions []trace.Session
	for _, name := range []string{"issue_comment", "issue_link"} {
		id := "pr_a1b2c3"
		if name == "issue_link" {
			id = "pr_d4e5f6"
		}
		p := primitive.Primitive{ID: id, Steps: []string{"mcp:issue_create", "mcp:" + name},
			StepEffects: []string{"write", "write"}, ExecutionCount: 2, SessionCount: 2,
			Bindings: []primitive.Binding{{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: ".key", Label: primitive.Explicit}}}
		for i := 0; i < 2; i++ {
			id := name + strconv.Itoa(i)
			key := "OLD-" + strconv.Itoa(i)
			args := map[string]string{"issue_key": key}
			if name == "issue_comment" {
				args["body"] = "comment " + strconv.Itoa(i)
			} else {
				args["target"] = "OTHER-" + strconv.Itoa(i)
			}
			sessions = append(sessions, trace.Session{Client: "claude-code", ID: id, Calls: []trace.Call{
				{Tool: "mcp:issue_create", MCPServer: "test", MCPTool: "issue_create", Args: map[string]string{"summary": "issue " + strconv.Itoa(i)}, Output: `{"key":"` + key + `"}`, Outcome: trace.OutcomeOK},
				{Tool: "mcp:" + name, MCPServer: "test", MCPTool: name, Args: args, Output: `{"ok":true}`, Outcome: trace.OutcomeOK},
			}})
			p.Executions = append(p.Executions, primitive.Execution{Client: "claude-code", Session: id,
				Calls:    []primitive.CallRef{{Step: 1, Index: 0}, {Step: 2, Index: 1}},
				Observed: []primitive.Observed{{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: ".key", Label: primitive.Explicit}}})
		}
		members = append(members, p)
		f.Members = append(f.Members, p.ID)
		f.FollowUps = append(f.FollowUps, primitive.FollowUp{Steps: []string{"mcp:" + name}, Runs: 2, Members: []string{p.ID}})
	}
	return f, members, sessions
}

func TestCausalBundleExecutesZeroOrSeveralTypedFollowUps(t *testing.T) {
	f, members, sessions := causalBundleFixture()
	by := map[string]*trace.Session{}
	for i := range sessions {
		by[sessions[i].Client+"\x00"+sessions[i].ID] = &sessions[i]
	}
	g, why := bundleGraph(f, members, by)
	if g == nil {
		t.Fatal(why)
	}
	pkg, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, tc := range []struct {
		name, input, want string
	}{
		{"create only", `{"step_1_summary":"fresh"}`, `[["step_1", {"summary": "fresh"}]]`},
		{"two reactions", `{"step_1_summary":"fresh","issue_comment_items":[{"body":"first"},{"body":"second"}],"issue_link_items":[{"target":"OTHER-9"}]}`,
			`[["step_1", {"summary": "fresh"}], ["step_2", {"body": "first", "issue_key": "NEW-9"}], ["step_2", {"body": "second", "issue_key": "NEW-9"}], ["step_3", {"issue_key": "NEW-9", "target": "OTHER-9"}]]`},
		{"invalid before create", `{"step_1_summary":"fresh","issue_link_items":[{"target":7}]}`, `[]`},
		{"unknown field before create", `{"step_1_summary":"fresh","issue_link_items":[{"target":"OTHER-9","extra":"ignored"}]}`, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := strings.Join([]string{
				"import json, sys",
				"calls = []",
				"class Tap:",
				"    def call(self, alias, args):",
				"        calls.append([alias, args])",
				"        return {'key': 'NEW-9'} if alias == 'step_1' else {'ok': True}",
				"sys.argv = ['main.py', " + strconv.Quote(tc.input) + "]",
				"try:",
				"    exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
				"except ValueError:",
				"    pass",
				"print('CALLS=' + json.dumps(calls))",
			}, "\n")
			cmd := exec.Command(python, "-c", harness)
			cmd.Stdin = strings.NewReader(string(pkg.Files["main.py"]))
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "CALLS="+tc.want) {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}

func TestUnrelatedContinuationDoesNotJoinCausalBundle(t *testing.T) {
	f, members, sessions := causalBundleFixture()
	// The second operation follows create in the same request, but its
	// target came from the caller rather than from the newly created key.
	for i := range members[1].Executions {
		members[1].Executions[i].Observed[0] = primitive.Observed{Step: 2, Arg: "issue_key", Source: "input", Label: primitive.Missing}
	}
	res := primitive.Result{Families: []primitive.Family{f}, Primitives: members}
	planPrimitiveFamilies(&res, sessions)
	got := res.Families[0]
	if got.RelationshipConfidence != 0 || got.FollowUps[1].RelationshipScore != 0 || got.FollowUps[1].APIMode != "needs_refinement" {
		t.Fatalf("unrelated path was admitted: %+v", got)
	}
	if got.APIMode != "optional_followups" || len(got.APIInputs) != 1 || got.APIInputs[0] != "issue_comment_items" {
		t.Fatalf("strong path was lost: %+v", got)
	}
}

func TestCausalBundleScoresAndExcludesDifferentCallShape(t *testing.T) {
	f, members, sessions := causalBundleFixture()
	id := "comment_variant"
	sessions = append(sessions, trace.Session{Client: "claude-code", ID: id, Calls: []trace.Call{
		{Tool: "mcp:issue_create", MCPServer: "test", MCPTool: "issue_create", Args: map[string]string{"summary": "issue variant"}, Output: `{"key":"OLD-3"}`, Outcome: trace.OutcomeOK},
		{Tool: "mcp:issue_comment", MCPServer: "test", MCPTool: "issue_comment", Args: map[string]string{"issue_key": "OLD-3", "body": "variant", "extra": "yes"}, Output: `{"ok":true}`, Outcome: trace.OutcomeOK},
	}})
	members[0].Executions = append(members[0].Executions, primitive.Execution{Client: "claude-code", Session: id,
		Calls:    []primitive.CallRef{{Step: 1, Index: 0}, {Step: 2, Index: 1}},
		Observed: []primitive.Observed{{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: ".key", Label: primitive.Explicit}}})
	res := primitive.Result{Families: []primitive.Family{f}, Primitives: members}
	planPrimitiveFamilies(&res, sessions)
	got := res.Families[0]
	comment := got.FollowUps[0]
	if comment.RelationshipScore != 100 || comment.ShapeScore != 66 || comment.Confidence != 66 ||
		comment.APIMode != "exact_chain" || !strings.Contains(comment.APIReason, "another call shape") ||
		got.APIMode != "optional_followups" || got.APIConfidence != 66 {
		t.Fatalf("call-shape coverage was not scored and limited: %+v", got)
	}
}

func TestCausalBundleReportsPartialWrites(t *testing.T) {
	f, members, sessions := causalBundleFixture()
	by := map[string]*trace.Session{}
	for i := range sessions {
		by[sessions[i].Client+"\x00"+sessions[i].ID] = &sessions[i]
	}
	g, why := bundleGraph(f, members, by)
	if g == nil {
		t.Fatal(why)
	}
	pkg, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	harness := strings.Join([]string{
		"import sys",
		"class Tap:",
		"    def call(self, alias, args):",
		"        if alias == 'step_1': return {'key': 'NEW-9'}",
		"        if args['body'] == 'second': raise RuntimeError('second comment failed')",
		"        return {'ok': True}",
		"sys.argv = ['main.py', '{\"step_1_summary\":\"fresh\",\"issue_comment_items\":[{\"body\":\"first\"},{\"body\":\"second\"}]}' ]",
		"exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
	}, "\n")
	cmd := exec.Command(python, "-c", harness)
	cmd.Stdin = strings.NewReader(string(pkg.Files["main.py"]))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), `"failed_step": "step_2"`) ||
		!strings.Contains(string(out), `"step_1": {"key": "NEW-9"}`) ||
		!strings.Contains(string(out), `"step_2": [{"ok": true}]`) {
		t.Fatalf("partial completion missing: %v\n%s", err, out)
	}
}

func TestCausalBundlePlannerMatchesInstaller(t *testing.T) {
	f, members, sessions := causalBundleFixture()
	res := primitive.Result{Families: []primitive.Family{f}, Primitives: members}
	planPrimitiveFamilies(&res, sessions)
	planned := res.Families[0]
	if planned.APIMode != "optional_followups" || len(planned.APIInputs) != 2 {
		t.Fatalf("planner did not offer the bundle: %+v", planned)
	}
	root := t.TempDir()
	installed, err := primitiveInstaller(sessions, "claude-code", root, root)(planned, members)
	if err != nil || !installed.Installed {
		t.Fatalf("planner and installer disagree: result=%+v err=%v", installed, err)
	}
	if _, err := os.Stat(installed.Where); err != nil {
		t.Fatalf("generated package missing: %v", err)
	}
}
