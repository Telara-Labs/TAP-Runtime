package main

import (
	"bytes"
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

// TENG-3103 (G3): the contract holds what the program sends, not only what the
// connector accepts at admission.
func TestACallIsHeldToTheContractsArguments(t *testing.T) {
	tool := threadsTool
	tool.Server = "gmail"
	br := withSchemas(tool)
	a, err := admit(decl(), br, contract())
	if err != nil {
		t.Fatal(err)
	}
	var j bytes.Buffer
	for name, args := range map[string]map[string]any{
		"an argument the contract does not declare": {"query": "x", "view": "full"},
		"a missing required argument":               {"pageSize": 5},
		"a value of the wrong type":                 {"query": 7},
	} {
		rq := request{Method: "call", Alias: "search", Arguments: args}
		if r := callTool(a, br, rq, true, &j); r.Refused == "" || len(br.calls) != 0 {
			t.Errorf("%s was sent to the tool: %+v", name, r)
		}
	}
	if !strings.Contains(j.String(), "refused_arguments") {
		t.Errorf("the refusals are not in the record: %s", j.String())
	}
	ok := request{Method: "call", Alias: "search", Arguments: map[string]any{"query": "x", "pageSize": 5}}
	if r := callTool(a, br, ok, true, &j); r.Refused != "" || len(br.calls) != 1 {
		t.Fatalf("a call that matches the contract was refused: %+v", r)
	}
}

func TestACallToAToolWithNoContractIsNotHeldToOne(t *testing.T) {
	br := gmail()
	a, err := admit(searchDecl, br)
	if err != nil {
		t.Fatal(err)
	}
	var j bytes.Buffer
	if r := callTool(a, br, request{Method: "call", Alias: "search", Arguments: map[string]any{"anything": 1}}, true, &j); r.Refused != "" {
		t.Fatalf("a call with no contract was refused: %+v", r)
	}
}
