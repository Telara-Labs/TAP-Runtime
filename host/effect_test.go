package main

import (
	"testing"

	mf "gitlab.com/telara-labs/tap-runtime/contract/manifest"
)

// A primitive generated from history declares write when the history could
// not show the effect. The tool's own server decides within that bound: a
// tool it annotates read-only runs without asking; one it says nothing about,
// or that the client asks about, still asks.
func TestTheToolsOwnServerDecidesWithinTheDeclaredEffect(t *testing.T) {
	pinned := func(tool string) []toolDecl {
		return []toolDecl{{Alias: "a", Capability: "local.discover/x@1", Effect: "write", Pin: &mf.Pin{Server: "claude.ai Gmail", Tool: tool}}}
	}
	for tool, want := range map[string]string{"search_threads": "read", "create_draft": "write"} {
		a, err := admit(pinned(tool), gmail())
		if err != nil {
			t.Fatal(err)
		}
		if got := a.byAlias["a"].effective(); got != want {
			t.Errorf("%s declared write: gate treats it as %q, want %q", tool, got, want)
		}
	}
	br := &askingBridge{fakeBridge: gmail(), ask: map[string]bool{"claude.ai Gmail/search_threads": true}}
	a, err := admit(pinned("search_threads"), br)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.byAlias["a"].effective(); got != "write" {
		t.Errorf("a read-only tool the client asks about runs as %q", got)
	}
	a, err = admit(pinned("delete_draft"), gmail())
	if err != nil {
		t.Fatalf("an effectful tool annotation should promote a write declaration, not prevent binding: %v", err)
	}
	if got := a.byAlias["a"].effective(); got != "destructive" {
		t.Errorf("write declaration with destructive annotation gates as %q, want destructive", got)
	}
}
