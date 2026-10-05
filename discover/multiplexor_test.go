package discover

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/codegen"
	"github.com/Telara-Labs/TAP-Runtime/discover/primitive"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// One head, three recorded follow-ups: a comment whose key was sent under
// two argument names, a create-then-link chain, and the comment twice. The
// result is one program that runs any combination of them.
func TestMultiplexorRunsSeveralStepFollowUpsAndMergesArgumentNames(t *testing.T) {
	f := primitive.Family{ID: "pf_mux"}
	by := map[string]*trace.Session{}
	var members []primitive.Primitive
	explicit := func(step, from int, arg string) primitive.Binding {
		return primitive.Binding{Step: step, Arg: arg, Source: "step", From: from, Selector: ".key", Label: primitive.Explicit}
	}
	observed := func(b primitive.Binding) primitive.Observed {
		return primitive.Observed{Step: b.Step, Arg: b.Arg, Source: b.Source, From: b.From, Selector: b.Selector, Label: b.Label}
	}
	add := func(id string, steps []string, binds []primitive.Binding, uses [][]trace.Call) {
		p := primitive.Primitive{ID: id, Steps: steps, ExecutionCount: len(uses), SessionCount: len(uses), Bindings: binds}
		for range steps {
			p.StepEffects = append(p.StepEffects, "write")
		}
		for u, calls := range uses {
			sid := fmt.Sprintf("%s_%d", id, u)
			by["claude-code\x00"+sid] = &trace.Session{Client: "claude-code", ID: sid, Calls: calls}
			ex := primitive.Execution{Client: "claude-code", Session: sid}
			for i := range calls {
				ex.Calls = append(ex.Calls, primitive.CallRef{Step: i + 1, Index: i})
			}
			for _, b := range binds {
				ex.Observed = append(ex.Observed, observed(b))
			}
			p.Executions = append(p.Executions, ex)
		}
		members = append(members, p)
		f.Members = append(f.Members, id)
		f.FollowUps = append(f.FollowUps, primitive.FollowUp{Steps: steps[1:], Runs: len(uses), Optional: true, Members: []string{id}})
	}
	call := func(tool string, args map[string]string, out string) trace.Call {
		return trace.Call{Tool: "mcp:" + tool, MCPServer: "jira", MCPTool: tool, Args: args, Output: out, Outcome: trace.OutcomeOK}
	}
	create := func(n int) trace.Call {
		return call("issue_create", map[string]string{"summary": fmt.Sprintf("s%d", n)}, fmt.Sprintf(`{"key":"K-%d"}`, n))
	}
	// The comment's key arrives as issue_key in two runs, issueIdOrKey in one.
	add("pr_comment", []string{"mcp:issue_create", "mcp:comment"},
		[]primitive.Binding{explicit(2, 1, "issue_key"), explicit(2, 1, "issueIdOrKey")},
		[][]trace.Call{
			{create(1), call("comment", map[string]string{"issue_key": "K-1", "body": "a"}, `{}`)},
			{create(2), call("comment", map[string]string{"issue_key": "K-2", "body": "b"}, `{}`)},
			{create(3), call("comment", map[string]string{"issueIdOrKey": "K-3", "body": "c"}, `{}`)},
		})
	// Create a second issue, then link the head (inward) to it (outward).
	add("pr_link", []string{"mcp:issue_create", "mcp:issue_create", "mcp:link"},
		[]primitive.Binding{explicit(3, 1, "inward"), explicit(3, 2, "outward")},
		[][]trace.Call{
			{create(4), create(5), call("link", map[string]string{"inward": "K-4", "outward": "K-5", "type": "Blocks"}, `{}`)},
			{create(6), create(7), call("link", map[string]string{"inward": "K-6", "outward": "K-7", "type": "Blocks"}, `{}`)},
		})
	// The comment twice: the same as two comment items.
	add("pr_twice", []string{"mcp:issue_create", "mcp:comment", "mcp:comment"},
		[]primitive.Binding{explicit(2, 1, "issue_key"), explicit(3, 1, "issue_key")},
		[][]trace.Call{
			{create(8), call("comment", map[string]string{"issue_key": "K-8", "body": "d"}, `{}`), call("comment", map[string]string{"issue_key": "K-8", "body": "e"}, `{}`)},
			{create(9), call("comment", map[string]string{"issue_key": "K-9", "body": "f"}, `{}`), call("comment", map[string]string{"issue_key": "K-9", "body": "g"}, `{}`)},
		})

	var sessions []trace.Session
	for _, s := range by {
		sessions = append(sessions, *s)
	}
	res := primitive.Result{Families: []primitive.Family{f}, Primitives: members}
	planPrimitiveFamilies(&res, sessions)
	planned := res.Families[0]
	if planned.APIMode != "optional_followups" || planned.APIReason != "" {
		t.Fatalf("family not planned as one program: %s / %s", planned.APIMode, planned.APIReason)
	}
	for _, fu := range planned.FollowUps {
		if fu.APIMode != "exact_chain" {
			t.Fatalf("follow-up %v left out: %s", fu.Steps, fu.APIReason)
		}
		if len(fu.Steps) == 2 && fu.Steps[0] == fu.Steps[1] && !strings.HasPrefix(fu.APIReason, "covered") {
			t.Fatalf("repeat not shown as covered: %q", fu.APIReason)
		}
	}
	if strings.Join(planned.APIInputs, ",") != "comment_items,issue_create_then_link_items" {
		t.Fatalf("want one list per distinct follow-up, got %v", planned.APIInputs)
	}

	keep, kept, left := executableFamily(f, members, by)
	if len(left) != 0 {
		t.Fatalf("left out: %v", left)
	}
	g, why := bundleGraph(keep, kept, by)
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
	inputs, _ := json.Marshal(map[string]any{
		"step_1_summary":               "head",
		"comment_items":                []map[string]string{{"body": "one"}, {"body": "two"}},
		"issue_create_then_link_items": []map[string]string{{"summary": "child"}},
	})
	harness := strings.Join([]string{
		"import json, sys",
		"calls = []",
		"class Tap:",
		"    def call(self, alias, args):",
		"        calls.append([alias, args])",
		"        return {'key': 'NEW-%d' % len(calls)}",
		"sys.argv = ['main.py', " + fmt.Sprintf("%q", string(inputs)) + "]",
		"exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
		"print('CALLS=' + json.dumps(calls, sort_keys=True))",
	}, "\n")
	cmd := exec.Command(python, "-c", harness)
	cmd.Stdin = strings.NewReader(string(pkg.Files["main.py"]))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated program failed: %v\n%s\n%s", err, out, pkg.Files["main.py"])
	}
	idx := strings.Index(string(out), "CALLS=")
	var calls [][]json.RawMessage
	if idx < 0 || json.Unmarshal([]byte(strings.TrimSpace(string(out[idx+6:]))), &calls) != nil {
		t.Fatalf("no call log: %s", out)
	}
	var got []string
	for _, c := range calls {
		var v map[string]any
		if json.Unmarshal(c[1], &v) != nil {
			t.Fatalf("call args are not an object: %s", c[1])
		}
		b, _ := json.Marshal(v)
		got = append(got, string(b))
	}
	want := []string{
		`{"summary":"head"}`,
		`{"body":"one","issue_key":"NEW-1"}`,
		`{"body":"two","issue_key":"NEW-1"}`,
		`{"summary":"child"}`,
		`{"inward":"NEW-1","outward":"NEW-4","type":"Blocks"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Most runs linked the issue just created; one linked an issue it typed in.
// That run is left out instead of making the key a caller input. A value
// every run passed becomes an input defaulting to it, except the arguments
// that choose a dispatched operation.
func TestMajorityResultBindingAndRecordedDefaults(t *testing.T) {
	by := map[string]*trace.Session{}
	p := primitive.Primitive{ID: "pr_link", Steps: []string{"mcp:create", "mcp:link"}, StepEffects: []string{"write", "write"},
		Bindings: []primitive.Binding{{Step: 2, Arg: "inward", Source: "step", From: 1, Selector: ".key", Label: primitive.Explicit, Contradicting: []string{"ex2"}}}}
	for i, inward := range []string{"K-0", "K-1", "OTHER-9"} {
		sid := fmt.Sprintf("s%d", i)
		key := fmt.Sprintf("K-%d", i)
		params, _ := json.Marshal(map[string]string{"inward": inward, "outward": fmt.Sprintf("O-%d", i), "type": "Blocks"})
		by["claude-code\x00"+sid] = &trace.Session{Client: "claude-code", ID: sid, Calls: []trace.Call{
			{Tool: "mcp:create", MCPServer: "gw", MCPTool: "create", Args: map[string]string{"summary": fmt.Sprintf("s%d", i)}, Output: `{"key":"` + key + `"}`, Outcome: trace.OutcomeOK},
			{Tool: "mcp:link", MCPServer: "gw", MCPTool: "dispatch", Args: map[string]string{"integration": "jira", "action": "link", "params": string(params)}, Output: `{}`, Outcome: trace.OutcomeOK},
		}}
		p.Executions = append(p.Executions, primitive.Execution{ID: fmt.Sprintf("ex%d", i), Client: "claude-code", Session: sid,
			Calls: []primitive.CallRef{{Step: 1, Index: 0}, {Step: 2, Index: 1}}})
	}
	g, why := directGraph(p, by)
	if g == nil {
		t.Fatal(why)
	}
	if g.Executions != 2 {
		t.Fatalf("the dissenting run was not left out: %d executions", g.Executions)
	}
	args := map[string]codegen.ProgramValue{}
	for _, a := range g.Steps[1].Args {
		args[strings.Join(a.Path, "/")] = a.Value
	}
	if v := args["params/inward"]; v.Kind != "result" || v.Step != 1 {
		t.Fatalf("inward is not the created issue: %+v", v)
	}
	for _, path := range []string{"integration", "action"} {
		if v := args[path]; v.Kind != "selector" {
			t.Fatalf("%s chooses the operation and must stay fixed: %+v", path, v)
		}
	}
	defaults := map[string]string{}
	for _, in := range g.Inputs {
		defaults[in.Name] = in.Default
	}
	if v := args["params/type"]; v.Kind != "input" || defaults[v.Input] != "Blocks" {
		t.Fatalf("a recorded constant is not a defaulted input: %+v %v", v, defaults)
	}
	if _, err := codegen.GenerateProgramPackage(g); err != nil {
		t.Fatal(err)
	}
}
