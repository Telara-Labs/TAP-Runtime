package codegen_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/codegen"
)

func TestCallerChoiceRunsOnlyItsResultLinkedBranch(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_choice",
		Inputs: []codegen.ProgramInput{
			{Name: "name", Type: "string", Source: "caller"},
			{Name: "action", Type: "string", Allowed: []string{"link", "transition"}, Source: "caller"},
			{Name: "link_target", Type: "string", Optional: true, RequiredWhenInput: "action", RequiredWhenValue: "link", Source: "caller"},
			{Name: "transition_to", Type: "string", Optional: true, RequiredWhenInput: "action", RequiredWhenValue: "transition", Source: "caller"},
		},
		Steps: []codegen.ProgramStep{
			{Role: "create", Tool: "mcp:create", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "create"}, Effect: "write", Args: []codegen.ProgramArg{
				{Path: []string{"name"}, Value: codegen.ProgramValue{Kind: "input", Input: "name"}},
			}},
			{Role: "transition", Tool: "mcp:transition", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "transition"}, Effect: "write", WhenInput: "action", WhenValue: "transition", Args: []codegen.ProgramArg{
				{Path: []string{"issue"}, Value: codegen.ProgramValue{Kind: "result", Step: 1, ResultPath: ".key"}},
				{Path: []string{"to"}, Value: codegen.ProgramValue{Kind: "input", Input: "transition_to"}},
			}},
			{Role: "link", Tool: "mcp:link", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "link"}, Effect: "write", WhenInput: "action", WhenValue: "link", Args: []codegen.ProgramArg{
				{Path: []string{"issue"}, Value: codegen.ProgramValue{Kind: "result", Step: 1, ResultPath: ".key"}},
				{Path: []string{"target"}, Value: codegen.ProgramValue{Kind: "input", Input: "link_target"}},
			}},
		},
	}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	if problems := p.Manifest.RunProblems(); len(problems) != 0 {
		t.Fatalf("manifest problems: %v", problems)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, tc := range []struct {
		input string
		want  string
		fail  bool
	}{
		{input: `{"name":"fresh","action":"transition","transition_to":"doing"}`, want: `["step_1", "step_2"]`},
		{input: `{"name":"fresh","action":"link","link_target":"OTHER"}`, want: `["step_1", "step_3"]`},
		{input: `{"name":"fresh","action":"link"}`, want: `[]`, fail: true},
		{input: `{"name":"fresh","action":"delete"}`, want: `[]`, fail: true},
	} {
		harness := strings.Join([]string{
			"import json, sys",
			"calls = []",
			"class Tap:",
			"    def call(self, alias, args):",
			"        calls.append(alias)",
			"        return {'key': 'NEW-1'} if alias == 'step_1' else {'ok': True}",
			"sys.argv = ['main.py', " + strconvQuote(tc.input) + "]",
			"try:",
			"    exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
			"except ValueError:",
			"    print('REJECTED')",
			"print('CALLS=' + json.dumps(calls))",
		}, "\n")
		cmd := exec.Command(python, "-c", harness)
		cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "CALLS="+tc.want) {
			t.Fatalf("branch %s: %v\n%s", tc.input, err, out)
		}
		if strings.Contains(string(out), "REJECTED") != tc.fail {
			t.Fatalf("branch rejection %s: want %t\n%s", tc.input, tc.fail, out)
		}
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
