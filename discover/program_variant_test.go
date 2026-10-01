package discover

import (
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestGroupProgramVariantsKeepsOneAndManyTogether(t *testing.T) {
	create := func(id string) trace.Call {
		return testkit.SpanRefs(trace.Call{Tool: "mcp:create", MCPServer: "test", MCPTool: "create",
			Args: map[string]string{"name": "record"}, Output: `{"id":"` + id + `"}`, Outcome: trace.OutcomeOK})
	}
	link := func(root, target string) trace.Call {
		return trace.Call{Tool: "mcp:link", MCPServer: "test", MCPTool: "link",
			Args: map[string]string{"root": root, "target": target}, Outcome: trace.OutcomeOK}
	}
	ss := []trace.Session{
		testkit.NewSession("one", "Create record and link TENG-2", create("TENG-1"), link("TENG-1", "TENG-2")),
		testkit.NewSession("many", "Create record and link TENG-4 and TENG-5", create("TENG-3"), link("TENG-3", "TENG-4"), link("TENG-3", "TENG-5")),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:create", "mcp:link")
	variants, err := codegen.GroupProgramVariants(c, ps, ss)
	if err != nil || len(variants) != 1 || variants[0].Executions != 2 {
		t.Fatalf("one and many should be one invocation contract: %+v %v", variants, err)
	}
}

func TestGroupProgramVariantsSeparatesBindingsAndArgumentShapes(t *testing.T) {
	ss := []trace.Session{
		testkit.NewSession("a", "Get TENG-1 and then follow up",
			testkit.SpanRefs(trace.Call{Tool: "mcp:get", MCPServer: "one", MCPTool: "get", Args: map[string]string{"id": "TENG-1"}, Output: `{"id":"TENG-2"}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:next", MCPServer: "one", MCPTool: "next", Args: map[string]string{"id": "TENG-2"}, Outcome: trace.OutcomeOK}),
		testkit.NewSession("b", "Get TENG-3 and then follow up",
			testkit.SpanRefs(trace.Call{Tool: "mcp:get", MCPServer: "two", MCPTool: "get", Args: map[string]string{"id": "TENG-3"}, Output: `{"id":"TENG-4"}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:next", MCPServer: "two", MCPTool: "next", Args: map[string]string{"id": "TENG-4"}, Outcome: trace.OutcomeOK}),
		testkit.NewSession("c", "Get TENG-5 and then follow up",
			testkit.SpanRefs(trace.Call{Tool: "mcp:get", MCPServer: "one", MCPTool: "get", Args: map[string]string{"id": "TENG-5", "scope": "narrow"}, Output: `{"id":"TENG-6"}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:next", MCPServer: "one", MCPTool: "next", Args: map[string]string{"id": "TENG-6"}, Outcome: trace.OutcomeOK}),
	}
	c, ps := graphCandidateFor(t, ss, "mcp:get", "mcp:next")
	variants, err := codegen.GroupProgramVariants(c, ps, ss)
	if err != nil || len(variants) != 3 {
		t.Fatalf("different binding or typed argument shape must be explicit: %+v %v", variants, err)
	}
}
