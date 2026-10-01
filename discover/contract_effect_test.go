package discover

import "testing"

func TestMCPEffectUsesLeadingOperationVerb(t *testing.T) {
	cases := []struct {
		name   string
		step   Step
		effect string
	}{
		{"get noun that is also a write verb", Step{Label: "mcp:jira.get_comment"}, "read"},
		{"list noun that is also a write verb", Step{Label: "mcp:jira.list_comments"}, "read"},
		{"write action", Step{Label: "mcp:jira.add_comment"}, "write"},
		{"selected action overrides gateway name", Step{Label: "mcp:telara_execute_action", Slots: []Slot{{Key: "action", Value: "get_comment"}}}, "read"},
		{"camel case action", Step{Label: "mcp:telara_execute_action", Slots: []Slot{{Key: "action", Value: "getComment"}}}, "read"},
		{"opaque selected action", Step{Label: "mcp:telara_execute_action", Slots: []Slot{{Key: "action", Value: "frobnicate"}}}, "unknown"},
		{"ambiguous compound action", Step{Label: "mcp:telara_execute_action", Slots: []Slot{{Key: "action", Value: "search_and_update"}}}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepEffect(tc.step); got != tc.effect {
				t.Fatalf("effect=%q, want %q", got, tc.effect)
			}
		})
	}
}
