package main

import (
	"bytes"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
	mf "gitlab.com/telara-labs/tap-runtime/contract/manifest"
)

// A call through a dispatcher takes the effect of the operation it
// dispatches, when the client also lists that operation as its own tool.
func TestADispatchedCallTakesTheEffectOfTheOperationItNames(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "telara", Name: "telara_execute_action", Annotated: bind.Unknown},
		{Server: "telara", Name: "telara_jira_get_issue", Annotated: bind.Read},
		{Server: "telara", Name: "telara_jira_get_issue_comments", Annotated: bind.Read},
		{Server: "telara", Name: "telara_jira_delete_issue", Annotated: bind.Destructive},
		{Server: "telara", Name: "telara_jira_add_comment", Annotated: bind.Unknown},
	}}
	decl := []toolDecl{{Alias: "step_1", Capability: "local.discover/x@1", Effect: "write", Pin: &mf.Pin{Server: "telara", Tool: "telara_execute_action"}}}
	a, err := admit(decl, br)
	if err != nil {
		t.Fatal(err)
	}
	call := func(action string) reply {
		var j bytes.Buffer
		return callTool(a, br, request{Method: "call", Alias: "step_1", Arguments: map[string]any{
			"integration": "jira", "action": action, "params": `{"issue_key":"K-1"}`}}, false, &j)
	}
	if r := call("get_issue"); r.Refused != "" {
		t.Fatalf("a dispatched read asked for approval: %+v", r)
	}
	if r := call("add_comment"); !r.Gated {
		t.Fatalf("a dispatched operation with no annotation ran without approval: %+v", r)
	}
	if r := call("delete_issue"); !r.Gated || !strings.Contains(r.Refused, "destructive tool needs approval") {
		t.Fatalf("a dispatched destructive operation was not promoted to a destructive approval: %+v", r)
	}
	if r := call("unlisted_thing"); !r.Gated {
		t.Fatalf("an operation the client does not list ran without approval: %+v", r)
	}
	if len(br.calls) != 1 {
		t.Fatalf("want only the read called, got %v", br.calls)
	}
}

func TestGenericTelaraReadIsAllowedOnlyAfterActionCatalogConfirmsIt(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "telara", Name: "telara_execute_action", Annotated: bind.Destructive},
		{Server: "telara", Name: "telara_tool_search", Annotated: bind.Read},
	}, results: map[string]string{
		"telara/telara_tool_search":    "- **telara_jira_get_issue** (read) — Get issue\n",
		"telara/telara_execute_action": `{"key":"TENG-3059"}`,
	}}
	a, err := admit([]toolDecl{{Alias: "read", Capability: "local.discover/jira.get.issue@1", Effect: "read", Pin: &mf.Pin{Server: "telara", Tool: "telara_execute_action"}}}, br)
	if err != nil {
		t.Fatal(err)
	}
	r := callTool(a, br, request{Alias: "read", Arguments: map[string]any{
		"integration": "jira", "action": "get_issue", "params": map[string]any{"issue_key": "TENG-3059"},
	}}, false, &bytes.Buffer{})
	if r.Refused != "" || r.Result == "" {
		t.Fatalf("catalog-confirmed read was refused: %+v", r)
	}
	if got, want := strings.Join(br.calls, ","), "telara/telara_tool_search,telara/telara_execute_action"; got != want {
		t.Fatalf("called %s, want %s", got, want)
	}
}

func TestGenericTelaraReadRefusesWhenCatalogSaysWrite(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "telara", Name: "telara_execute_action", Annotated: bind.Destructive},
		{Server: "telara", Name: "telara_tool_search", Annotated: bind.Read},
	}, results: map[string]string{
		"telara/telara_tool_search": "- **telara_jira_add_comment** (write) — Add comment\n",
	}}
	a, err := admit([]toolDecl{{Alias: "read", Capability: "local.discover/jira.get.issue@1", Effect: "read", Pin: &mf.Pin{Server: "telara", Tool: "telara_execute_action"}}}, br)
	if err != nil {
		t.Fatal(err)
	}
	r := callTool(a, br, request{Alias: "read", Arguments: map[string]any{
		"integration": "jira", "action": "add_comment", "params": map[string]any{"issue_key": "TENG-3059", "body": "test"},
	}}, true, &bytes.Buffer{})
	if r.Refused == "" || !strings.Contains(r.Refused, "declares read") {
		t.Fatalf("write action was not refused for a read primitive: %+v", r)
	}
	if len(br.calls) != 1 || br.calls[0] != "telara/telara_tool_search" {
		t.Fatalf("write action dispatched after preflight: %v", br.calls)
	}
}

func TestCodexBindsMissingTelaraActionPinThroughVerifiedDispatcher(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "telara", Name: "telara_execute_action", Annotated: bind.Destructive},
		{Server: "telara", Name: "telara_tool_search", Annotated: bind.Read},
	}, results: map[string]string{
		"telara/telara_tool_search":    "- **telara_jira_get_issue** (read) — Get issue\n",
		"telara/telara_execute_action": `{"key":"TENG-3059"}`,
	}}
	a, err := admit([]toolDecl{{Alias: "get", Capability: "local.discover/jira.get.issue@1", Effect: "read", Pin: &mf.Pin{Server: "telara", Tool: "telara_jira_get_issue"}}}, br)
	if err != nil {
		t.Fatal(err)
	}
	bd := a.byAlias["get"]
	if bd.Tool != "telara_execute_action" || bd.Operation != "jira/get_issue" {
		t.Fatalf("unexpected compatibility binding: %+v", bd)
	}
	r := callTool(a, br, request{Alias: "get", Arguments: map[string]any{"params_issue_key": "TENG-3059", "approval_reason": "read test"}}, false, &bytes.Buffer{})
	if r.Refused != "" || r.Result == "" {
		t.Fatalf("verified generic dispatch failed: %+v", r)
	}
	if len(br.args) != 2 {
		t.Fatalf("want search and action call arguments, got %d", len(br.args))
	}
	actionArgs := br.args[1]
	params, _ := actionArgs["params"].(map[string]any)
	if actionArgs["integration"] != "jira" || actionArgs["action"] != "get_issue" || params["issue_key"] != "TENG-3059" || actionArgs["approval_reason"] != "read test" {
		t.Fatalf("direct-action arguments were not adapted: %#v", actionArgs)
	}
}

func TestTelaraActionArgumentAdapterRejectsMalformedParams(t *testing.T) {
	for _, args := range []map[string]any{
		{"params": "not-json"},
		{"params": 42},
		{"params_": "value"},
	} {
		if _, err := wrapTelaraActionArgs("jira", "get_issue", args); err == nil {
			t.Fatalf("accepted malformed action arguments: %#v", args)
		}
	}
}
