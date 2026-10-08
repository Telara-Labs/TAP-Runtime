package main

import (
	"strings"
	"testing"
)

// Asked why they skipped TAP, Goose and Kilo quoted "Not for one-off
// requests" (a question about one commit looked like a one-off), and Crush
// quoted "a task that takes several tool calls" (it expected one fetch to
// do). A first ask is exactly when only tap_search can say whether the task
// repeats, so nothing that tells an agent to search may offer those outs.
func TestSearchGuidanceGivesNoReasonToSkipTheSearch(t *testing.T) {
	desc, _ := searchTool["description"].(string)
	skill := strings.SplitN(authorSkill, "\n---", 2)[0]
	for name, text := range map[string]string{"server instructions": serverInstructions, "tap_search": desc, "tap-author description": skill} {
		if text == "" {
			t.Fatalf("%s: empty", name)
		}
		for _, out := range []string{"several tool calls", "Not for one-off", "asks for repeatedly"} {
			if strings.Contains(text, out) {
				t.Errorf("%s offers a reason to skip the search: %q", name, out)
			}
		}
		if !strings.Contains(text, "every request") {
			t.Errorf("%s does not say to search on every request", name)
		}
	}
}
