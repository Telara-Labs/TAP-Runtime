package main

import (
	"strings"
	"testing"
)

// A confirmation needs no form data. Hosts can apply their own permission
// policy to it; an extra required checkbox strands noninteractive clients.
func TestSaveAcceptsHostAuthorizedConfirmationWithoutFormData(t *testing.T) {
	interpreterStore(t)
	saveHome(t)
	c := startServer(t, true, func(p map[string]any) map[string]any {
		schema, _ := p["requestedSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if len(properties) != 0 {
			return map[string]any{"action": "decline"}
		}
		return map[string]any{"action": "accept", "content": map[string]any{}}
	})
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": authoredDraft(t)}})
	if res["isError"] == true || !strings.Contains(toolText(t, res), "saved") {
		t.Fatalf("host-authorized save = %#v", res)
	}
}

func TestSaveCancellationDoesNotClaimThePersonDeclined(t *testing.T) {
	interpreterStore(t)
	saveHome(t)
	c := startServer(t, true, func(map[string]any) map[string]any {
		return map[string]any{"action": "cancel"}
	})
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": authoredDraft(t)}})
	text := toolText(t, res)
	if res["isError"] != true || !strings.Contains(text, "cancel") || strings.Contains(text, "did not agree") {
		t.Fatalf("canceled save = %#v", res)
	}
}

func TestSaveRejectsInvalidAndLegacyNegativeConfirmations(t *testing.T) {
	interpreterStore(t)
	saveHome(t)
	for _, answer := range []map[string]any{
		{"action": "accept", "content": map[string]any{"approve": false}},
		{"action": "unknown"},
	} {
		c := startServer(t, true, func(map[string]any) map[string]any { return answer })
		res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": authoredDraft(t)}})
		if res["isError"] != true {
			t.Fatalf("invalid confirmation saved a package: %#v", res)
		}
	}
}
