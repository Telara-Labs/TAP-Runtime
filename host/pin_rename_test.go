package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

func telaraPin(tool string) []toolDecl {
	return []toolDecl{{Alias: "commit", Capability: "gitlab.commit.get", Effect: "read", Pin: &mf.Pin{Server: "telara", Tool: tool}}}
}

// The author's machine connects Telara as "telara"; a claude.ai account
// connects the same server as "claude.ai Telara".
func TestPinBindsTheSameToolOnARenamedServer(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "claude.ai Telara", Name: "telara_gitlab_get_commit", Annotated: bind.Read},
		{Server: "claude.ai Gmail", Name: "get_thread", Annotated: bind.Read},
	}}
	a, err := admit(telaraPin("telara_gitlab_get_commit"), br)
	if err != nil {
		t.Fatal(err)
	}
	bd := a.byAlias["commit"]
	if bd.Server != "claude.ai Telara" || bd.Tool != "telara_gitlab_get_commit" || bd.PinnedServer != "telara" || !bd.Pinned {
		t.Fatalf("unexpected binding: %+v", bd)
	}
	if r := callTool(a, br, request{Alias: "commit"}, false, &bytes.Buffer{}); r.Refused != "" {
		t.Fatalf("read refused: %s", r.Refused)
	}
}

func TestPinReachesARenamedTelaraThroughItsDispatcher(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "claude.ai Telara", Name: "telara_execute_action", Annotated: bind.Destructive},
		{Server: "claude.ai Telara", Name: "telara_tool_search", Annotated: bind.Read},
	}, results: map[string]string{
		"claude.ai Telara/telara_tool_search":    "- **telara_gitlab_list_mr_notes** (read) — List notes\n",
		"claude.ai Telara/telara_execute_action": `{"items":[]}`,
	}}
	a, err := admit(telaraPin("telara_gitlab_list_mr_notes"), br)
	if err != nil {
		t.Fatal(err)
	}
	bd := a.byAlias["commit"]
	if bd.Server != "claude.ai Telara" || bd.Tool != "telara_execute_action" || bd.Operation != "gitlab/list_mr_notes" || bd.PinnedServer != "telara" {
		t.Fatalf("unexpected binding: %+v", bd)
	}
	r := callTool(a, br, request{Alias: "commit", Arguments: map[string]any{"params_mr_iid": 7458, "approval_reason": "read"}}, false, &bytes.Buffer{})
	if r.Refused != "" || r.Result == "" {
		t.Fatalf("verified dispatch through the renamed server failed: %+v", r)
	}
}

func TestPinOnTwoRenamedServersIsThePersonsChoice(t *testing.T) {
	inv := []bind.Tool{
		{Server: "claude.ai Telara", Name: "telara_gitlab_get_commit", Annotated: bind.Read},
		{Server: "telara-staging", Name: "telara_gitlab_get_commit", Annotated: bind.Read},
	}
	br := &fakeBridge{deny: map[string]bool{}, inv: inv}
	_, err := admitWith(nil, nil, telaraPin("telara_gitlab_get_commit"), br)
	if err == nil || !strings.Contains(err.Error(), "tap bind --client claude-code server:telara") {
		t.Fatalf("an ambiguous rename was not refused with the bind remedy: %v", err)
	}
	store := newFileBindings(filepath.Join(t.TempDir(), "bindings.json"))
	choose := func(p Pick) (string, bool) { return "telara-staging", true }
	a, err := admitWith(store, choose, telaraPin("telara_gitlab_get_commit"), br)
	if err != nil || a.byAlias["commit"].Server != "telara-staging" {
		t.Fatalf("chosen server not bound: %v", err)
	}
	// The choice is kept: no one is asked the second time.
	a, err = admitWith(store, nil, telaraPin("telara_gitlab_get_commit"), br)
	if err != nil || a.byAlias["commit"].Server != "telara-staging" {
		t.Fatalf("kept choice not reused: %v", err)
	}
}

func TestPinWithNoServerOfferingTheToolNamesTheRemedy(t *testing.T) {
	_, err := admit(telaraPin("telara_gitlab_get_commit"), gmail())
	if err == nil || !strings.Contains(err.Error(), "connect the MCP server that provides it to claude-code") {
		t.Fatalf("refusal does not name the remedy: %v", err)
	}
}

// A connected server under the pinned name is never swapped for another
// server's tool of the same name.
func TestPinNeverLeavesAConnectedPinnedServer(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "telara", Name: "telara_jira_get_issue", Annotated: bind.Read},
		{Server: "other", Name: "telara_gitlab_get_commit", Annotated: bind.Read},
	}}
	if _, err := admit(telaraPin("telara_gitlab_get_commit"), br); err == nil {
		t.Fatal("bound another server's tool while the pinned server is connected")
	}
}

// A renamed server's tool is still held to the declared effect.
func TestRenamedPinStillChecksTheAnnotation(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "claude.ai Telara", Name: "telara_gitlab_get_commit", Annotated: bind.Destructive},
	}}
	if _, err := admit(telaraPin("telara_gitlab_get_commit"), br); err == nil {
		t.Fatal("a declared read bound to a destructive tool on a renamed server")
	}
}
