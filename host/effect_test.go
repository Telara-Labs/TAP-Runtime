package main

import (
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// A primitive generated from history declares write when the history could
// not show the effect. The declaration is the least that is ever asked about:
// the tool's own server can raise it but never lower it. A tool it annotates
// read-only is still asked about under a write declaration, and one it says
// nothing about, or that the client asks about, asks as well. A primitive that
// declares read for a read-only tool runs it unasked.
func TestTheToolsOwnServerCanRaiseButNeverLowerTheDeclaredEffect(t *testing.T) {
	pinned := func(tool string) []toolDecl {
		return []toolDecl{{Alias: "a", Capability: "local.discover/x@1", Effect: "write", Pin: &mf.Pin{Server: "claude.ai Gmail", Tool: tool}}}
	}
	for tool, want := range map[string]string{"search_threads": "write", "create_draft": "write"} {
		a, err := admit(pinned(tool), gmail())
		if err != nil {
			t.Fatal(err)
		}
		if got := a.byAlias["a"].effective(); got != want {
			t.Errorf("%s declared write: gate treats it as %q, want %q", tool, got, want)
		}
	}
	readDeclared := []toolDecl{{Alias: "a", Capability: "local.discover/x@1", Effect: "read", Pin: &mf.Pin{Server: "claude.ai Gmail", Tool: "search_threads"}}}
	if a, err := admit(readDeclared, gmail()); err != nil {
		t.Fatal(err)
	} else if got := a.byAlias["a"].effective(); got != "read" {
		t.Errorf("a read declaration of a read-only tool gates as %q, want read", got)
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
