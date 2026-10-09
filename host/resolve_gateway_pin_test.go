package main

import (
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// A pin on a configured gateway's dispatcher, on a client that lists no
// tools (Gemini, Kilo): the runner learns each operation's effect from that
// gateway's catalog, so the catalog tools are lent as reads beside the pin,
// and a read operation is verified as a read.
func TestResolvePinnedDispatcherLendsTheGatewayCatalog(t *testing.T) {
	b := newUnlisted(connected("telara", "", "https://api.telara.dev/v1/mcp"))
	m := resolveManifest(toolDecl{Alias: "gl", Capability: "gitlab.list_projects", Effect: "read", Pin: &mf.Pin{Server: "telara", Tool: "telara_execute_action"}})
	if _, _, err := resolveUnlisted(m, b, tempStore(t), nil); err != nil {
		t.Fatal(err)
	}
	inv, err := b.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	search, ok := findTool(inv, "telara", "telara_tool_search")
	if !ok || search.Annotated != bind.Read {
		t.Fatalf("the gateway's catalog search was not lent as a read: %+v", inv)
	}
	effect, err := telaraActionEffect(b, "telara", inv, "google_workspace", "calendar_list_events")
	if err != nil || effect != string(bind.Read) {
		t.Fatalf("a read operation through the pinned dispatcher: effect %q, %v", effect, err)
	}
}

// A pin on a server that is not a gateway lends nothing beside it.
func TestResolvePinOnAnOrdinaryServerLendsOnlyThePin(t *testing.T) {
	b := newUnlisted(connected("tracker", "tracker-mcp", ""))
	m := resolveManifest(toolDecl{Alias: "s", Capability: "tracker.issues.search", Effect: "read", Pin: &mf.Pin{Server: "tracker", Tool: "search_issues"}})
	if _, _, err := resolveUnlisted(m, b, tempStore(t), nil); err != nil {
		t.Fatal(err)
	}
	if len(b.given) != 1 || b.given[0].Name != "search_issues" {
		t.Fatalf("lent %+v, want only the pin", b.given)
	}
}

// A capability that names the dispatcher, not an operation, is refused with
// what to declare instead.
func TestResolveSaysWhenACapabilityNamesTheDispatcher(t *testing.T) {
	b := newUnlisted(connected("telara", "", "https://api.telara.dev/v1/mcp"))
	m := resolveManifest(toolDecl{Alias: "gitlab", Capability: "gitlab.execute_action", Effect: "read"})
	_, _, err := resolveUnlisted(m, b, tempStore(t), nil)
	if err == nil || !strings.Contains(err.Error(), "names the gateway's dispatcher, not an operation") || !strings.Contains(err.Error(), "gitlab.list_projects") {
		t.Fatalf("refusal: %v", err)
	}
}
