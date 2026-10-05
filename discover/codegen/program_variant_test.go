package codegen_test

import (
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/internal/testkit"

	"github.com/Telara-Labs/TAP-Runtime/discover/codegen"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
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
	c, ps := testkit.GraphCandidateFor(t, ss, "mcp:create", "mcp:link")
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
	// A choice the caller never supplied (scope=narrow, once) is part of
	// the operation by the corpus evidence: its own family. Bindings split
	// within a family as variants.
	c, ps := testkit.GraphCandidateFor(t, ss, "mcp:get", "mcp:next")
	variants, err := codegen.GroupProgramVariants(c, ps, ss)
	if err != nil || len(variants) != 2 {
		t.Fatalf("different bindings must be explicit variants: %+v %v", variants, err)
	}
	for _, id := range c.Members {
		for _, p := range ps {
			if p.ID == id && p.Session == "c" {
				t.Fatalf("a different argument shape must not merge: %+v", c)
			}
		}
	}
}
