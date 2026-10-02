package main

import (
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

// A client that can say which tools the person set to "ask first".
type askingBridge struct {
	*fakeBridge
	ask map[string]bool
}

func (a *askingBridge) Asks(t bind.Tool) (bool, error) { return a.ask[t.Server+"/"+t.Name], nil }

func TestAToolTheClientAsksAboutIsGatedEvenWhenDeclaredARead(t *testing.T) {
	br := &askingBridge{fakeBridge: gmail(), ask: map[string]bool{"claude.ai Gmail/search_threads": true}}
	a, err := admit(searchDecl, br)
	if err != nil {
		t.Fatal(err)
	}
	b := a.byAlias["search"]
	if !b.Asked || b.effective() != "write" {
		t.Fatalf("a tool the client asks about was treated as %q (asked=%v)", b.effective(), b.Asked)
	}
	plain, _ := admit(searchDecl, gmail())
	if plain.byAlias["search"].effective() != "read" {
		t.Fatal("a client that cannot say anything changed a read")
	}
}

func TestADeniedToolIsRefusedOnTheCodexShapedBridge(t *testing.T) {
	br := gmail()
	br.deny["claude.ai Gmail/search_threads"] = true
	if _, err := admit(searchDecl, br); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("a denied tool bound: %v", err)
	}
}

func TestThePersonIsAskedBeforeACallToAToolTheirClientAsksAbout(t *testing.T) {
	br := &askingBridge{fakeBridge: gmail(), ask: map[string]bool{"claude.ai Gmail/search_threads": true}}
	var asks []Ask
	res, err := runLimited(t, "main.py", "tools:\n  - {alias: search, capability: gmail.threads.search, effect: read}\n", `
try:
    tap.call("search", {})
    print("called")
except PermissionError:
    print("refused")
`, Options{Bridge: br, Approve: func(a Ask) Grant { asks = append(asks, a); return Grant{} }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "refused" || len(asks) != 1 || !strings.Contains(asks[0].Example, "your client is set to ask") {
		t.Fatalf("got %q, asks %+v", res.Stdout, asks)
	}
	if len(br.calls) != 0 {
		t.Fatalf("the tool was called without the person's yes: %v", br.calls)
	}
}
