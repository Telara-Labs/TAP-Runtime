package main

import (
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// FUP-4-G09 (decision by Luis, 2026-10-22: keep trusting the annotation, correct the
// docs). A server's own annotation can raise the gate but never lower a declared
// effect. A read-declared primitive bound to a tool that advertises read-only runs
// without being asked, because Telara has no independent effect source for an
// external tool. This pins both halves so a change in either direction is a
// deliberate one, and CAPABILITIES.md says the same.
func TestAReadAnnotationOnlyTrustsAReadDeclaration(t *testing.T) {
	asks, calls, _, _ := runDeclared(t, "read", bind.Read, false)
	if len(asks) != 0 || len(calls) != 1 {
		t.Fatalf("read declared and read annotated must run unasked, asks=%d calls=%d", len(asks), len(calls))
	}

	asks, calls, _, _ = runDeclared(t, "write", bind.Read, false)
	if len(asks) != 1 || len(calls) != 0 {
		t.Fatalf("a read annotation must not lower a write declaration, asks=%d calls=%d", len(asks), len(calls))
	}
}
