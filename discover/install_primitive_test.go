package discover

import (
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
)

func TestMultiContinuationFamilyDoesNotInstallOneChain(t *testing.T) {
	install := primitiveInstaller(nil, "claude-code", t.TempDir(), t.TempDir())
	result, err := install(primitive.Family{FollowUps: []primitive.FollowUp{
		{Steps: []string{"mcp:issue_transition"}, Runs: 3},
		{Steps: []string{"mcp:issue_link"}, Runs: 2},
	}}, []primitive.Primitive{{Steps: []string{"mcp:issue_create", "mcp:issue_transition"}}})
	if err != nil || result.Installed || !strings.Contains(result.Reason, "selection rule") {
		t.Fatalf("multi-continuation family installed one chain: %+v, %v", result, err)
	}
}
