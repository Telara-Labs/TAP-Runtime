package primitive

import (
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestRelationshipEvidenceRequiresStructuredSameRequestHeadBinding(t *testing.T) {
	p := Primitive{Steps: []string{"mcp:create", "mcp:react"}}
	by := map[string]*trace.Session{}
	for i, observation := range []Observed{
		{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: ".key", Label: Explicit},
		{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: "output line", Label: Inferred},
		{Step: 2, Arg: "issue_key", Source: "step", From: 1, Selector: ".key", Label: Explicit},
	} {
		id := string(rune('a' + i))
		s := &trace.Session{Client: "test", ID: id, Calls: []trace.Call{{Request: 0}, {Request: 0}}}
		if i == 2 {
			s.Calls[1].Request = 1
		}
		by["test\x00"+id] = s
		p.Executions = append(p.Executions, Execution{Client: "test", Session: id, Request: 0,
			Calls: []CallRef{{Step: 1, Index: 0}, {Step: 2, Index: 1}}, Observed: []Observed{observation}})
	}
	score, support, reason := RelationshipEvidence(p, by)
	if score != 33 || !strings.HasPrefix(support, "1/3") || !strings.Contains(reason, "request boundary") {
		t.Fatalf("relationship=%d %q %q", score, support, reason)
	}
}
