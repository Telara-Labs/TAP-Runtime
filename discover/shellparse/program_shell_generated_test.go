package shellparse_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/internal/testkit"

	"github.com/Telara-Labs/TAP-Runtime/discover/genreview"

	"github.com/Telara-Labs/TAP-Runtime/discover/codegen"

	"github.com/Telara-Labs/TAP-Runtime/discover/retrieval"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestGeneratedPipelinePassesStdoutAsStdinAndStopsOnFailure(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_pipe", Inputs: []codegen.ProgramInput{
		{Name: "path", Type: "string", Source: "supplied at invocation"},
		{Name: "pattern", Type: "string", Source: "supplied at invocation"},
	}, Steps: []codegen.ProgramStep{{Role: "sh:cat+sh:grep", Tool: "shell", Effect: "read", Pipeline: []codegen.ProgramCommand{{Name: "cat", Effect: "read"}, {Name: "grep", Effect: "read", Connector: "pipe"}}, Args: []codegen.ProgramArg{
		{Path: []string{"pipe_0_argv_0"}, Value: codegen.ProgramValue{Kind: "input", Input: "path"}},
		{Path: []string{"pipe_1_argv_0"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "-n"}},
		{Path: []string{"pipe_1_argv_1"}, Value: codegen.ProgramValue{Kind: "input", Input: "pattern"}},
	}}}}
	pkg, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Manifest.Commands) != 2 || pkg.Manifest.Commands[0].Command != "cat" || pkg.Manifest.Commands[1].Command != "grep" || pkg.Manifest.Commands[1].Args[0] != "-n" {
		t.Fatalf("pipeline reach not declared: %+v", pkg.Manifest.Commands)
	}
	if problems := pkg.Manifest.RunProblems(); len(problems) != 0 {
		t.Fatalf("invalid pipeline manifest: %v", problems)
	}
	var review strings.Builder
	if err := genreview.ReviewGenerated(strings.NewReader("q\n"), &review, g, t.TempDir(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(review.String(), "pipeline cat (read) | grep (read)") {
		t.Fatalf("review hid pipeline command reach: %s", review.String())
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, fail := range []bool{false, true} {
		harness := strings.Join([]string{
			"import json, sys",
			"calls = []",
			"class Tap:",
			"    def exec(self, command, args, stdin=''):",
			"        calls.append([command, args, stdin])",
			"        return {'exit': 1 if " + map[bool]string{false: "False", true: "True"}[fail] + " and len(calls) == 1 else 0, 'stdout': 'fresh content'}",
			"sys.argv = ['main.py', json.dumps({'path': 'new.txt', 'pattern': 'fresh'})]",
			"try:",
			"    exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
			"except RuntimeError: pass",
			"print('CALLS=' + json.dumps(calls))",
		}, "\n")
		cmd := exec.Command(python, "-c", harness)
		cmd.Stdin = strings.NewReader(string(pkg.Files["main.py"]))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pipeline program failed: %v\n%s", err, out)
		}
		index := strings.LastIndex(string(out), "CALLS=")
		var calls [][]any
		if index < 0 || json.Unmarshal([]byte(strings.TrimSpace(string(out[index+6:]))), &calls) != nil {
			t.Fatalf("missing pipeline calls: %s", out)
		}
		want := 2
		if fail {
			want = 1
		}
		if len(calls) != want {
			t.Fatalf("fail=%v calls=%v", fail, calls)
		}
		if !fail && calls[1][2] != "fresh content" {
			t.Fatalf("stdout was not piped: %v", calls)
		}
	}
}

func TestSynthesizePipelineFromIndependentExecutions(t *testing.T) {
	ss := []trace.Session{
		testkit.NewSession("pipe-one", "Filter one log", trace.Call{Tool: "shell", Command: "cat logs/one.txt | grep ERROR", Outcome: trace.OutcomeOK}),
		testkit.NewSession("pipe-two", "Filter another log", trace.Call{Tool: "shell", Command: "cat logs/two.txt | grep WARN", Outcome: trace.OutcomeOK}),
	}
	var spans []model.SpanProposal
	for i, s := range ss {
		spans = append(spans, model.SpanProposal{ID: "pipe-span-" + string(rune('a'+i)), Client: s.Client, Session: s.ID,
			Request: 0, Calls: []int{1}, CallHashes: []string{retrieval.SpanCallHash(s.Calls[0])}})
	}
	c := model.LogicCandidate{ID: "lc_abc123", Executions: 2, Sessions: 2, Members: []string{spans[0].ID, spans[1].ID}}
	g, err := codegen.SynthesizeProgramGraph(c, spans, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("pipeline graph unresolved: %+v %v", g, err)
	}
	if len(g.Steps) != 1 || len(g.Steps[0].Pipeline) != 2 || len(g.Inputs) != 2 {
		t.Fatalf("pipeline structure or inputs lost: %+v", g)
	}
	if _, err := codegen.GenerateProgramPackage(g); err != nil {
		t.Fatalf("pipeline graph did not compile: %v", err)
	}
}

func TestSynthesizeSuccessChainAndKeepConnectorsDistinct(t *testing.T) {
	ss := []trace.Session{
		testkit.NewSession("chain-one", "Stage one file", trace.Call{Tool: "shell", Command: "git add src/one.go && git status --short", Outcome: trace.OutcomeOK}),
		testkit.NewSession("chain-two", "Stage another file", trace.Call{Tool: "shell", Command: "git add src/two.go && git status --short", Outcome: trace.OutcomeOK}),
	}
	var spans []model.SpanProposal
	for i, s := range ss {
		spans = append(spans, model.SpanProposal{ID: "chain-span-" + string(rune('a'+i)), Client: s.Client, Session: s.ID,
			Request: 0, Calls: []int{1}, CallHashes: []string{retrieval.SpanCallHash(s.Calls[0])}})
	}
	c := model.LogicCandidate{ID: "lc_chain", Executions: 2, Sessions: 2, Members: []string{spans[0].ID, spans[1].ID}}
	g, err := codegen.SynthesizeProgramGraph(c, spans, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("success chain graph unresolved: %+v %v", g, err)
	}
	if len(g.Steps) != 1 || len(g.Steps[0].Pipeline) != 2 || g.Steps[0].Pipeline[1].Connector != "and" {
		t.Fatalf("success chain structure lost: %+v", g)
	}
	pkg, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Manifest.Commands) != 2 || !strings.Contains(string(pkg.Files["README.md"]), "success chain") {
		t.Fatalf("success chain reach not exposed: %+v", pkg.Manifest.Commands)
	}
	if codegen.ProgramCallToolIdentity(trace.Call{Tool: "shell", Command: "cat a | grep b"}) == codegen.ProgramCallToolIdentity(trace.Call{Tool: "shell", Command: "cat a && grep b"}) {
		t.Fatal("pipe and success chain share an identity")
	}
}

func TestGeneratedSuccessChainDoesNotPipeAndStopsOnFailure(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_chain", Steps: []codegen.ProgramStep{{Tool: "shell", Effect: "read",
		Pipeline: []codegen.ProgramCommand{{Name: "printf", Effect: "read"}, {Name: "wc", Effect: "read", Connector: "and"}},
		Args: []codegen.ProgramArg{{Path: []string{"pipe_0_argv_0"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "hello"}},
			{Path: []string{"pipe_1_argv_0"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "-l"}}}}}}
	pkg, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, fail := range []bool{false, true} {
		harness := strings.Join([]string{
			"import json, sys", "calls = []", "class Tap:",
			"    def exec(self, *args):", "        calls.append(args)",
			"        return {'exit': 1 if " + map[bool]string{false: "False", true: "True"}[fail] + " and len(calls) == 1 else 0, 'stdout': 'do not forward'}",
			"sys.argv = ['main.py', '{}']", "try:",
			"    exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
			"except RuntimeError: pass", "print('CALLS=' + json.dumps(calls))",
		}, "\n")
		cmd := exec.Command(python, "-c", harness)
		cmd.Stdin = strings.NewReader(string(pkg.Files["main.py"]))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("success chain failed: %v\n%s", err, out)
		}
		index := strings.LastIndex(string(out), "CALLS=")
		var calls [][]any
		if index < 0 || json.Unmarshal([]byte(strings.TrimSpace(string(out[index+6:]))), &calls) != nil {
			t.Fatalf("missing success chain calls: %s", out)
		}
		want := 2
		if fail {
			want = 1
		}
		if len(calls) != want || !fail && len(calls[1]) != 2 {
			t.Fatalf("fail=%v calls=%v", fail, calls)
		}
	}
}

func TestGeneratedPipelineRejectsUnboundArgumentAndLoop(t *testing.T) {
	base := codegen.ProgramGraph{CandidateID: "lc_def456", Steps: []codegen.ProgramStep{{Tool: "shell", Effect: "read", Pipeline: []codegen.ProgramCommand{{Name: "cat", Effect: "read"}, {Name: "grep", Effect: "read", Connector: "pipe"}},
		Args: []codegen.ProgramArg{{Path: []string{"pipe_2_argv_0"}, Value: codegen.ProgramValue{Kind: "input", Input: "x"}}}}}}
	if _, err := codegen.GenerateProgramPackage(&base); err == nil {
		t.Fatal("out-of-range pipeline argument was silently dropped")
	}
	base.Steps[0].Args = nil
	base.Steps[0].Loop = "items"
	if _, err := codegen.GenerateProgramPackage(&base); err == nil {
		t.Fatal("pipeline loop was accepted without generated loop control")
	}
}

func TestShellVariantIdentityIncludesExecutable(t *testing.T) {
	git := trace.Call{Tool: "shell", Command: "git add src/a.go"}
	gh := trace.Call{Tool: "shell", Command: "gh add src/a.go"}
	if codegen.ProgramCallSignature(git, nil) == codegen.ProgramCallSignature(gh, nil) || codegen.ProgramCallCoreSignature(git, nil) == codegen.ProgramCallCoreSignature(gh, nil) {
		t.Fatal("different host commands must not share a program variant")
	}
}

func TestGeneratedCommandProgramUsesDeclaredArgvAndStopsOnFailure(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_command", Inputs: []codegen.ProgramInput{{Name: "path", Type: "string", Source: "supplied at invocation"}},
		Steps: []codegen.ProgramStep{
			{Role: "sh:git status", Tool: "shell", Command: "git", Effect: "read", Args: []codegen.ProgramArg{
				{Path: []string{"argv_0"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "status"}},
				{Path: []string{"argv_1"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "--short"}},
			}},
			{Role: "sh:git add", Tool: "shell", Command: "git", Effect: "write", Args: []codegen.ProgramArg{
				{Path: []string{"argv_0"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "add"}},
				{Path: []string{"argv_1"}, Value: codegen.ProgramValue{Kind: "input", Input: "path"}},
			}},
		},
	}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.Manifest.Commands); got != 2 {
		t.Fatalf("want two declared commands, got %d", got)
	}
	if p.Manifest.Commands[0].Args[0] != "status" || p.Manifest.Commands[1].Args[0] != "add" || p.Manifest.Commands[1].Args[1] != "*" {
		t.Fatalf("wrong command reach: %+v", p.Manifest.Commands)
	}
	if problems := p.Manifest.RunProblems(); len(problems) != 0 {
		t.Fatalf("invalid manifest: %v", problems)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, fail := range []bool{false, true} {
		harness := strings.Join([]string{
			"import json, sys",
			"calls = []",
			"class Tap:",
			"    def exec(self, command, args):",
			"        calls.append([command, args])",
			"        return {'exit': 1 if " + map[bool]string{false: "False", true: "True"}[fail] + " and len(calls) == 1 else 0}",
			"sys.argv = ['main.py', json.dumps({'path': 'a new file.txt'})]",
			"try:",
			"    exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
			"except RuntimeError: pass",
			"print('CALLS=' + json.dumps(calls))",
		}, "\n")
		cmd := exec.Command(python, "-c", harness)
		cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated program failed: %v\n%s", err, out)
		}
		index := strings.LastIndex(string(out), "CALLS=")
		if index < 0 {
			t.Fatalf("no call log: %s", out)
		}
		var calls [][]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[index+6:]))), &calls); err != nil {
			t.Fatal(err)
		}
		want := 2
		if fail {
			want = 1
		}
		if len(calls) != want {
			t.Fatalf("fail=%v: call order %v", fail, calls)
		}
		if !fail && calls[1][0] != "git" {
			t.Fatalf("wrong command: %v", calls)
		}
	}
}

func TestGeneratedCommandRejectsUndeclaredShape(t *testing.T) {
	for _, st := range []codegen.ProgramStep{
		{Tool: "shell", Command: "bash", Effect: "read"},
		{Tool: "shell", Command: "git", Effect: "unknown"},
		{Tool: "shell", Command: "git", Effect: "read", Args: []codegen.ProgramArg{{Path: []string{"argv_1"}, Value: codegen.ProgramValue{Kind: "input", Input: "x"}}}},
	} {
		if _, err := codegen.GenerateProgramPackage(&codegen.ProgramGraph{CandidateID: "lc_bad", Steps: []codegen.ProgramStep{st}}); err == nil {
			t.Fatalf("accepted unresolved command: %+v", st)
		}
	}
}

func TestSynthesizeLiteralCommandChain(t *testing.T) {
	ss := []trace.Session{
		testkit.NewSession("shell-one", "Run git add on the changed file, then git status",
			trace.Call{Tool: "shell", Command: "git add src/one.go", Outcome: trace.OutcomeOK},
			trace.Call{Tool: "shell", Command: "git status --short", Outcome: trace.OutcomeOK}),
		testkit.NewSession("shell-two", "Run git add on the changed file, then git status",
			trace.Call{Tool: "shell", Command: "git add src/two.go", Outcome: trace.OutcomeOK},
			trace.Call{Tool: "shell", Command: "git status --short", Outcome: trace.OutcomeOK}),
	}
	c, ps := testkit.GraphCandidateFor(t, ss, "sh:git add", "sh:git status")
	g, err := codegen.SynthesizeProgramGraph(c, ps, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("command graph unresolved: %+v %v", g, err)
	}
	if g.Steps[0].Command != "git" || len(g.Inputs) != 1 || g.Inputs[0].Name != "step_1_argv_1" {
		t.Fatalf("wrong command binding or input: %+v", g)
	}
	if _, err := codegen.GenerateProgramPackage(g); err != nil {
		t.Fatalf("determined command graph did not compile: %v", err)
	}
	var review strings.Builder
	if err := genreview.ReviewGenerated(strings.NewReader("q\n"), &review, g, t.TempDir(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(review.String(), "via command git (write)") || !strings.Contains(review.String(), "commands:") {
		t.Fatalf("review omitted command reach or effect: %s", review.String())
	}
	for _, request := range []string{"Stage and inspect a changed file", "Run git add on the changed file"} {
		copy := append([]trace.Session(nil), ss[:1]...)
		for i := range copy {
			copy[i].Requests = []string{request}
		}
		for _, proposal := range retrieval.SelectSpanProposals(copy) {
			if len(proposal.Calls) > 1 {
				t.Fatalf("unstated command sequence surfaced for %q: %+v", request, proposal)
			}
		}
	}
}

func TestRepeatedCommandOrderSurfacesWithoutPromptWording(t *testing.T) {
	ss := []trace.Session{
		testkit.NewSession("implicit-one", "Prepare the change",
			trace.Call{Tool: "shell", Command: "git add src/one.go", Outcome: trace.OutcomeOK},
			trace.Call{Tool: "shell", Command: "git status src/one.go --short", Outcome: trace.OutcomeOK}),
		testkit.NewSession("implicit-two", "Prepare another change",
			trace.Call{Tool: "shell", Command: "git add src/two.go", Outcome: trace.OutcomeOK},
			trace.Call{Tool: "shell", Command: "git status src/two.go --short", Outcome: trace.OutcomeOK}),
	}
	proposals := retrieval.SelectSpanProposals(ss)
	seen := 0
	for _, p := range proposals {
		if p.Kind == "repeated_order" && len(p.Calls) == 2 {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("want two cross-session repeated order proposals, got %d: %+v", seen, proposals)
	}
	if len(retrieval.GroupLogicCandidates(proposals)) == 0 {
		t.Fatal("repeated order did not reach candidate grouping")
	}
	for i := range ss {
		ss[i].Calls[1].Command = "git status --short"
	}
	for _, p := range retrieval.SelectSpanProposals(ss) {
		if p.Kind == "repeated_order" {
			t.Fatalf("unrelated repeated order became a process: %+v", p)
		}
	}
}
