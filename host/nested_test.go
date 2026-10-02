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
	if r := call("delete_issue"); r.Gated || !strings.Contains(r.Refused, "destructive") {
		t.Fatalf("a dispatched operation doing more than declared was not refused: %+v", r)
	}
	if r := call("unlisted_thing"); !r.Gated {
		t.Fatalf("an operation the client does not list ran without approval: %+v", r)
	}
	if len(br.calls) != 1 {
		t.Fatalf("want only the read called, got %v", br.calls)
	}
}
