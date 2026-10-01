package routine

import (
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// An MCP tool's effect is never inferred from a verb in its name or in a
// selected action: a session log does not record what the tool declares.
func TestMCPEffectIsNotInferredFromNames(t *testing.T) {
	for _, st := range []trace.Step{
		{Label: "mcp:jira.get_comment"},
		{Label: "mcp:jira.add_comment"},
		{Label: "mcp:telara_execute_action", Slots: []trace.Slot{{Key: "action", Value: "get_comment"}}},
		{Label: "mcp:telara_execute_action", Slots: []trace.Slot{{Key: "action", Value: "frobnicate"}}},
	} {
		if got := trace.StepEffect(st); got != "unknown" {
			t.Errorf("%s %v: effect=%q, want unknown", st.Label, st.Slots, got)
		}
	}
}
