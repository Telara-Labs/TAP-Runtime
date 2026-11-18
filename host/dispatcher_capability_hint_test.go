package main

import (
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// An unpinned capability that names the dispatcher cannot bind: no operation
// matches it, and the dispatcher is broader than a read. The refusal says what
// to declare instead, as tap resolve does, and nothing is called.
func TestUnpinnedDispatcherCapabilitySaysWhatToDeclare(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "telara", Name: "telara_execute_action", Annotated: bind.Destructive},
		{Server: "telara", Name: "telara_tool_search", Annotated: bind.Read},
	}}
	_, err := admit([]toolDecl{{Alias: "execute", Capability: "local.discover/telara.execute_action@1", Effect: "read"}}, br)
	if err == nil {
		t.Fatal("an unpinned dispatcher capability bound")
	}
	for _, want := range []string{"names the gateway's dispatcher, not an operation", "telara.list_projects", "or pin the dispatcher"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q lacks %q", err, want)
		}
	}
	if len(br.calls) != 0 {
		t.Fatalf("refused binding called %v", br.calls)
	}

	// Pinned, the same declaration binds; each call is then checked against
	// the gateway catalog.
	if _, err := admit([]toolDecl{{Alias: "execute", Capability: "local.discover/telara.execute_action@1", Effect: "read", Pin: &mf.Pin{Server: "telara", Tool: "telara_execute_action"}}}, br); err != nil {
		t.Fatalf("pinned dispatcher refused: %v", err)
	}
}

func TestDispatcherCapabilityHintOnlyForTheDispatcher(t *testing.T) {
	if got := dispatcherCapabilityHint("local.discover/jira.get.issue@1"); got != "" {
		t.Fatalf("hint for an operation capability: %q", got)
	}
}
