package primitive

import (
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/client"
)

func TestAgentCommandUsesTheRegistryLaunchLine(t *testing.T) {
	for _, c := range client.All() {
		got := agentCommand(c.ID, "/h/HANDOFF.md")
		if client.HasLaunch(c) {
			if !strings.HasPrefix(got, strings.Join(c.Launch, " ")+` "Read /h/HANDOFF.md`) {
				t.Errorf("%s: %s", c.ID, got)
			}
		} else if !strings.HasPrefix(got, "Ask your coding agent:") {
			t.Errorf("%s: %s", c.ID, got)
		}
	}
	// The first launchable client in the list wins.
	if got := agentCommand("aider,codex,claude-code", "H"); !strings.HasPrefix(got, "codex ") {
		t.Errorf("got %s", got)
	}
}
