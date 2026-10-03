package bind

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type fixtureTool struct {
	Server    string `json:"server"`
	Name      string `json:"name"`
	Annotated Effect `json:"annotated"`
}

// load reads an inventory recorded from a real client on 2026-09-28: tool
// names and the effect each server annotates, nothing else.
func load(t *testing.T, client string) []Tool {
	t.Helper()
	b, err := os.ReadFile("testdata/inventory-" + client + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var raw []fixtureTool
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	out := make([]Tool, len(raw))
	for i, r := range raw {
		out[i] = Tool{Server: r.Server, Name: r.Name, Annotated: r.Annotated}
	}
	return out
}

type want struct {
	capability string
	declared   Effect
	tool       string // "" means the runner must refuse
	gated      bool
}

var claudeCases = []want{
	{"gmail.threads.search", Read, "search_threads", false},
	{"gmail.threads.get", Read, "get_thread", false},
	{"gmail.messages.get", Read, "get_message", false},
	{"gmail.drafts.list", Read, "list_drafts", false},
	{"gmail.drafts.get", Read, "get_draft", false},
	{"gmail.labels.list", Read, "list_labels", false},
	{"gmail.drafts.create", Write, "create_draft", true},
	{"gmail.drafts.delete", Write, "delete_draft", false}, // an effectful annotation promotes the approval gate
	{"gmail.drafts.update", Write, "update_draft", true},
	{"gmail.labels.create", Write, "create_label", true},
	{"gmail.messages.send", Write, "send_message", true},
	{"gmail.drafts.delete", Destructive, "delete_draft", false},
	{"gmail.threads.trash", Destructive, "trash_thread", false},
	{"calendar.events.list", Read, "list_events", false},
	{"calendar.events.search", Read, "search_events", false},
	{"calendar.events.get", Read, "get_event", false},
	{"calendar.calendars.list", Read, "list_calendars", false},
	{"calendar.events.create", Write, "create_event", true},
	{"calendar.events.delete", Destructive, "delete_event", false},
	{"google_drive.files.search", Read, "search_files", false},
	{"google_drive.files.create", Write, "create_file", true},
	{"google_drive.files.trash", Destructive, "trash_file", false},
	{"jira.issues.search", Read, "telara_jira_search_issues", false},
	{"jira.issues.create", Write, "telara_jira_create_issue", true},

	// Must refuse.
	{"gmail.messages.search", Read, "", false},  // this connector searches threads only
	{"slack.messages.send", Write, "", false},   // no such connector
	{"gmail.drafts.delete", Read, "", false},    // declared read, the tool is destructive
	{"gmail.threads.archive", Write, "", false}, // no such action
	{"calendar.events.delete", Read, "", false}, // declared read, the tool is destructive
}

var codexCases = []want{
	{"gmail.emails.search", Read, "gmail.search_emails", false},
	{"gmail.threads.get", Read, "gmail.read_email_thread", false},
	{"gmail.emails.get", Read, "gmail.read_email", false},
	{"gmail.drafts.list", Read, "gmail.list_drafts", false},
	{"gmail.labels.list", Read, "gmail.list_labels", false},
	{"gmail.drafts.create", Write, "gmail.create_draft", false},
	{"gmail.emails.send", Write, "gmail.send_email", false},
	{"gmail.emails.delete", Destructive, "gmail.delete_emails", false},
	{"google_drive.files.search", Read, "google_drive.search", false},
	{"google_drive.files.create", Write, "google_drive.create_file", false},

	// Must refuse.
	{"gmail.threads.search", Read, "", false}, // section 4.1.1: this connector searches messages
	{"calendar.events.list", Read, "", false}, // no calendar connector installed
	{"slack.messages.send", Write, "", false},
	{"gmail.emails.send", Read, "", false}, // declared read, the tool writes
}

func run(t *testing.T, client string, cases []want) {
	inv := load(t, client)
	for _, c := range cases {
		t.Run(c.capability+"/"+string(c.declared), func(t *testing.T) {
			got := Resolve(c.capability, c.declared, inv)
			name := ""
			if got.Bound != nil {
				name = got.Bound.Name
			}
			ru := ""
			if got.RunnerUp != nil {
				ru = got.RunnerUp.Name
			}
			t.Logf("%-28s %-11s -> %-28q %.3f   runner-up %q %.3f   %s", c.capability, c.declared, name, got.Score, ru, got.RunnerUpScore, got.Refused)
			if name != c.tool {
				t.Fatalf("bound %q, want %q (refused: %q)", name, c.tool, got.Refused)
			}
			if got.Bound != nil && got.Gated != c.gated {
				t.Fatalf("gated = %v, want %v", got.Gated, c.gated)
			}
		})
	}
}

func TestResolveAgainstClaudeCodeInventory(t *testing.T) { run(t, "claude", claudeCases) }
func TestResolveAgainstCodexInventory(t *testing.T)      { run(t, "codex", codexCases) }

// The order a client lists its tools in must never change the choice.
func TestResolveIgnoresInventoryOrder(t *testing.T) {
	inv := load(t, "claude")
	rev := make([]Tool, len(inv))
	for i, x := range inv {
		rev[len(inv)-1-i] = x
	}
	for _, c := range claudeCases {
		a, b := Resolve(c.capability, c.declared, inv), Resolve(c.capability, c.declared, rev)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: choice depends on inventory order: %+v vs %+v", c.capability, a.Bound, b.Bound)
		}
	}
}

func TestEqualScoresBreakDeterministically(t *testing.T) {
	inv := []Tool{
		{Server: "b gmail", Name: "search_threads", Annotated: Read},
		{Server: "a gmail", Name: "search_threads", Annotated: Read},
	}
	got := Resolve("gmail.threads.search", Read, inv)
	if got.Bound == nil || got.Bound.Server != "a gmail" {
		t.Fatalf("want the server first in byte order, got %+v", got.Bound)
	}
	if got.RunnerUp == nil || got.RunnerUpScore != got.Score {
		t.Fatalf("the receipt must carry the equal runner-up, got %+v", got.RunnerUp)
	}
}

func TestTokens(t *testing.T) {
	cases := map[string][]string{
		"gmail.threads.search":      {"gmail", "thread", "search"},
		"search_threads":            {"search", "thread"},
		"claude.ai Google Drive":    {"claude", "ai", "google", "drive"},
		"getFileMetadata":           {"get", "file", "metadata"},
		"gmail.read_email_thread":   {"gmail", "get", "email", "thread"},
		"telara_jira_search_issues": {"telara", "jira", "search", "issue"},
		"list_entries":              {"list", "entry"},
		"find_class":                {"search", "class"},
	}
	for in, want := range cases {
		if got := Tokens(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokens(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMalformedCapabilityIsRefused(t *testing.T) {
	for _, c := range []string{"", "gmail", "...", "gmail."} {
		if got := Resolve(c, Read, []Tool{{Server: "gmail", Name: "search", Annotated: Read}}); got.Bound != nil {
			t.Errorf("%q bound to %v", c, got.Bound)
		}
	}
}
