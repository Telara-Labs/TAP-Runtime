package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
	"gitlab.com/telara-labs/tap-runtime/bridge"
	mf "gitlab.com/telara-labs/tap-runtime/manifest"
)

func schema(req []string, props ...string) map[string]any {
	p := map[string]any{}
	for i := 0; i+1 < len(props); i += 2 {
		p[props[i]] = map[string]any{"type": props[i+1]}
	}
	m := map[string]any{"type": "object", "properties": p}
	if len(req) > 0 {
		r := make([]any, len(req))
		for i, s := range req {
			r[i] = s
		}
		m["required"] = r
	}
	return m
}

const label = "dev.telara/gmail.threads.search@1"

func contract() mf.Capability {
	args := schema([]string{"query"}, "query", "string", "pageSize", "integer", "pageToken", "string")
	result := map[string]any{"type": "object", "required": []any{"threads"}, "properties": map[string]any{"threads": map[string]any{"type": "array"}}}
	q := "threads matching a query, newest first"
	return mf.Capability{Label: label, ID: mf.CapabilityID(q, args, result), Question: q, Args: args, Result: result}
}

// withSchemas is a client that gives schemas, as Codex does.
func withSchemas(tools ...bind.Tool) *fakeBridge {
	return &fakeBridge{deny: map[string]bool{}, inv: tools, schemas: true}
}

var (
	threadsTool  = bind.Tool{Server: "a", Name: "search_threads", Annotated: bind.Read, Schema: schema([]string{"query"}, "query", "string", "pageSize", "integer", "pageToken", "string", "view", "string")}
	messagesTool = bind.Tool{Server: "a", Name: "search_emails", Annotated: bind.Read, Schema: schema([]string{"query"}, "query", "string", "max_results", "integer")}
	// Nothing in its name says Gmail or search. With a schema, names do no work.
	oddlyNamed = bind.Tool{Server: "broker", Name: "op_17", Annotated: bind.Read, Schema: schema([]string{"query"}, "query", "string", "pageSize", "integer", "pageToken", "string")}
	writer     = bind.Tool{Server: "a", Name: "purge_threads", Annotated: bind.Destructive, Schema: schema([]string{"query"}, "query", "string", "pageSize", "integer", "pageToken", "string")}
)

func decl() []toolDecl {
	return []toolDecl{{Alias: "search", Capability: label, Effect: "read"}}
}

func TestWithASchemaExactlyOneToolMustSatisfy(t *testing.T) {
	a, err := admit(decl(), withSchemas(threadsTool, messagesTool, writer), contract())
	if err != nil {
		t.Fatal(err)
	}
	b := a.byAlias["search"]
	if b.Tool != "search_threads" || !b.ContractChecked || !b.ResultChecked || !strings.HasPrefix(b.Schema, "sha256:") {
		t.Fatalf("%+v", b)
	}

	// Section 4.1.1: the connector that searches messages does not satisfy
	// a contract written for threads. It takes max_results, not pageSize.
	if _, err := admit(decl(), withSchemas(messagesTool), contract()); err == nil || !strings.Contains(err.Error(), "no tool on this client satisfies") {
		t.Fatalf("the messages connector was bound to the threads contract: %v", err)
	}

	// The name does no work: a tool called op_17 binds.
	a, err = admit(decl(), withSchemas(oddlyNamed, messagesTool), contract())
	if err != nil || a.byAlias["search"].Tool != "op_17" {
		t.Fatalf("%v", err)
	}

	// Two that satisfy: refused and named, never scored.
	_, err = admit(decl(), withSchemas(threadsTool, oddlyNamed), contract())
	if err == nil || !strings.Contains(err.Error(), "2 tools satisfy") || !strings.Contains(err.Error(), "op_17") || !strings.Contains(err.Error(), "search_threads") {
		t.Fatalf("two satisfying tools: %v", err)
	}

	// A tool that satisfies the arguments and is annotated as destroying is
	// not a candidate for a declared read.
	if _, err := admit(decl(), withSchemas(writer), contract()); err == nil {
		t.Fatal("a declared read bound to a destructive tool")
	}
}

func TestAPinMustSatisfyToo(t *testing.T) {
	d := decl()
	d[0].Pin = &mf.Pin{Server: "a", Tool: "search_emails"}
	if _, err := admit(d, withSchemas(threadsTool, messagesTool), contract()); err == nil || !strings.Contains(err.Error(), "does not satisfy the contract") {
		t.Fatalf("a pin to a tool that does not satisfy was admitted: %v", err)
	}
	d[0].Pin = &mf.Pin{Server: "a", Tool: "search_threads"}
	a, err := admit(d, withSchemas(threadsTool, oddlyNamed), contract())
	if err != nil || !a.byAlias["search"].ContractChecked {
		t.Fatalf("a pin did not settle two satisfying tools: %v", err)
	}
}

func TestWithoutASchemaOrAContractBindingIsByName(t *testing.T) {
	// No schema from the client: bound by name, answers still checked.
	a, err := admit(decl(), gmail(), contract())
	if err != nil {
		t.Fatal(err)
	}
	if b := a.byAlias["search"]; b.ContractChecked || !b.ResultChecked || b.Tool != "search_threads" {
		t.Fatalf("%+v", b)
	}
	// A schema from the client and no contract in the manifest: by name.
	a, err = admit([]toolDecl{{Alias: "search", Capability: "gmail.threads.search", Effect: "read"}}, withSchemas(bind.Tool{Server: "gmail", Name: "search_threads", Annotated: bind.Read}))
	if err != nil || a.byAlias["search"].ContractChecked || a.byAlias["search"].ResultChecked {
		t.Fatalf("%v", err)
	}
}

// answering returns what it is told to, whatever is asked.
type answering struct {
	fakeBridge
	answer string
}

func (a *answering) Call(bind.Tool, map[string]any) (string, error) { return a.answer, nil }

func TestAnAnswerIsHeldToTheContract(t *testing.T) {
	for name, c := range map[string]struct {
		effect, answer    string
		violation, landed bool
	}{
		"a read that conforms":                          {"read", `{"threads":[]}`, false, false},
		"a read answering a different question":         {"read", `{"emails":[]}`, true, false},
		"a change whose answer is malformed has landed": {"write", `Action completed.`, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			tool := threadsTool
			if c.effect == "write" {
				tool.Annotated = bind.Write
			}
			b := &answering{fakeBridge: *withSchemas(tool), answer: c.answer}
			a, err := admit([]toolDecl{{Alias: "search", Capability: label, Effect: c.effect}}, b, contract())
			if err != nil {
				t.Fatal(err)
			}
			var j bytes.Buffer
			r := callTool(a, b, request{Alias: "search"}, true, &j)
			if r.Violation != c.violation || r.Landed != c.landed {
				t.Fatalf("violation=%v landed=%v: %+v", r.Violation, r.Landed, r)
			}
			if c.violation && (r.Result != "" || !strings.Contains(j.String(), "output_schema_violation")) {
				t.Fatalf("a violating answer reached the program or was not recorded: %+v\n%s", r, j.String())
			}
			if c.landed && !strings.Contains(j.String(), `"landed":true`) {
				t.Fatalf("the record does not say the change was made:\n%s", j.String())
			}
		})
	}
}

// How often does the rule as ruled find exactly one tool, on a real
// inventory? Section 4.1.6 recorded this as unmeasured.
func TestLiveSatisfactionAgainstCodex(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	c, err := bridge.NewCodex()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	inv, err := c.Inventory()
	if err != nil || len(inv) == 0 {
		t.Skipf("no inventory: %v", err)
	}
	var search *bind.Tool
	for i := range inv {
		if inv[i].Name == "gmail.search_emails" {
			search = &inv[i]
		}
	}
	if search == nil {
		t.Skip("this Codex has no Gmail connector")
	}
	props, _ := search.Schema["properties"].(map[string]any)
	var names []string
	for n := range props {
		names = append(names, n)
	}
	t.Logf("gmail.search_emails takes %v, requires %v", names, search.Schema["required"])

	try := func(what string, args map[string]any) {
		cap := mf.Capability{Label: "dev.telara/gmail.emails.search@1", Question: "q", Args: args, Result: map[string]any{"type": "object"}}
		a, err := admit([]toolDecl{{Alias: "s", Capability: cap.Label, Effect: "read"}}, c, cap)
		if err != nil {
			t.Logf("%-52s -> refused: %.220s", what, err)
			return
		}
		t.Logf("%-52s -> bound %s / %s", what, a.byAlias["s"].Server, a.byAlias["s"].Tool)
	}
	try("a loose contract: query only", schema([]string{"query"}, "query", "string"))
	try("the threads contract (pageSize, pageToken)", contract().Args)
	// The connector's own schema, as a contract.
	try("a contract copied from the connector's schema", search.Schema)
}
