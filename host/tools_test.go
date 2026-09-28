package main

import (
	"bytes"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
	mf "gitlab.com/telara-labs/tap-runtime/manifest"
)

// fakeBridge stands in for a client. It is a test double for a third party's
// program, not for a Telara service.
type fakeBridge struct {
	inv     []bind.Tool
	deny    map[string]bool
	calls   []string
	schemas bool
}

func (f *fakeBridge) Client() (string, string)        { return "claude-code", "2.1.284" }
func (f *fakeBridge) Inventory() ([]bind.Tool, error) { return f.inv, nil }
func (f *fakeBridge) HasSchemas() bool                { return f.schemas }
func (f *fakeBridge) Close()                          {}
func (f *fakeBridge) Denied(t bind.Tool) (bool, error) {
	return f.deny[t.Server+"/"+t.Name], nil
}
func (f *fakeBridge) Call(t bind.Tool, args map[string]any) (string, error) {
	f.calls = append(f.calls, t.Server+"/"+t.Name)
	return `{"ok":true}`, nil
}

func gmail() *fakeBridge {
	return &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "claude.ai Gmail", Name: "search_threads", Annotated: bind.Read},
		{Server: "claude.ai Gmail", Name: "get_thread", Annotated: bind.Read},
		{Server: "claude.ai Gmail", Name: "create_draft", Annotated: bind.Unknown},
		{Server: "claude.ai Gmail", Name: "list_labels", Annotated: bind.Unknown},
		{Server: "claude.ai Gmail", Name: "delete_draft", Annotated: bind.Destructive},
	}}
}

func TestAdmitBindsAndRecords(t *testing.T) {
	a, err := admit([]toolDecl{{Alias: "search", Capability: "gmail.threads.search", Effect: "read"}}, gmail())
	if err != nil {
		t.Fatal(err)
	}
	b := a.byAlias["search"]
	if b == nil || b.Tool != "search_threads" || b.Server != "claude.ai Gmail" {
		t.Fatalf("bound %+v", b)
	}
	if b.ContractChecked {
		t.Fatal("the receipt claims a contract check that did not happen")
	}
	if !a.Tested {
		t.Fatal("a listed client version reads as untested")
	}
}

func TestAdmitRefusesRequiredAndSkipsOptional(t *testing.T) {
	if _, err := admit([]toolDecl{{Alias: "s", Capability: "slack.messages.send", Effect: "write"}}, gmail()); err == nil {
		t.Fatal("a required tool with no connector was admitted")
	}
	a, err := admit([]toolDecl{
		{Alias: "s", Capability: "slack.messages.send", Effect: "write", Optional: true},
		{Alias: "search", Capability: "gmail.threads.search", Effect: "read"},
	}, gmail())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Skipped) != 1 || a.Skipped[0] != "s" {
		t.Fatalf("skipped = %v", a.Skipped)
	}
	if got := a.aliases(); len(got) != 1 || got[0] != "search" {
		t.Fatalf("the guest would be told %v", got)
	}
}

func TestAdmitRefusesAToolTheUserDenied(t *testing.T) {
	f := gmail()
	f.deny["claude.ai Gmail/search_threads"] = true
	_, err := admit([]toolDecl{{Alias: "search", Capability: "gmail.threads.search", Effect: "read"}}, f)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("a denied tool was admitted: %v", err)
	}
}

func TestAdmitRefusesBadPackagesBeforeAskingTheClient(t *testing.T) {
	for name, decls := range map[string][]toolDecl{
		"no alias":        {{Capability: "gmail.threads.search", Effect: "read"}},
		"bad effect":      {{Alias: "a", Capability: "gmail.threads.search", Effect: "harmless"}},
		"duplicate alias": {{Alias: "a", Capability: "gmail.threads.search", Effect: "read"}, {Alias: "a", Capability: "gmail.threads.get", Effect: "read"}},
	} {
		if _, err := admit(decls, gmail()); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
}

func TestPin(t *testing.T) {
	pin := func(server, tool string) *mf.Pin { return &mf.Pin{Server: server, Tool: tool} }
	a, err := admit([]toolDecl{{Alias: "x", Capability: "gmail.anything.at_all", Effect: "read", Pin: pin("claude.ai Gmail", "get_thread")}}, gmail())
	if err != nil || a.byAlias["x"].Tool != "get_thread" || !a.byAlias["x"].Pinned {
		t.Fatalf("pin did not bind: %v %+v", err, a)
	}
	if _, err := admit([]toolDecl{{Alias: "x", Capability: "gmail.x.y", Effect: "read", Pin: pin("claude.ai Gmail", "nope")}}, gmail()); err == nil {
		t.Fatal("a pin to a tool that does not exist was admitted")
	}
	if _, err := admit([]toolDecl{{Alias: "x", Capability: "gmail.x.y", Effect: "read", Pin: pin("claude.ai Gmail", "delete_draft")}}, gmail()); err == nil {
		t.Fatal("a declared read was pinned to a destructive tool")
	}
}

func TestCallGate(t *testing.T) {
	f := gmail()
	a, err := admit([]toolDecl{
		{Alias: "search", Capability: "gmail.threads.search", Effect: "read"},
		{Alias: "draft", Capability: "gmail.drafts.create", Effect: "write"},
		{Alias: "labels", Capability: "gmail.labels.list", Effect: "read"},
	}, f)
	if err != nil {
		t.Fatal(err)
	}
	var journal bytes.Buffer
	if r := callTool(a, f, request{Alias: "search"}, false, &journal); r.Refused != "" || r.Result == "" {
		t.Fatalf("a read was not dispatched: %+v", r)
	}
	if r := callTool(a, f, request{Alias: "draft"}, false, &journal); r.Refused == "" {
		t.Fatal("a write ran without approval")
	}
	// Declared read, but its server said nothing about it: ruling 20.
	if r := callTool(a, f, request{Alias: "labels"}, false, &journal); r.Refused == "" {
		t.Fatal("a tool with no annotation ran as a read without approval")
	}
	if r := callTool(a, f, request{Alias: "labels"}, true, &journal); r.Refused != "" {
		t.Fatalf("an approved call was refused: %s", r.Refused)
	}
	if r := callTool(a, f, request{Alias: "never-declared"}, true, &journal); r.Refused == "" {
		t.Fatal("an undeclared alias was dispatched")
	}
	if want := []string{"claude.ai Gmail/search_threads", "claude.ai Gmail/list_labels"}; strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatched %v, want %v", f.calls, want)
	}
	if n := strings.Count(journal.String(), "\n"); n != 5 {
		t.Fatalf("journal has %d lines for 5 calls; refused calls must be recorded too", n)
	}
}

func TestOpenBridgeRefusesClientsThatCannotDispatch(t *testing.T) {
	for _, c := range []string{"cursor", "opencode", "grok"} {
		if _, err := openBridge(c); err == nil || !strings.Contains(err.Error(), "cannot lend") {
			t.Errorf("%s: %v", c, err)
		}
	}
}
