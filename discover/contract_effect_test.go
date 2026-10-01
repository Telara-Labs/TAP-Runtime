package discover

import (
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestMCPEffectUsesLeadingOperationVerb(t *testing.T) {
	cases := []struct {
		name   string
		step   trace.Step
		effect string
	}{
		{"get noun that is also a write verb", trace.Step{Label: "mcp:jira.get_comment"}, "read"},
		{"list noun that is also a write verb", trace.Step{Label: "mcp:jira.list_comments"}, "read"},
		{"write action", trace.Step{Label: "mcp:jira.add_comment"}, "write"},
		{"selected action overrides gateway name", trace.Step{Label: "mcp:telara_execute_action", Slots: []trace.Slot{{Key: "action", Value: "get_comment"}}}, "read"},
		{"camel case action", trace.Step{Label: "mcp:telara_execute_action", Slots: []trace.Slot{{Key: "action", Value: "getComment"}}}, "read"},
		{"opaque selected action", trace.Step{Label: "mcp:telara_execute_action", Slots: []trace.Slot{{Key: "action", Value: "frobnicate"}}}, "unknown"},
		{"ambiguous compound action", trace.Step{Label: "mcp:telara_execute_action", Slots: []trace.Slot{{Key: "action", Value: "search_and_update"}}}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trace.StepEffect(tc.step); got != tc.effect {
				t.Fatalf("effect=%q, want %q", got, tc.effect)
			}
		})
	}
}
