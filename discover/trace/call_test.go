package trace

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOutputRefsPaths(t *testing.T) {
	text := `{"key":"PROJ-12","url":"https://example.com/browse/PROJ-12","again":"PROJ-12"}`
	ids, ctx, paths := OutputRefsPaths(text)
	if want := []string{"PROJ-12", "https://example.com/browse/PROJ-12"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %q, want %q", ids, want)
	}
	if len(ctx) != 2 || !strings.HasSuffix(ctx[0], "\x00\"") || !strings.Contains(ctx[0], `"key":"`) {
		t.Errorf("ctx = %q", ctx)
	}
	// PROJ-12 is a value at two places, so its path is ambiguous.
	if want := []string{"*", ".url"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %q, want %q", paths, want)
	}
	if got := OutputIDs("no identifiers here"); len(got) != 0 {
		t.Errorf("OutputIDs = %q", got)
	}
}

func TestOutputRefsStopsAt64(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("ID-")
		b.WriteString(strings.Repeat("1", 1+i%9))
		b.WriteString(string(rune('0' + i/10)))
		b.WriteString(string(rune('0' + i%10)))
		b.WriteString("\n")
	}
	if ids := OutputIDs(b.String()); len(ids) != 64 {
		t.Errorf("%d ids, want 64", len(ids))
	}
}

func TestOutputRefsContextStartsAtTheLine(t *testing.T) {
	_, ctx := OutputRefs("first line\nid: PROJ-7.")
	if len(ctx) != 1 || ctx[0] != "id: \x00." {
		t.Errorf("ctx = %q", ctx)
	}
	// A JSON-encoded result keeps "\n" escaped.
	_, ctx = OutputRefs(`first\nid: PROJ-7`)
	if len(ctx) != 1 || ctx[0] != "id: \x00" {
		t.Errorf("escaped ctx = %q", ctx)
	}
}

func TestExitAndResultOutcome(t *testing.T) {
	for text, want := range map[string]Outcome{
		"done":                         OutcomeOK,
		`{"exit_code": 0}`:             OutcomeOK,
		`{"exit_code": 2}`:             OutcomeFailed,
		"Exit code: 1\nerror":          OutcomeFailed,
		"process exited with code -1":  OutcomeFailed,
		"exit code 0 then exit code 0": OutcomeOK,
	} {
		if got := ExitOutcome(text); got != want {
			t.Errorf("ExitOutcome(%q) = %v, want %v", text, got, want)
		}
	}
	for text, want := range map[string]Outcome{
		"User cancelled MCP tool call":                        OutcomeFailed,
		"The tool use was rejected by the user":               OutcomeFailed,
		"the user doesn't want to proceed with this tool use": OutcomeFailed,
		"ok":                 OutcomeOK,
		`{"exit_code": 127}`: OutcomeFailed,
	} {
		if got := ResultOutcome(text); got != want {
			t.Errorf("ResultOutcome(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestUsageArithmetic(t *testing.T) {
	u := Usage{Fresh: 10, Cached: 20, Output: 30}
	if u.Total() != 60 {
		t.Errorf("Total = %v", u.Total())
	}
	if got := u.Add(Usage{1, 2, 3}); got != (Usage{11, 22, 33}) {
		t.Errorf("Add = %+v", got)
	}
	if got := u.Scale(0.5); got != (Usage{5, 10, 15}) {
		t.Errorf("Scale = %+v", got)
	}
}

func TestSpreadSharesOneTurn(t *testing.T) {
	calls := make([]Call, 3)
	Spread(calls, 1, 4, Usage{Fresh: 10, Output: 4})
	if calls[0].Measured || calls[0].Turn != 0 {
		t.Errorf("a call before from was changed: %+v", calls[0])
	}
	for _, c := range calls[1:] {
		if !c.Measured || c.Turn != 4 || c.Tokens != (Usage{Fresh: 5, Output: 2}) {
			t.Errorf("share = %+v", c)
		}
	}
	Spread(calls, 3, 9, Usage{Fresh: 1}) // nothing after from: no-op
	if calls[2].Turn != 4 {
		t.Errorf("Spread past the end changed a call")
	}
}

func TestRequestClassification(t *testing.T) {
	for text, want := range map[string]bool{
		"fix the build": true,
		"  ":            false,
		"<environment_context>x</environment_context>": false,
		"<command-name>/clear</command-name>":          false,
	} {
		if got := IsRequest(text); got != want {
			t.Errorf("IsRequest(%q) = %v", text, got)
		}
	}
	for text, want := range map[string]bool{
		"# AGENTS.md instructions for /repo": true,
		" [Request interrupted by user]":     true,
		"# Context from my IDE setup:\nfoo":  true,
		"please add a test":                  false,
	} {
		if got := IsHarness(text); got != want {
			t.Errorf("IsHarness(%q) = %v", text, got)
		}
	}
	for text, want := range map[string]bool{
		"yes":                        true,
		"Yes, file it.":              true,
		"ok, continue":               true,
		"LGTM!":                      true,
		"yes and also check PROJ-12": false,
		"ok fix src/main.go":         false,
		"please deploy":              false,
		"yes this is a request with far too many words": false,
		"": false,
	} {
		if got := IsAcknowledgement(text); got != want {
			t.Errorf("IsAcknowledgement(%q) = %v", text, got)
		}
	}
	if got := RequestText("# In app browser:\nctx\n## My request for Codex:\n  rename the tab "); got != "rename the tab" {
		t.Errorf("RequestText = %q", got)
	}
	if got := RequestText("plain"); got != "plain" {
		t.Errorf("RequestText = %q", got)
	}
}

func TestAddRequest(t *testing.T) {
	var s Session
	// A call before any message belongs to an empty request 0.
	s.Calls = append(s.Calls, Call{Request: s.Request()})
	s.AddRequest("rename the billing tab")
	s.AddRequest("rename the billing tab") // a retry
	s.Calls = append(s.Calls, Call{Request: s.Request()})
	s.AddRequest("yes, go ahead") // answers the agent mid-request
	s.AddRequestWithRole("# AGENTS.md instructions", "context")
	if want := []string{"", "rename the billing tab", "# AGENTS.md instructions"}; !reflect.DeepEqual(s.Requests, want) {
		t.Fatalf("Requests = %q, want %q", s.Requests, want)
	}
	if want := []string{"unknown", "user", "context"}; !reflect.DeepEqual(s.RequestRoles, want) {
		t.Errorf("RequestRoles = %q, want %q", s.RequestRoles, want)
	}
	if want := []Approval{{Request: 1, AfterCall: 2}}; !reflect.DeepEqual(s.Approvals, want) {
		t.Errorf("Approvals = %+v, want %+v", s.Approvals, want)
	}
	// Long requests are cut without splitting a character.
	var long Session
	long.AddRequest(strings.Repeat("é", 3000))
	if r := long.Requests[0]; len(r) > 4000 || !utf8.ValidString(r) {
		t.Errorf("long request: %d bytes, valid %v", len(r), utf8.ValidString(r))
	}
}

func TestTruncateUTF8(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"aé", 2, "a"},
		{"éé", 3, "é"},
		{"é", 0, ""},
	} {
		if got := TruncateUTF8(c.in, c.n); got != c.want {
			t.Errorf("TruncateUTF8(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestOutputTokens(t *testing.T) {
	got := OutputTokens("Created user alice@example.com in team core/platform. id 123456, alice@example.com again; ab")
	want := []string{"Created", "user", "alice@example.com", "team", "core/platform", "again"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OutputTokens = %q, want %q", got, want)
	}
}
