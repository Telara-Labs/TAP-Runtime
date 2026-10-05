package codegen_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/codegen"
)

func TestGenerateProgramPackageExecutesWithNewInputs(t *testing.T) {
	g := &codegen.ProgramGraph{
		CandidateID: "lc_example",
		Sources:     []string{"span-a", "span-b"},
		Inputs: []codegen.ProgramInput{
			{Name: "summary", Type: "string", Source: "caller"},
			{Name: "related_ids", Type: "string", List: true, Source: "caller"},
		},
		Steps: []codegen.ProgramStep{
			{Role: "jira.create_issue", Tool: "mcp:telara_execute_action", Binding: &codegen.ProgramToolBinding{Server: "telara", Tool: "telara_execute_action"}, Effect: "write", Args: []codegen.ProgramArg{
				{Path: []string{"integration"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "jira"}},
				{Path: []string{"action"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "create_issue"}},
				{Path: []string{"params", "summary"}, JSONString: true, Value: codegen.ProgramValue{Kind: "input", Input: "summary"}},
			}},
			{Role: "jira.create_issue_link", Tool: "mcp:telara_execute_action", Binding: &codegen.ProgramToolBinding{Server: "telara", Tool: "telara_execute_action"}, Effect: "write", Loop: "related_ids", Args: []codegen.ProgramArg{
				{Path: []string{"integration"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "jira"}},
				{Path: []string{"action"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "create_issue_link"}},
				{Path: []string{"params", "inward_issue_key"}, JSONString: true, Value: codegen.ProgramValue{Kind: "result", Step: 1, ResultPath: ".key"}},
				{Path: []string{"params", "outward_issue_key"}, JSONString: true, Value: codegen.ProgramValue{Kind: "item", Input: "related_ids"}},
			}},
		},
	}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	if problems := p.Manifest.RunProblems(); len(problems) != 0 {
		t.Fatalf("generated manifest invalid: %v", problems)
	}
	if strings.Contains(string(p.Files["main.py"]), "TENG-") {
		t.Fatal("recorded issue ID leaked into generated source")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	harness := strings.Join([]string{
		"import json, sys",
		"calls = []",
		"class Tap:",
		"    def call(self, alias, args):",
		"        calls.append([alias, args])",
		"        return {'key': 'TENG-100'} if alias == 'step_1' else {'linked': True}",
		"sys.argv = ['main.py', json.dumps({'summary': 'new task', 'related_ids': ['TENG-200', 'TENG-300']})]",
		"exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
		"print('CALLS=' + json.dumps(calls, sort_keys=True))",
	}, "\n")
	cmd := exec.Command(python, "-c", harness)
	cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated program failed: %v\n%s", err, out)
	}
	idx := strings.Index(string(out), "CALLS=")
	if idx < 0 {
		t.Fatalf("missing call log: %s", out)
	}
	var calls [][]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[idx+6:]))), &calls); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("want create then two links, got %d: %s", len(calls), out)
	}
	for i, want := range []string{"TENG-200", "TENG-300"} {
		var args map[string]string
		if err := json.Unmarshal(calls[i+1][1], &args); err != nil {
			t.Fatal(err)
		}
		var params map[string]string
		if err := json.Unmarshal([]byte(args["params"]), &params); err != nil {
			t.Fatal(err)
		}
		if params["inward_issue_key"] != "TENG-100" || params["outward_issue_key"] != want {
			t.Fatalf("link %d used wrong result/input binding: %+v", i, params)
		}
	}
}

func TestGenerateProgramPackageRejectsUnknownSource(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_unresolved", Problems: []string{"missing source"}}
	if _, err := codegen.GenerateProgramPackage(g); err == nil {
		t.Fatal("underdetermined graph must not produce an installable package")
	}
}

func TestProgramResultPath(t *testing.T) {
	got, err := codegen.ProgramResultPath(".items[0].key")
	if err != nil || got != "[\"items\", 0, \"key\"]" {
		t.Fatalf("path = %q, %v", got, err)
	}
}

func TestGenerateProgramPackageOmitsOptionalNestedArgument(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_optional",
		Inputs: []codegen.ProgramInput{
			{Name: "name", Type: "string", Source: "supplied at invocation"},
			{Name: "priority", Type: "string", Optional: true, Source: "supplied at invocation"},
			{Name: "labels", Type: "array", Optional: true, Source: "supplied at invocation"},
		},
		Steps: []codegen.ProgramStep{{Role: "create", Tool: "mcp:execute_action",
			Binding: &codegen.ProgramToolBinding{Server: "example", Tool: "execute_action"}, Effect: "write",
			OptionalProfiles: [][]string{{}, {"priority"}, {"labels"}},
			Args: []codegen.ProgramArg{
				{Path: []string{"action"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "create"}},
				{Path: []string{"params", "name"}, JSONString: true, Value: codegen.ProgramValue{Kind: "input", Input: "name"}},
				{Path: []string{"params", "priority"}, JSONString: true, Optional: true, Value: codegen.ProgramValue{Kind: "input", Input: "priority"}},
				{Path: []string{"params", "labels"}, JSONString: true, Optional: true, Value: codegen.ProgramValue{Kind: "input", Input: "labels"}},
			}}},
	}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	harness := strings.Join([]string{
		"import json, sys",
		"calls = []",
		"class Tap:",
		"    def call(self, alias, args):",
		"        calls.append(args)",
		"        return {'ok': True}",
		"sys.argv = ['main.py', sys.argv[1]]",
		"exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
		"print('CALLS=' + json.dumps(calls, sort_keys=True))",
	}, "\n")
	for _, tc := range []struct {
		input        string
		wantPriority bool
	}{
		{`{"name":"fresh"}`, false},
		{`{"name":"fresh","priority":"high"}`, true},
	} {
		cmd := exec.Command(python, "-c", harness, tc.input)
		cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated optional program failed: %v\n%s", err, out)
		}
		idx := strings.Index(string(out), "CALLS=")
		if idx < 0 {
			t.Fatalf("missing call log: %s", out)
		}
		var calls []map[string]string
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[idx+6:]))), &calls); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 {
			t.Fatalf("calls: %+v", calls)
		}
		var params map[string]string
		if err := json.Unmarshal([]byte(calls[0]["params"]), &params); err != nil {
			t.Fatal(err)
		}
		_, hasPriority := params["priority"]
		if params["name"] != "fresh" || hasPriority != tc.wantPriority {
			t.Fatalf("optional arg was not conditionally omitted: %+v", params)
		}
	}
	invalid := exec.Command(python, "-c", harness, `{"name":"fresh","priority":"high","labels":["one"]}`)
	invalid.Stdin = strings.NewReader(string(p.Files["main.py"]))
	output, err := invalid.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "unobserved optional input combination") {
		t.Fatalf("unobserved combination must stop before the call: %v\n%s", err, output)
	}
}

func TestGenerateProgramPackageRunsResultListLoop(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_resultitems", Steps: []codegen.ProgramStep{
		{Role: "list", Tool: "mcp:list", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "list"}, Effect: "read"},
		{Role: "act", Tool: "mcp:act", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "act"}, Effect: "write",
			LoopResultStep: 1, LoopResultPath: `["items"]`, Args: []codegen.ProgramArg{{Path: []string{"id"}, Value: codegen.ProgramValue{Kind: "item_result", Step: 1, ResultPath: `["id"]`}}}},
	}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	harness := strings.Join([]string{
		"import json, sys",
		"calls = []",
		"class Tap:",
		"    def call(self, alias, args):",
		"        calls.append([alias, args])",
		"        return {'items': [{'id': 'NEW-1'}, {'id': 'NEW-2'}]} if alias == 'step_1' else {'ok': True}",
		"sys.argv = ['main.py', '{}']",
		"exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
		"print('CALLS=' + json.dumps(calls, sort_keys=True))",
	}, "\n")
	cmd := exec.Command(python, "-c", harness)
	cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated result loop failed: %v\n%s", err, out)
	}
	idx := strings.Index(string(out), "CALLS=")
	if idx < 0 {
		t.Fatalf("no calls: %s", out)
	}
	var raw [][]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[idx+6:]))), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 {
		t.Fatalf("want list then two actions: %s", out)
	}
	for i, want := range []string{"NEW-1", "NEW-2"} {
		var args map[string]string
		if err := json.Unmarshal(raw[i+1][1], &args); err != nil {
			t.Fatal(err)
		}
		if args["id"] != want {
			t.Fatalf("wrong result item %d: %+v", i, args)
		}
	}
}

func TestGenerateProgramPackageSelectsFreshResultSubsetBeforeEffects(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_resultsubset", Inputs: []codegen.ProgramInput{
		{Name: "positions", Type: "integer", List: true, Source: "caller selects result positions"},
	}, Steps: []codegen.ProgramStep{
		{Role: "list", Tool: "mcp:list", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "list"}, Effect: "read"},
		{Role: "act", Tool: "mcp:act", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "act"}, Effect: "write", Loop: "positions", DistinctLoopSelections: true,
			Args: []codegen.ProgramArg{{Path: []string{"id"}, Value: codegen.ProgramValue{Kind: "collection_index_item", Step: 1, CollectionPath: ".items", ResultPath: ".id", Input: "positions"}}}},
	}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.Files["README.md"]), "Selected positions in `positions` must be distinct") || len(p.Manifest.Tools) != 2 {
		t.Fatalf("review package did not disclose selector/effects: %s", p.Files["README.md"])
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	harness := strings.Join([]string{
		"import json, sys",
		"calls = []",
		"class Tap:",
		"    def call(self, alias, args):",
		"        calls.append([alias, args])",
		"        return {'items': [{'id': 'FRESH-A'}, {'id': 'FRESH-B'}, {'id': 'FRESH-C'}]} if alias == 'step_1' else {'ok': True}",
		"sys.argv = ['main.py', sys.argv[1]]",
		"try:",
		"    exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
		"except Exception as exc:",
		"    print('ERROR=' + str(exc))",
		"print('CALLS=' + json.dumps(calls))",
	}, "\n")
	run := func(input string) (string, [][]json.RawMessage) {
		t.Helper()
		cmd := exec.Command(python, "-c", harness, input)
		cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated program failed outside its expected input errors: %v\n%s", err, output)
		}
		idx := strings.Index(string(output), "CALLS=")
		if idx < 0 {
			t.Fatalf("missing call log: %s", output)
		}
		var calls [][]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(output[idx+6:]))), &calls); err != nil {
			t.Fatal(err)
		}
		return string(output), calls
	}
	output, calls := run(`{"positions":[2,0]}`)
	if strings.Contains(output, "ERROR=") || len(calls) != 3 {
		t.Fatalf("fresh subset did not run: %s", output)
	}
	for i, want := range []string{"FRESH-C", "FRESH-A"} {
		var args map[string]string
		if err := json.Unmarshal(calls[i+1][1], &args); err != nil || args["id"] != want {
			t.Fatalf("selection %d got %+v: %v", i, args, err)
		}
	}
	for _, input := range []string{`{"positions":[0,3]}`, `{"positions":[1,1]}`} {
		output, calls := run(input)
		if !strings.Contains(output, "ERROR=") || len(calls) != 1 {
			t.Fatalf("invalid selection made downstream effects: %s", output)
		}
	}
	output, calls = run(`{"positions":["0"]}`)
	if !strings.Contains(output, "ERROR=") || len(calls) != 0 {
		t.Fatalf("mistyped selection passed input preflight: %s", output)
	}
	brokenHarness := strings.Replace(harness, "{'id': 'FRESH-C'}", "{'missing': 'FRESH-C'}", 1)
	broken := exec.Command(python, "-c", brokenHarness, `{"positions":[0,2]}`)
	broken.Stdin = strings.NewReader(string(p.Files["main.py"]))
	brokenOutput, err := broken.CombinedOutput()
	if err != nil || !strings.Contains(string(brokenOutput), "ERROR=") || strings.Contains(string(brokenOutput), `"step_2"`) {
		t.Fatalf("missing field in later selected item caused partial effects: %v\n%s", err, brokenOutput)
	}
	scalar := &codegen.ProgramGraph{CandidateID: "lc_resultsingle", Inputs: []codegen.ProgramInput{{Name: "position", Type: "integer", Source: "caller"}}, Steps: []codegen.ProgramStep{
		{Role: "list", Tool: "mcp:list", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "list"}, Effect: "read"},
		{Role: "get", Tool: "mcp:act", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "act"}, Effect: "read",
			Args: []codegen.ProgramArg{{Path: []string{"id"}, Value: codegen.ProgramValue{Kind: "collection_index", Step: 1, CollectionPath: ".items", ResultPath: ".id", Input: "position"}}}},
	}}
	singlePackage, err := codegen.GenerateProgramPackage(scalar)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", harness, `{"position":1}`)
	cmd.Stdin = strings.NewReader(string(singlePackage.Files["main.py"]))
	outputBytes, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(outputBytes), `"id": "FRESH-B"`) {
		t.Fatalf("scalar selection did not use fresh result item: %v\n%s", err, outputBytes)
	}
}

func TestGenerateProgramPackageRunsRepeatedProducerJoin(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_paralleljoin", Inputs: []codegen.ProgramInput{{Name: "sources", Type: "object", List: true, Source: "caller", Fields: []codegen.ProgramInputField{{Name: "source_id", Path: []string{"source_id"}, Type: "string"}, {Name: "name", Path: []string{"name"}, Type: "string"}}}}, Steps: []codegen.ProgramStep{
		{Role: "create", Tool: "mcp:create", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "create"}, Effect: "write", Loop: "sources",
			Args: []codegen.ProgramArg{{Path: []string{"source_id"}, Value: codegen.ProgramValue{Kind: "item", Input: "sources", ResultPath: ".source_id"}}, {Path: []string{"name"}, Value: codegen.ProgramValue{Kind: "item", Input: "sources", ResultPath: ".name"}}}},
		{Role: "update", Tool: "mcp:update", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "update"}, Effect: "write", LoopResultStep: 1,
			Args: []codegen.ProgramArg{{Path: []string{"record_id"}, Value: codegen.ProgramValue{Kind: "item_result", Step: 1, ResultPath: ".id"}}}},
	}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	harness := strings.Join([]string{
		"import json, sys",
		"calls = []",
		"class Tap:",
		"    def call(self, alias, args):",
		"        calls.append([alias, args])",
		"        return {'id': 'new-' + args['source_id']} if alias == 'step_1' else {'updated': True}",
		"sys.argv = ['main.py', sys.argv[1]]",
		"exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
		"print('CALLS=' + json.dumps(calls))",
	}, "\n")
	cmd := exec.Command(python, "-c", harness, `{"sources":[{"source_id":"A","name":"alpha"},{"source_id":"B","name":"beta"}]}`)
	cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated producer join failed: %v\n%s", err, out)
	}
	idx := strings.Index(string(out), "CALLS=")
	if idx < 0 {
		t.Fatalf("missing call log: %s", out)
	}
	var calls [][]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[idx+6:]))), &calls); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("want two creates then two updates: %s", out)
	}
	for i, want := range []string{"alpha", "beta"} {
		var args map[string]string
		if err := json.Unmarshal(calls[i][1], &args); err != nil {
			t.Fatal(err)
		}
		if args["name"] != want {
			t.Fatalf("producer %d item fields not aligned: %+v", i, args)
		}
	}
	for i, want := range []string{"new-A", "new-B"} {
		var args map[string]string
		if err := json.Unmarshal(calls[i+2][1], &args); err != nil {
			t.Fatal(err)
		}
		if args["record_id"] != want {
			t.Fatalf("consumer %d used wrong producer: %+v", i, args)
		}
	}
	invalid := exec.Command(python, "-c", harness, `{"sources":[{"source_id":"A"}]}`)
	invalid.Stdin = strings.NewReader(string(p.Files["main.py"]))
	invalidOutput, err := invalid.CombinedOutput()
	if err == nil || !strings.Contains(string(invalidOutput), "missing or mistyped field") {
		t.Fatalf("missing record field must stop before calls: %v\n%s", err, invalidOutput)
	}
}

func TestGenerateProgramPackageSelectsOneResultItem(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_selectone", Inputs: []codegen.ProgramInput{{Name: "wanted_status", Type: "string", Source: "caller"}}, Steps: []codegen.ProgramStep{
		{Role: "list", Tool: "mcp:list", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "list"}, Effect: "read"},
		{Role: "get", Tool: "mcp:get", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "get"}, Effect: "read", Args: []codegen.ProgramArg{
			{Path: []string{"id"}, Value: codegen.ProgramValue{Kind: "selected_result", Step: 1, CollectionPath: ".items", PredicatePath: ".status", ResultPath: ".id", Input: "wanted_status"}},
		}},
	}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	harness := strings.Join([]string{
		"import json, sys",
		"calls = []",
		"class Tap:",
		"    def call(self, alias, args):",
		"        calls.append([alias, args])",
		"        return {'items': [{'id': 'N-1', 'status': 'ok'}, {'id': 'N-2', 'status': 'failed'}]} if alias == 'step_1' else {'found': True}",
		"sys.argv = ['main.py', sys.argv[1]]",
		"exec(compile(sys.stdin.read(), 'main.py', 'exec'), {'tap': Tap()})",
		"print('CALLS=' + json.dumps(calls))",
	}, "\n")
	cmd := exec.Command(python, "-c", harness, `{"wanted_status":"failed"}`)
	cmd.Stdin = strings.NewReader(string(p.Files["main.py"]))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("selected result failed: %v\n%s", err, out)
	}
	idx := strings.Index(string(out), "CALLS=")
	if idx < 0 {
		t.Fatalf("missing call log: %s", out)
	}
	var calls [][]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out[idx+6:]))), &calls); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("want list then get: %s", out)
	}
	var args map[string]string
	if err := json.Unmarshal(calls[1][1], &args); err != nil {
		t.Fatal(err)
	}
	if args["id"] != "N-2" {
		t.Fatalf("selected wrong item: %+v", args)
	}
	missing := exec.Command(python, "-c", harness, `{"wanted_status":"absent"}`)
	missing.Stdin = strings.NewReader(string(p.Files["main.py"]))
	missOutput, err := missing.CombinedOutput()
	if err == nil || !strings.Contains(string(missOutput), "selection requires exactly one matching item") {
		t.Fatalf("zero matches must fail: %v\n%s", err, missOutput)
	}
	duplicateHarness := strings.Replace(harness, "{'id': 'N-1', 'status': 'ok'}", "{'id': 'N-1', 'status': 'failed'}", 1)
	duplicate := exec.Command(python, "-c", duplicateHarness, `{"wanted_status":"failed"}`)
	duplicate.Stdin = strings.NewReader(string(p.Files["main.py"]))
	duplicateOutput, err := duplicate.CombinedOutput()
	if err == nil || !strings.Contains(string(duplicateOutput), "selection requires exactly one matching item") {
		t.Fatalf("multiple matches must fail: %v\n%s", err, duplicateOutput)
	}
}

func TestGenerateProgramPackageRejectsScalarResultFromLoop(t *testing.T) {
	g := &codegen.ProgramGraph{CandidateID: "lc_badloop", Inputs: []codegen.ProgramInput{{Name: "items", Type: "string", List: true, Source: "caller"}}, Steps: []codegen.ProgramStep{
		{Role: "create", Tool: "mcp:create", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "create"}, Effect: "write", Loop: "items", Args: []codegen.ProgramArg{
			{Path: []string{"name"}, Value: codegen.ProgramValue{Kind: "item", Input: "items"}},
		}},
		{Role: "link", Tool: "mcp:link", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "link"}, Effect: "write", Args: []codegen.ProgramArg{
			{Path: []string{"id"}, Value: codegen.ProgramValue{Kind: "result", Step: 1, ResultPath: ".id"}},
		}},
	}}
	if _, err := codegen.GenerateProgramPackage(g); err == nil || !strings.Contains(err.Error(), "treats looped step 1 as a scalar result") {
		t.Fatalf("invalid loop-to-scalar binding compiled: %v", err)
	}
}
