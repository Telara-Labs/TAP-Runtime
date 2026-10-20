package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// A primitive's declared effect is the least that is ever asked about. A
// server annotation or a dispatched operation can raise what is asked, never
// lower it below what the primitive declared. A tool whose server calls it
// read-only is therefore still asked about when the primitive declares write.

// runDeclared runs a real primitive that calls one pinned tool once, through
// the real Run gate, and reports what was asked, what reached the client and
// what the journal recorded.
func runDeclared(t *testing.T, declared string, annotated bind.Effect, allow bool) (asks []Ask, calls []string, begin string, finish string) {
	t.Helper()
	store := interpreterStore(t)
	if _, _, _, err := obtain(store, "main.py"); err != nil {
		t.Fatal(err)
	}
	inDir(t)
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "desktop_repl", Name: "js", Annotated: annotated},
	}}
	pkg, runs := t.TempDir(), t.TempDir()
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: floor, version: 0.1.0}\nexecution: {entrypoint: main.py}\ntools:\n  - {alias: js, capability: dev.test/page.script.run@1, effect: " + declared + ", pin: {server: desktop_repl, tool: js}}\n"
	program := "try:\n    tap.call('js', {'code': 'document.title'})\nexcept PermissionError:\n    print('refused')\n"
	if err := os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "main.py"), []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Package: pkg, RunsDir: runs, InterpDir: store, Bridge: br, Journal: io.Discard, Approve: func(a Ask) Grant {
		asks = append(asks, a)
		return Grant{OK: allow, Limit: 1}
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(runs, res.RunID, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var entry struct {
			Phase   string `json:"phase"`
			Effect  string `json:"effect"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Phase == "begin" {
			begin = entry.Effect
		}
		if entry.Phase == "finish" {
			finish = entry.Outcome
		}
	}
	return asks, br.calls, begin, finish
}

func TestADeclaredWriteIsAskedAboutWhateverTheServerAnnotates(t *testing.T) {
	cases := []struct {
		name      string
		declared  string
		annotated bind.Effect
		allow     bool
		wantAsk   string // empty means no question
		wantCalls int
		wantBegin string
	}{
		// The reported defect: a read annotation lowered a declared write.
		{"declared write, annotated read, declined", "write", bind.Read, false, "write", 0, "write"},
		{"declared write, annotated read, approved", "write", bind.Read, true, "write", 1, "write"},
		// What stays as it was.
		{"declared write, no annotation, declined", "write", bind.Unknown, false, "write", 0, "write"},
		{"declared write, annotated destructive, declined", "write", bind.Destructive, false, "destructive", 0, "destructive"},
		{"declared write, annotated destructive, approved", "write", bind.Destructive, true, "destructive", 1, "destructive"},
		{"declared destructive, annotated read, declined", "destructive", bind.Read, false, "destructive", 0, "destructive"},
		{"declared read, annotated read runs unasked", "read", bind.Read, false, "", 1, "read"},
		{"declared read, no annotation is still asked as a write", "read", bind.Unknown, false, "write", 0, "write"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asks, calls, begin, finish := runDeclared(t, c.declared, c.annotated, c.allow)
			if c.wantAsk == "" {
				if len(asks) != 0 {
					t.Fatalf("asked about a call that should run unasked: %+v", asks)
				}
			} else if len(asks) != 1 || asks[0].Effect != c.wantAsk {
				t.Fatalf("asked %+v, want one %s question", asks, c.wantAsk)
			}
			if len(calls) != c.wantCalls {
				t.Fatalf("%d calls reached the client (%v), want %d", len(calls), calls, c.wantCalls)
			}
			if begin != c.wantBegin {
				t.Fatalf("journal records effect %q, want %q", begin, c.wantBegin)
			}
			wantFinish := "completed"
			if c.wantAsk != "" && !c.allow {
				wantFinish = "completed_with_refusals"
			}
			if finish != wantFinish {
				t.Fatalf("finish = %q, want %q", finish, wantFinish)
			}
		})
	}
}

// The effect the gate uses, for every pairing, so a later edit cannot lower it.
func TestEffectiveNeverFallsBelowTheDeclaredEffect(t *testing.T) {
	cases := []struct {
		declared  string
		annotated bind.Effect
		asked     bool
		want      string
	}{
		{"write", bind.Read, false, "write"},
		{"destructive", bind.Read, false, "destructive"},
		{"financial", bind.Read, false, "financial"},
		{"identity-admin", bind.Read, false, "identity-admin"},
		{"write", bind.Write, false, "write"},
		{"write", bind.Destructive, false, "destructive"},
		{"destructive", bind.Write, false, "destructive"},
		{"write", bind.Unknown, false, "write"},
		{"read", bind.Read, false, "read"},
		{"read", bind.Unknown, false, "write"},
		{"read", bind.Read, true, "write"},
		{"write", bind.Read, true, "write"},
	}
	for _, c := range cases {
		b := &binding{Declared: c.declared, tool: bind.Tool{Server: "s", Name: "t", Annotated: c.annotated}, Asked: c.asked, Gated: c.annotated == bind.Unknown}
		if got := b.effective(); got != c.want {
			t.Errorf("declared %s, annotated %q, asked %v gates as %q, want %q", c.declared, c.annotated, c.asked, got, c.want)
		}
	}
}

// The same floor holds when the effect comes from a dispatched operation. A
// verified read operation behind a dispatcher the primitive declared write for
// is still asked about; a stricter operation still raises it.
func TestADispatchedReadDoesNotLowerADeclaredWrite(t *testing.T) {
	newBridge := func() *fakeBridge {
		return &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
			{Server: "telara", Name: "telara_execute_action", Annotated: bind.Unknown},
			{Server: "telara", Name: "telara_jira_get_issue", Annotated: bind.Read},
			{Server: "telara", Name: "telara_jira_delete_issue", Annotated: bind.Destructive},
		}}
	}
	pin := &mf.Pin{Server: "telara", Tool: "telara_execute_action"}
	call := func(effect, action string) (reply, *fakeBridge) {
		br := newBridge()
		a, err := admit([]toolDecl{{Alias: "step", Capability: "local.discover/x@1", Effect: effect, Pin: pin}}, br)
		if err != nil {
			t.Fatal(err)
		}
		return callTool(a, br, request{Method: "call", Alias: "step", Arguments: map[string]any{"integration": "jira", "action": action, "params": `{"issue_key":"K-1"}`}}, false, &bytes.Buffer{}), br
	}
	if r, br := call("write", "get_issue"); !r.Gated || len(br.calls) != 0 {
		t.Fatalf("a declared write ran because the dispatched operation reads: %+v calls=%v", r, br.calls)
	}
	if r, br := call("read", "get_issue"); r.Refused != "" || len(br.calls) != 1 {
		t.Fatalf("a declared read of a dispatched read was asked about: %+v calls=%v", r, br.calls)
	}
	if r, _ := call("write", "delete_issue"); !r.Gated || !strings.Contains(r.Refused, "destructive tool needs approval") {
		t.Fatalf("a stricter dispatched operation did not raise the question: %+v", r)
	}
}

// A catalog that confirms an action is a read lowers nothing either.
func TestACatalogReadDoesNotLowerADeclaredWrite(t *testing.T) {
	br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
		{Server: "telara", Name: "telara_execute_action", Annotated: bind.Destructive},
		{Server: "telara", Name: "telara_tool_search", Annotated: bind.Read},
	}, results: map[string]string{
		"telara/telara_tool_search":    "- **telara_jira_get_issue** (read) — Get issue\n",
		"telara/telara_execute_action": `{"key":"K-1"}`,
	}}
	pin := &mf.Pin{Server: "telara", Tool: "telara_execute_action"}
	a, err := admit([]toolDecl{{Alias: "step", Capability: "local.discover/jira.get.issue@1", Effect: "write", Pin: pin}}, br)
	if err != nil {
		t.Fatal(err)
	}
	r := callTool(a, br, request{Alias: "step", Arguments: map[string]any{"integration": "jira", "action": "get_issue", "params": map[string]any{"issue_key": "K-1"}}}, false, &bytes.Buffer{})
	if !r.Gated {
		t.Fatalf("a declared write ran because the catalog calls the action a read: %+v", r)
	}
	for _, call := range br.calls {
		if call == "telara/telara_execute_action" {
			t.Fatalf("the action was dispatched without approval: %v", br.calls)
		}
	}
}

// A nested read behind a gateway the client does not name settles an
// unannotated tool the primitive declared read, and never lowers a write one.
func TestANestedReadDoesNotLowerADeclaredWrite(t *testing.T) {
	newBridge := func() *fakeBridge {
		return &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
			{Server: "gw", Name: "run", Annotated: bind.Unknown},
			{Server: "gw", Name: "get_issue", Annotated: bind.Read},
		}}
	}
	call := func(effect string) (reply, *fakeBridge) {
		br := newBridge()
		a, err := admit([]toolDecl{{Alias: "op", Capability: "local.discover/x@1", Effect: effect, Pin: &mf.Pin{Server: "gw", Tool: "run"}}}, br)
		if err != nil {
			t.Fatal(err)
		}
		return callTool(a, br, request{Method: "call", Alias: "op", Arguments: map[string]any{"operation": "get_issue", "params": map[string]any{"key": "K-1"}}}, false, &bytes.Buffer{}), br
	}
	if r, br := call("write"); !r.Gated || len(br.calls) != 0 {
		t.Fatalf("a declared write ran because a nested operation reads: %+v calls=%v", r, br.calls)
	}
	if r, br := call("read"); r.Refused != "" || len(br.calls) != 1 {
		t.Fatalf("a declared read of a nested read was asked about: %+v calls=%v", r, br.calls)
	}
}

// Financial and identity-admin share a rank with destructive. When a dispatched
// or catalog-confirmed operation has the same rank as the declaration, the
// primitive's declared category is what is asked about, not whichever class the
// operation happened to be annotated with. A genuinely higher rank still wins.
func TestADeclaredCategoryIsKeptWhenARankTies(t *testing.T) {
	pin := &mf.Pin{Server: "telara", Tool: "telara_execute_action"}

	// A dispatched operation the client lists as its own tool, annotated destructive.
	nested := func(declared string) reply {
		br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
			{Server: "telara", Name: "telara_execute_action", Annotated: bind.Unknown},
			{Server: "telara", Name: "telara_jira_delete_issue", Annotated: bind.Destructive},
		}}
		a, err := admit([]toolDecl{{Alias: "step", Capability: "local.discover/x@1", Effect: declared, Pin: pin}}, br)
		if err != nil {
			t.Fatal(err)
		}
		r := callTool(a, br, request{Method: "call", Alias: "step", Arguments: map[string]any{"integration": "jira", "action": "delete_issue", "params": `{"issue_key":"K-1"}`}}, false, &bytes.Buffer{})
		if len(br.calls) != 0 {
			t.Fatalf("a gated call reached the client: %v", br.calls)
		}
		return r
	}
	// An action the Telara catalog reports with the given effect word.
	catalog := func(declared, catalogEffect string) (reply, *fakeBridge) {
		br := &fakeBridge{deny: map[string]bool{}, inv: []bind.Tool{
			{Server: "telara", Name: "telara_execute_action", Annotated: bind.Destructive},
			{Server: "telara", Name: "telara_tool_search", Annotated: bind.Read},
		}, results: map[string]string{
			"telara/telara_tool_search": "- **telara_billing_do_thing** (" + catalogEffect + ") — Do thing\n",
		}}
		a, err := admit([]toolDecl{{Alias: "step", Capability: "local.discover/x@1", Effect: declared, Pin: pin}}, br)
		if err != nil {
			t.Fatal(err)
		}
		r := callTool(a, br, request{Method: "call", Alias: "step", Arguments: map[string]any{"integration": "billing", "action": "do_thing", "params": map[string]any{"k": "v"}}}, false, &bytes.Buffer{})
		return r, br
	}
	want := func(t *testing.T, r reply, class string) {
		t.Helper()
		if !r.Gated || r.Refused != class+" tool needs approval" {
			t.Fatalf("asked %q (gated=%v), want a %s approval", r.Refused, r.Gated, class)
		}
	}
	wantCatalog := func(t *testing.T, declared, catalogEffect, class string) {
		t.Helper()
		r, br := catalog(declared, catalogEffect)
		for _, call := range br.calls {
			if call == "telara/telara_execute_action" {
				t.Fatalf("a gated action was dispatched: %v", br.calls)
			}
		}
		want(t, r, class)
	}

	for _, declared := range []string{"financial", "identity-admin"} {
		t.Run("dispatched destructive under declared "+declared, func(t *testing.T) { want(t, nested(declared), declared) })
		t.Run("catalog delete under declared "+declared, func(t *testing.T) { wantCatalog(t, declared, "delete", declared) })
	}
	t.Run("catalog financial under declared destructive keeps destructive", func(t *testing.T) {
		wantCatalog(t, "destructive", "financial", "destructive")
	})
	t.Run("catalog identity-admin under declared financial keeps financial", func(t *testing.T) {
		wantCatalog(t, "financial", "identity-admin", "financial")
	})
	t.Run("catalog financial under declared identity-admin keeps identity-admin", func(t *testing.T) {
		wantCatalog(t, "identity-admin", "financial", "identity-admin")
	})
	// A genuinely higher rank still promotes, and a lower one is raised to the declaration.
	t.Run("dispatched destructive promotes a declared write", func(t *testing.T) { want(t, nested("write"), "destructive") })
	t.Run("catalog financial promotes a declared write", func(t *testing.T) { wantCatalog(t, "write", "financial", "financial") })
	t.Run("catalog write is raised to a declared financial", func(t *testing.T) { wantCatalog(t, "financial", "write", "financial") })
	t.Run("catalog read is raised to a declared identity-admin", func(t *testing.T) { wantCatalog(t, "identity-admin", "read", "identity-admin") })
	t.Run("catalog read under declared read still runs unasked", func(t *testing.T) {
		r, br := catalog("read", "read")
		if r.Refused != "" || r.Gated {
			t.Fatalf("a declared read of a catalog read was asked about: %+v", r)
		}
		if got := strings.Join(br.calls, ","); got != "telara/telara_tool_search,telara/telara_execute_action" {
			t.Fatalf("a checked read was not dispatched normally: %s", got)
		}
	})
}

// The direct (non-dispatched) gate keeps the declared category on a tie too.
func TestEffectiveKeepsTheDeclaredCategoryOnATie(t *testing.T) {
	for _, c := range []struct {
		declared  string
		annotated bind.Effect
		want      string
	}{
		{"financial", bind.Destructive, "financial"},
		{"identity-admin", bind.Destructive, "identity-admin"},
		{"destructive", bind.Financial, "destructive"},
		{"destructive", bind.IdentityAdmin, "destructive"},
		{"financial", bind.IdentityAdmin, "financial"},
		{"write", bind.Financial, "financial"},
	} {
		b := &binding{Declared: c.declared, tool: bind.Tool{Server: "s", Name: "t", Annotated: c.annotated}}
		if got := b.effective(); got != c.want {
			t.Errorf("declared %s, annotated %s gates as %q, want %q", c.declared, c.annotated, got, c.want)
		}
	}
}
