package discover

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func graphCandidateFor(t *testing.T, ss []trace.Session, actions ...string) (model.LogicCandidate, []model.SpanProposal) {
	t.Helper()
	ps := SelectSpanProposals(ss)
	for _, c := range GroupLogicCandidates(ps) {
		if len(c.Actions) != len(actions) {
			continue
		}
		match := true
		for i := range actions {
			match = match && c.Actions[i] == actions[i]
		}
		if match {
			return c, ps
		}
	}
	t.Fatalf("no logic candidate %v among %+v", actions, GroupLogicCandidates(ps))
	return model.LogicCandidate{}, nil
}

func TestSynthesizeProgramGraphUsesRolesAndResultFlow(t *testing.T) {
	created := func(key string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{
			"integration": "jira", "action": "create_issue", "params": `{"summary":"follow up"}`,
		}, MCPServer: "telara", MCPTool: "telara_execute_action", Output: `{"key":"` + key + `"}`, Outcome: trace.OutcomeOK})
	}
	link := func(root, child string) trace.Call {
		return trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{
			"integration": "jira", "action": "create_issue_link",
			"params": `{"inward_issue_key":"` + root + `","outward_issue_key":"` + child + `"}`,
		}, MCPServer: "telara", MCPTool: "telara_execute_action", Output: `{"linked":true}`, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("one", "Create follow up and link TENG-2", created("TENG-1"), link("TENG-1", "TENG-2")),
		selSession("many", "Create follow up and link TENG-8 and TENG-9", created("TENG-7"), link("TENG-7", "TENG-8"), link("TENG-7", "TENG-9")),
	}
	c, ps := graphCandidateFor(t, ss, "jira.create_issue", "jira.create_issue_link")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("graph should be determined: graph=%+v err=%v", g, err)
	}
	if len(g.Steps) != 2 || g.Steps[1].Loop == "" {
		t.Fatalf("one and many links must form one loop step: %+v", g.Steps)
	}
	var result, item bool
	for _, a := range g.Steps[1].Args {
		switch a.Path[len(a.Path)-1] {
		case "inward_issue_key":
			result = a.Value.Kind == "result" && a.Value.Step == 1 && a.Value.ResultPath == ".key" && a.JSONString
		case "outward_issue_key":
			item = a.Value.Kind == "item" && a.Value.Input == g.Steps[1].Loop && a.JSONString
		}
	}
	if !result || !item {
		t.Fatalf("parent result and caller item bindings were lost: %+v", g.Steps[1].Args)
	}
	if len(g.Inputs) != 2 || !g.Inputs[1].List {
		t.Fatalf("want summary and related-ID list inputs, got %+v", g.Inputs)
	}
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), "TENG-") || strings.Contains(string(b), "follow up") {
		t.Fatalf("concrete session values leaked into program identity: %s", b)
	}
}

func TestSynthesizeTwoCreatedResultsWithVariableRoleOrder(t *testing.T) {
	create := func(name, id string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:records_create", MCPServer: "records", MCPTool: "create",
			Args: map[string]string{"name": name}, Output: `{"id":"` + id + `"}`, Outcome: trace.OutcomeOK})
	}
	link := func(inward, outward string) trace.Call {
		return trace.Call{Tool: "mcp:records_create_link", MCPServer: "records", MCPTool: "create_link",
			Args: map[string]string{"inward_id": inward, "outward_id": outward}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("left-first", "Create Alpha and Beta then link them", create("Alpha", "TENG-101"), create("Beta", "TENG-102"), link("TENG-101", "TENG-102")),
		selSession("right-first", "Create Delta and Gamma then link them", create("Delta", "TENG-202"), create("Gamma", "TENG-201"), link("TENG-201", "TENG-202")),
	}
	c, spans := graphCandidateFor(t, ss, "mcp:records_create", "mcp:records_create_link")
	g, err := SynthesizeProgramGraph(c, spans, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("two-role result graph unresolved: %+v %v", g, err)
	}
	if len(g.Steps) != 2 || g.Steps[0].Loop == "" || len(g.Steps[1].DistinctResultInputs) != 1 {
		t.Fatalf("source roles or distinctness lost: %+v", g.Steps)
	}
	for _, arg := range g.Steps[1].Args {
		if arg.Path[0] == "inward_id" || arg.Path[0] == "outward_id" {
			if arg.Value.Kind != "indexed_result" || arg.Value.Step != 1 || arg.Value.ResultPath != ".id" {
				t.Fatalf("loop result was treated as a scalar: %+v", arg)
			}
		}
	}
	if _, err := GenerateProgramPackage(g); err != nil {
		t.Fatalf("role-selected program did not compile: %v", err)
	}
}

func TestSynthesizeProgramGraphSharesStableInputAcrossLoopSteps(t *testing.T) {
	create := func(project, id string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:records_create", MCPServer: "records", MCPTool: "create",
			Args: map[string]string{"project_id": project, "name": "follow up"}, Output: `{"id":"` + id + `"}`, Outcome: trace.OutcomeOK})
	}
	link := func(project, id, target string) trace.Call {
		return trace.Call{Tool: "mcp:records_create_link", MCPServer: "records", MCPTool: "create_link",
			Args: map[string]string{"project_id": project, "record_id": id, "target_id": target}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("one", "Create and link one", create("project-one", "NEW-1"), link("project-one", "NEW-1", "TENG-1")),
		selSession("many", "Create and link two", create("project-two", "NEW-2"), link("project-two", "NEW-2", "TENG-2"), link("project-two", "NEW-2", "TENG-3")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_create", "mcp:records_create_link")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("stable shared scope should produce a program: %+v %v", g, err)
	}
	projectInputs := map[string]bool{}
	for _, step := range g.Steps {
		for _, arg := range step.Args {
			if len(arg.Path) == 1 && arg.Path[0] == "project_id" {
				projectInputs[arg.Value.Input] = true
			}
		}
	}
	if len(projectInputs) != 1 || !projectInputs["step_1_project_id"] {
		t.Fatalf("same project input was split across loop cardinalities: %+v", g)
	}
	for _, input := range g.Inputs {
		if input.Name == "step_2_project_id" {
			t.Fatalf("duplicate project input in generated interface: %+v", g.Inputs)
		}
	}
}

func TestSynthesizeProgramGraphMakesUnobservedValuesInvocationInputs(t *testing.T) {
	ss := []trace.Session{
		selSession("a", "Do the work", spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Args: map[string]string{"project_id": "123456"}, Output: `{"id":81234567}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Outcome: trace.OutcomeOK}),
		selSession("b", "Do the work", spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Args: map[string]string{"project_id": "789012"}, Output: `{"id":91234567}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "91234567"}, Outcome: trace.OutcomeOK}),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:gitlab_list_pipelines", "mcp:gitlab_list_jobs")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Inputs) == 0 || g.Inputs[0].Source != "supplied at invocation" {
		t.Fatalf("project ID must become an explicit runtime input: %+v", g)
	}
	if len(g.Steps) != 2 || g.Steps[1].Args[0].Value.Kind != "result" {
		t.Fatalf("generic GitLab result dependency was lost: %+v", g.Steps)
	}
}

func TestSynthesizeProgramGraphRejectsStaleSource(t *testing.T) {
	ss := []trace.Session{
		selSession("a", "Get jobs for the latest pipeline",
			spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Output: `{"id":81234567}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Outcome: trace.OutcomeOK}),
		selSession("b", "Get jobs for the latest pipeline",
			spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Output: `{"id":91234567}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "91234567"}, Outcome: trace.OutcomeOK}),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:gitlab_list_pipelines", "mcp:gitlab_list_jobs")
	ss[0].Calls[0].Output = `{"id":12345678}`
	if _, err := SynthesizeProgramGraph(c, ps, ss); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale source must refuse synthesis, got %v", err)
	}
}

func TestSynthesizeProgramGraphKeepsOptionalArgument(t *testing.T) {
	create := func(key string, priority bool) trace.Call {
		args := map[string]string{"summary": "follow up"}
		if priority {
			args["priority"] = "high"
		}
		return spanRefs(trace.Call{Tool: "mcp:records_create", MCPServer: "records", MCPTool: "create",
			Args: args, Output: `{"key":"` + key + `"}`, Outcome: trace.OutcomeOK})
	}
	link := func(root, target string) trace.Call {
		return trace.Call{Tool: "mcp:records_create_link", MCPServer: "records", MCPTool: "create_link",
			Args: map[string]string{"root": root, "target": target}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("plain", "Create follow up and link TENG-2", create("TENG-1", false), link("TENG-1", "TENG-2")),
		selSession("priority", "Create priority follow up and link TENG-4", create("TENG-3", true), link("TENG-3", "TENG-4")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_create", "mcp:records_create_link")
	variants, err := GroupProgramVariants(c, ps, ss)
	if err != nil || len(variants) != 1 || variants[0].Executions != 2 {
		t.Fatalf("optional argument must not split one program: %+v %v", variants, err)
	}
	g, err := SynthesizeProgramGraph(variants[0], ps, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("optional caller argument should be determined: %+v %v", g, err)
	}
	var foundArg, foundInput bool
	for _, arg := range g.Steps[0].Args {
		if len(arg.Path) == 1 && arg.Path[0] == "priority" {
			foundArg = arg.Optional && arg.Value.Kind == "input"
		}
	}
	for _, input := range g.Inputs {
		if input.Name == "step_1_priority" {
			foundInput = input.Optional
		}
	}
	if !foundArg || !foundInput {
		t.Fatalf("argument present in only one execution was dropped: %+v", g)
	}
}

func TestSynthesizeProgramGraphLoopsOverEarlierResultList(t *testing.T) {
	list := func(ids ...string) trace.Call {
		var items []map[string]string
		for _, id := range ids {
			items = append(items, map[string]string{"id": id})
		}
		body, _ := json.Marshal(map[string]any{"items": items})
		return spanRefs(trace.Call{Tool: "mcp:records_list", MCPServer: "records", MCPTool: "list",
			Output: string(body), Outcome: trace.OutcomeOK})
	}
	act := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_create_link", MCPServer: "records", MCPTool: "create_link",
			Args: map[string]string{"record_id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("one-list", "List the records and link each one", list("TENG-1"), act("TENG-1")),
		selSession("two-list", "List the records and link each one", list("TENG-2", "TENG-3"), act("TENG-2"), act("TENG-3")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_list", "mcp:records_create_link")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("result-list loop should be determined: %+v %v", g, err)
	}
	if len(g.Steps) != 2 || g.Steps[1].LoopResultStep != 1 || g.Steps[1].LoopResultPath != ".items" {
		t.Fatalf("earlier result must supply the collection: %+v", g.Steps)
	}
	if len(g.Steps[1].Args) != 1 || g.Steps[1].Args[0].Value.Kind != "item_result" || g.Steps[1].Args[0].Value.ResultPath != ".id" {
		t.Fatalf("item field binding lost: %+v", g.Steps[1].Args)
	}
	if len(g.Inputs) != 0 {
		t.Fatalf("result list should not become a caller input: %+v", g.Inputs)
	}
}

func TestObservedJSONNumbersKeepIntegerInputRoles(t *testing.T) {
	call := trace.Call{Tool: "mcp:example", Args: map[string]string{"params": `{"id":16438397790,"limit":10,"ratio":1.5}`}}
	fields := trace.ObservedArgs(call)
	if fields["params/id"].TypeName != "integer" || fields["params/limit"].TypeName != "integer" || fields["params/ratio"].TypeName != "number" {
		t.Fatalf("numeric argument roles lost JSON integer distinction: %+v", fields)
	}
	collections := trace.ResultCollections(`{"items":[{"id":16438397790,"ratio":1.5},{"id":16438397791,"ratio":2.5}]}`)
	if len(collections) != 1 || collections[0].Fields[".id"].Type != "integer" || collections[0].Fields[".ratio"].Type != "number" {
		t.Fatalf("numeric result roles lost JSON integer distinction: %+v", collections)
	}
}

func TestSynthesizeProgramGraphExposesPartialResultListSelection(t *testing.T) {
	list := func(a, b string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:records_list", MCPServer: "records", MCPTool: "list",
			Output: `{"items":[{"id":"` + a + `"},{"id":"` + b + `"}]}`, Outcome: trace.OutcomeOK})
	}
	act := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_create_link", MCPServer: "records", MCPTool: "create_link",
			Args: map[string]string{"record_id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("partial", "Link selected records", list("TENG-1", "TENG-2"), act("TENG-1")),
		selSession("full", "Link selected records", list("TENG-3", "TENG-4"), act("TENG-3"), act("TENG-4")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_list", "mcp:records_create_link")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Problems) != 0 || len(g.Inputs) != 1 || !g.Inputs[0].List || g.Inputs[0].Type != "integer" ||
		len(g.Steps) != 2 || g.Steps[1].Args[0].Value.Kind != "collection_index_item" || !g.Steps[1].DistinctLoopSelections {
		t.Fatalf("partial list must use caller-selected result positions: %+v", g)
	}
	if _, err := GenerateProgramPackage(g); err != nil {
		t.Fatalf("a fully sourced list with caller-selected positions should compile: %v", err)
	}
}

func TestSynthesizeProgramGraphSelectsLargeListByPositionWithoutCopyingIDs(t *testing.T) {
	list := func(base int64) trace.Call {
		items := make([]map[string]any, 200)
		for i := range items {
			items[i] = map[string]any{"id": base + int64(i), "status": "failed"}
		}
		body, _ := json.Marshal(map[string]any{"items": items})
		call := spanRefs(trace.Call{Tool: "mcp:telara_execute_action", MCPServer: "telara", MCPTool: "telara_execute_action",
			Args:   map[string]string{"integration": "gitlab", "action": "list_jobs", "params": `{"project_id":"example/repo"}`},
			Output: string(body), Outcome: trace.OutcomeOK})
		call.OutCollections = trace.ResultCollections(string(body))
		if len(call.OutCollections) != 1 || call.OutCollections[0].Count != 200 {
			t.Fatalf("large complete result must retain bounded collection proof: %+v", call.OutCollections)
		}
		return call
	}
	get := func(id int64) trace.Call {
		return trace.Call{Tool: "mcp:telara_execute_action", MCPServer: "telara", MCPTool: "telara_execute_action",
			Args:    map[string]string{"integration": "gitlab", "action": "get_job", "params": fmt.Sprintf(`{"project_id":"example/repo","job_id":%d}`, id)},
			Outcome: trace.OutcomeOK}
	}
	const base int64 = 16438397790
	farList := list(base)
	farID := fmt.Sprint(base + 199)
	for _, id := range farList.OutIDs {
		if id == farID {
			t.Fatal("test requires the selected ID to be beyond the bounded OutIDs prefix")
		}
	}
	farCall := get(base + 199)
	farTrace := observedTrace{groups: [][]observedOp{
		{{node: spanNode{call: farList}, fields: trace.ObservedArgs(farList)}},
		{{node: spanNode{call: farCall}, fields: trace.ObservedArgs(farCall)}},
	}}
	if !possiblePriorResult([]observedTrace{farTrace}, 1, "params/job_id") {
		t.Fatal("a selected item beyond the OutIDs prefix lost its result provenance")
	}
	ss := []trace.Session{
		selSession("first", "Inspect two failed jobs", list(base), get(base), get(base+1)),
		selSession("second", "Inspect two failed jobs", list(base+1000), get(base+1000), get(base+1001)),
	}
	fields := trace.ObservedArgs(get(base))
	if got := fields["params/job_id"].Value; got != "16438397790" {
		t.Fatalf("numeric ID lost exact decimal form: %q", got)
	}
	c, ps := graphCandidateFor(t, ss, "gitlab.list_jobs", "gitlab.get_job")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Problems) != 0 || len(g.Steps) != 2 || len(g.Inputs) != 2 ||
		g.Steps[1].Args[2].Value.Kind != "collection_index_item" || !g.Steps[1].DistinctLoopSelections {
		t.Fatalf("selection from 200 must use a typed position list, not literal IDs: %+v", g)
	}
	if _, err := GenerateProgramPackage(g); err != nil {
		t.Fatalf("caller-selected list positions should compile without guessing a rule: %v", err)
	}
}

func TestSynthesizeProgramGraphExposesReorderedResultListSelection(t *testing.T) {
	list := func(a, b string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:records_list", MCPServer: "records", MCPTool: "list",
			Output: `{"items":[{"id":"` + a + `"},{"id":"` + b + `"}]}`, Outcome: trace.OutcomeOK})
	}
	act := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_create_link", MCPServer: "records", MCPTool: "create_link",
			Args: map[string]string{"record_id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("reverse-a", "Link records in reverse", list("TENG-1", "TENG-2"), act("TENG-2"), act("TENG-1")),
		selSession("reverse-b", "Link records in reverse", list("TENG-3", "TENG-4"), act("TENG-4"), act("TENG-3")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_list", "mcp:records_create_link")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Problems) != 0 || len(g.Inputs) != 1 || !g.Inputs[0].List || g.Steps[1].Args[0].Value.Kind != "collection_index_item" {
		t.Fatalf("reordered selection must be a caller-provided position list: %+v", g)
	}
}

func TestSynthesizeProgramGraphUsesCompleteCollectionBeyondPreview(t *testing.T) {
	list := func(a, b string) trace.Call {
		full := `{"padding":"` + strings.Repeat("x", 700) + `","items":[{"id":"` + a + `"},{"id":"` + b + `"}]}`
		call := trace.Call{Tool: "mcp:records_list", MCPServer: "records", MCPTool: "list",
			Output: trace.TruncateUTF8(full, 600), OutCollections: trace.ResultCollections(full), Outcome: trace.OutcomeOK}
		call.OutIDs, call.OutCtx, call.OutPaths = trace.OutputRefsPaths(full)
		return call
	}
	act := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_create_link", MCPServer: "records", MCPTool: "create_link",
			Args: map[string]string{"record_id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("long-a", "List and link each record", list("TENG-1", "TENG-2"), act("TENG-1"), act("TENG-2")),
		selSession("long-b", "List and link each record", list("TENG-3", "TENG-4"), act("TENG-3"), act("TENG-4")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_list", "mcp:records_create_link")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil || len(g.Problems) != 0 || len(g.Steps) != 2 || g.Steps[1].LoopResultPath != ".items" {
		t.Fatalf("complete collection metadata must survive short preview: %+v %v", g, err)
	}
	encoded, _ := json.Marshal(ss[0].Calls[0].OutCollections)
	if strings.Contains(string(encoded), "TENG-") {
		t.Fatalf("collection evidence must not retain raw values: %s", encoded)
	}
	ss[0].Calls[0].OutCollections[0].Fields[".id"].Digests[0] = "changed"
	if _, err := SynthesizeProgramGraph(c, ps, ss); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed collection evidence must invalidate call hash: %v", err)
	}
}

func TestSynthesizeProgramGraphJoinsRepeatedProducerResults(t *testing.T) {
	create := func(source, created string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:records_create", MCPServer: "records", MCPTool: "create",
			Args: map[string]string{"source_id": source, "name": "record-" + source}, Output: `{"id":"` + created + `"}`, Outcome: trace.OutcomeOK})
	}
	update := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_update", MCPServer: "records", MCPTool: "update",
			Args: map[string]string{"record_id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("one", "Create from TENG-1 then update it", create("TENG-1", "NEW-1"), update("NEW-1")),
		selSession("two", "Create from TENG-2 and TENG-3 then update each", create("TENG-2", "NEW-2"), create("TENG-3", "NEW-3"), update("NEW-2"), update("NEW-3")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_create", "mcp:records_update")
	variants, err := GroupProgramVariants(c, ps, ss)
	if err != nil || len(variants) != 1 || variants[0].Executions != 2 || variants[0].Proposals < 3 {
		t.Fatalf("overlapping pairwise and combined spans must count two disjoint executions: %+v %v", variants, err)
	}
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil || len(g.Problems) != 0 {
		t.Fatalf("ordered producer results should join consumers: %+v %v", g, err)
	}
	if len(g.Steps) != 2 || g.Steps[0].Loop == "" || g.Steps[1].LoopResultStep != 1 || g.Steps[1].LoopResultPath != "" {
		t.Fatalf("producer output list not inferred: %+v", g.Steps)
	}
	if len(g.Inputs) != 1 || g.Inputs[0].Type != "object" || !g.Inputs[0].List || len(g.Inputs[0].Fields) != 2 {
		t.Fatalf("changing producer arguments should form one item record: %+v", g.Inputs)
	}
	if g.Steps[1].Args[0].Value.Kind != "item_result" || g.Steps[1].Args[0].Value.ResultPath != ".id" {
		t.Fatalf("consumer item result binding missing: %+v", g.Steps[1].Args)
	}
	if _, err := GenerateProgramPackage(g); err != nil {
		t.Fatalf("joined graph should generate: %v", err)
	}
}

func TestSynthesizeProgramGraphRejectsReorderedProducerResults(t *testing.T) {
	create := func(source, created string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:records_create", MCPServer: "records", MCPTool: "create",
			Args: map[string]string{"source_id": source}, Output: `{"id":"` + created + `"}`, Outcome: trace.OutcomeOK})
	}
	update := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_update", MCPServer: "records", MCPTool: "update",
			Args: map[string]string{"record_id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("ordered", "Create from TENG-1 then update it", create("TENG-1", "NEW-1"), update("NEW-1")),
		selSession("reversed", "Create from TENG-2 and TENG-3 then update in reverse", create("TENG-2", "NEW-2"), create("TENG-3", "NEW-3"), update("NEW-3"), update("NEW-2")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_create", "mcp:records_update")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Problems) == 0 {
		t.Fatalf("reverse correspondence needs an explicit transform: %+v", g)
	}
}

func TestSynthesizeProgramGraphBlocksFailedSourceCall(t *testing.T) {
	makeSession := func(id, root, child string, outcome trace.Outcome) trace.Session {
		return selSession(id, "Get a record and act on the returned ID",
			spanRefs(trace.Call{Tool: "mcp:records_get", MCPServer: "records", MCPTool: "get", Args: map[string]string{"id": root}, Output: `{"id":"` + child + `"}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:records_update", MCPServer: "records", MCPTool: "update", Args: map[string]string{"record_id": child}, Outcome: outcome})
	}
	ss := []trace.Session{makeSession("good", "TENG-1", "TENG-2", trace.OutcomeOK), makeSession("failed", "TENG-3", "TENG-4", trace.OutcomeFailed)}
	c, ps := graphCandidateFor(t, ss, "mcp:records_get", "mcp:records_update")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(g.Problems, ";"), "failed in a source execution") {
		t.Fatalf("failed call must block a generated package: %+v", g)
	}
	if _, err := GenerateProgramPackage(g); err == nil {
		t.Fatal("failed source execution became Accept-ready")
	}
}

func TestSynthesizeProgramGraphSelectsUniqueResultField(t *testing.T) {
	list := func(ids []string, statuses []string) trace.Call {
		items := make([]map[string]string, 0, len(ids))
		for i, id := range ids {
			items = append(items, map[string]string{"id": id, "status": statuses[i]})
		}
		body, _ := json.Marshal(map[string]any{"items": items})
		return spanRefs(trace.Call{Tool: "mcp:records_list", MCPServer: "records", MCPTool: "list", Output: string(body), Outcome: trace.OutcomeOK})
	}
	get := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_get", MCPServer: "records", MCPTool: "get", Args: map[string]string{"id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("middle", "Get the failed record", list([]string{"TENG-1", "TENG-2", "TENG-3"}, []string{"ok", "failed", "ok"}), get("TENG-2")),
		selSession("first", "Get the failed record", list([]string{"TENG-4", "TENG-5", "TENG-6"}, []string{"failed", "ok", "ok"}), get("TENG-4")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_list", "mcp:records_get")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil || len(g.Problems) > 0 {
		t.Fatalf("unique varying-position predicate should be determined: %+v %v", g, err)
	}
	v := g.Steps[1].Args[0].Value
	if v.Kind != "selected_result" || v.Step != 1 || v.CollectionPath != ".items" || v.PredicatePath != ".status" || v.ResultPath != ".id" {
		t.Fatalf("selection binding wrong: %+v", v)
	}
	if len(g.Inputs) != 1 || g.Inputs[0].Name != v.Input {
		t.Fatalf("selection value must be an invocation input: %+v", g.Inputs)
	}
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), "failed") || strings.Contains(string(b), "TENG-") {
		t.Fatalf("observed values leaked into program graph: %s", b)
	}
	if _, err := GenerateProgramPackage(g); err != nil {
		t.Fatalf("selection graph should generate: %v", err)
	}
}

func TestSynthesizeProgramGraphLeavesFirstVersusPredicateChoiceToCaller(t *testing.T) {
	list := func(selected, other string) trace.Call {
		return spanRefs(trace.Call{Tool: "mcp:records_list", MCPServer: "records", MCPTool: "list",
			Output: `{"items":[{"id":"` + selected + `","status":"failed"},{"id":"` + other + `","status":"ok"}]}`, Outcome: trace.OutcomeOK})
	}
	get := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:records_get", MCPServer: "records", MCPTool: "get", Args: map[string]string{"id": id}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		selSession("a", "Get failed record", list("TENG-1", "TENG-2"), get("TENG-1")),
		selSession("b", "Get failed record", list("TENG-3", "TENG-4"), get("TENG-3")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:records_list", "mcp:records_get")
	g, err := SynthesizeProgramGraph(c, ps, ss)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Problems) != 0 || len(g.Inputs) != 1 || g.Inputs[0].Type != "integer" ||
		g.Steps[1].Args[0].Value.Kind != "collection_index" {
		t.Fatalf("ambiguous predicate must become an explicit caller index, not an inferred rule: %+v", g)
	}
	if _, err := GenerateProgramPackage(g); err != nil {
		t.Fatalf("caller-selected subgraph should compile without claiming a failed-status predicate: %v", err)
	}
}
