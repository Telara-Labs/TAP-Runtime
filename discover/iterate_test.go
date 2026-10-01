package discover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/routine"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestCursorMCPEnvelopeIsUnwrapped(t *testing.T) {
	for name, raw := range map[string]string{
		"rawArgs": `{"name":"user-telara-telara_task_create","args":{"title":"t","summary":"s"},"toolCallId":"44","providerIdentifier":"telara","toolName":"telara_task_create"}`,
		"string":  `{"name":"x","args":"{\"title\":\"t\",\"summary\":\"s\"}","serverIdentifier":"user-telara"}`,
		"params":  `{"tools":[{"name":"telara_task_create","parameters":"{\"title\":\"t\",\"summary\":\"s\"}","serverName":"telara"}]}`,
	} {
		c := history.CursorCall(history.CursorRow{Name: "mcp-telara-telara_task_create", Args: raw})
		if len(c.Args) != 2 || c.Args["title"] != "t" || c.Args["summary"] != "s" {
			t.Errorf("%s: args = %v", name, c.Args)
		}
	}
	// A tool whose own argument is called "args" keeps it.
	var own map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"args":["-l"],"cmd":"ls"}`), &own)
	if got := history.CursorMCPArgs(own); len(got) != 2 {
		t.Errorf("own args argument was unwrapped: %v", got)
	}
}

func TestDashUIsACredentialOnlyForUserFlagPrograms(t *testing.T) {
	sl := trace.Slot{Key: "-u=", Type: trace.SlotText, Value: "+%Y-%m-%dT%H:%M:%SZ"}
	if redact.SensitiveSlot("sh:date", sl) {
		t.Error("date -u takes a format, not a user")
	}
	if !redact.SensitiveSlot("sh:curl", trace.Slot{Key: "-u=", Type: trace.SlotText, Value: "me:hunter2"}) {
		t.Error("curl -u user:password is a credential")
	}
}

func TestALineRepeatedInOneSessionIsNotFixed(t *testing.T) {
	// Four runs of the same programs. Two, both in one session, wrote the
	// same line; the other two sessions wrote it differently. Half the runs
	// share the line, but only one session did the work that way.
	lines := []string{
		"cd scratch && python3 remaining.py | head -5",
		"cd scratch && python3 remaining.py | head -5",
		"python3 remaining.py --all | head -20",
		"python3 other.py > out.txt; head out.txt",
	}
	ss := testkit.RequestSessions(4, func(i int) string { return "how much is left" }, func(i int) []trace.Call {
		return []trace.Call{testkit.ShellCall(lines[i]), testkit.ShellCall("git status --short")}
	})
	ss[0].AddRequest("deploy the gateway to staging")
	ss[0].AddRequest("how much is left")
	for _, c := range ss[1].Calls {
		c.Request = 2
		ss[0].Calls = append(ss[0].Calls, c)
	}
	ss = append(ss[:1], ss[2:]...)
	o := DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: ss}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rep.Routines {
		sh := string(routine.RoutineDraft(&r).Files["main.sh"])
		if strings.Contains(sh, "remaining.py") {
			t.Fatalf("a line only one session wrote was drafted as fixed:\n%s", sh)
		}
		if strings.Contains(sh, "all come from one session") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no routine drafted the line as an authoring step: %+v", rep.Funnel)
	}
}

func TestCopiedCallsAreReadOnce(t *testing.T) {
	ss := []trace.Session{
		{Client: "claude-code", ID: "a", Calls: []trace.Call{{ID: "toolu_1"}, {ID: "toolu_2"}, {}}},
		{Client: "claude-code", ID: "b", Calls: []trace.Call{{ID: "toolu_1"}, {ID: "toolu_3"}, {}}},
		{Client: "codex", ID: "c", Calls: []trace.Call{{ID: "toolu_1"}}},
	}
	trace.DropCopiedCalls(ss)
	if got := fmt.Sprint(len(ss[0].Calls), len(ss[1].Calls), len(ss[2].Calls)); got != "3 2 1" {
		t.Fatalf("calls kept = %s", got)
	}
}

func TestAnArgumentAnyRunSentAsJSONIsJSON(t *testing.T) {
	ss := testkit.RequestSessions(6, func(i int) string { return "run the action" }, func(i int) []trace.Call {
		params := fmt.Sprintf(`{"issue_key":"TENG-%d"}`, 100+i)
		c := trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"action": "jira_get_issue", "params": params}}
		if i%2 == 1 { // the first run recorded it as text
			c.RawArgs = map[string]bool{"params": true}
		}
		return []trace.Call{{Tool: "mcp:telara_tool_search", Args: map[string]string{"query": "jira issue"}}, c}
	})
	d := routine.RoutineDraft(firstRoutine(t, ss))
	var in *model.DraftInput
	for k := range d.Inputs {
		if strings.HasSuffix(d.Inputs[k].Name, "params") {
			in = &d.Inputs[k]
		}
	}
	if in == nil || !in.Raw || in.Type != "object" {
		t.Fatalf("params input = %+v", in)
	}
	if !strings.Contains(string(d.Files["main.sh"]), `json_raw "${`) {
		t.Fatalf("params must be passed as JSON:\n%s", d.Files["main.sh"])
	}
}

func TestEscapedNewlinesStartALineAndBackslashesNeverAnchor(t *testing.T) {
	_, ctx := trace.OutputRefs(`"Task created successfully.\n\n- **Task ID:** ` + "`" + `90991e90-de01-4847-a933-187b18ef2985` + "`\"")
	if len(ctx) != 1 || ctx[0] != "- **Task ID:** `\x00`" {
		t.Fatalf("ctx = %q", ctx)
	}
	for _, c := range []string{`\"id\":\"`, `a\tb: `} {
		st := func(v string) trace.Step { return trace.Step{OutIDs: []string{v}, OutCtx: []string{c + "\x00\""}} }
		d := &routine.Drafter{Occ: [][]trace.Step{{st("18c0000000000abc1")}, {st("18c0000000000abc2")}}}
		if _, _, _, ok := d.Extraction(0, map[int]string{0: "18c0000000000abc1", 1: "18c0000000000abc2"}); ok {
			t.Errorf("anchor %q was recorded escaped and must not be used", c)
		}
	}
}

func TestCodexSessionIdentityIsTheFilesOwnAndUnique(t *testing.T) {
	dir := t.TempDir()
	call := `{"timestamp":"2026-09-27T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status\"}"}}` + "\n"
	// A fork: its own meta first, then its parent's.
	fork := `{"timestamp":"2026-09-27T10:00:00Z","type":"session_meta","payload":{"id":"child"}}` + "\n" +
		`{"timestamp":"2026-09-27T10:00:00Z","type":"session_meta","payload":{"id":"parent"}}` + "\n" + call
	parent := `{"timestamp":"2026-09-27T09:00:00Z","type":"session_meta","payload":{"id":"parent"}}` + "\n" + call
	// A sub-rollout that opens with its parent's meta.
	sub := `{"timestamp":"2026-09-27T11:00:00Z","type":"session_meta","payload":{"id":"parent"}}` + "\n" + call
	os.WriteFile(filepath.Join(dir, "a-parent.jsonl"), []byte(parent), 0o600)
	os.WriteFile(filepath.Join(dir, "b-child.jsonl"), []byte(fork), 0o600)
	os.WriteFile(filepath.Join(dir, "c-parent_sub.jsonl"), []byte(sub), 0o600)
	ss, err := history.Codex{Dir: dir}.Read(time.Time{})
	if err != nil || len(ss) != 3 {
		t.Fatalf("read %d: %v", len(ss), err)
	}
	got := map[string]bool{}
	for _, s := range ss {
		if got[s.ID] {
			t.Fatalf("two sessions named %q", s.ID)
		}
		got[s.ID] = true
		for _, c := range s.Calls {
			if c.Session != s.ID {
				t.Fatalf("call names session %q, session is %q", c.Session, s.ID)
			}
		}
	}
	if !got["parent"] || !got["child"] || !got["c-parent_sub"] {
		t.Fatalf("ids = %v", got)
	}
}

func TestIncidentalStepsAreNotTheProcedureForAStatedGoal(t *testing.T) {
	// Ten requests say the same thing; three of them happened to run the
	// same two constant commands, the rest did other things.
	ss := testkit.Episodes("la", 10, func(i int) string { return "look around the repo" }, func(i int) []trace.Call {
		if i < 3 {
			return []trace.Call{testkit.ShellCall("pwd"), testkit.ShellCall("id")}
		}
		return []trace.Call{testkit.ShellCall(fmt.Sprintf("ls dir%d", i)), testkit.ShellCall(fmt.Sprintf("cat f%d.txt", i))}
	})
	rep := runOn(t, ss)
	for _, r := range rep.Routines {
		if hasStep(r, "sh:pwd") && r.Suitability == model.SuitUseful {
			t.Fatalf("3 of 10 requests ran it: not the procedure for that goal:\n%s", dump(rep))
		}
	}
}

func TestConstantScaffoldingIsNotABoundedPart(t *testing.T) {
	// Every run opens the browser the same way, then explores pages the
	// agent chose: the opening is not a procedure of its own.
	ss := testkit.Episodes("sc", 8, func(i int) string { return fmt.Sprintf("why is page %d slow", i) }, func(i int) []trace.Call {
		return []trace.Call{
			testkit.ShellCall("pwd"), testkit.ShellCall("git status --short"),
			{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("web/p%d/page.tsx", i*7)}},
			{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("web/p%d/layout.tsx", i*5)}},
		}
	})
	rep := runOn(t, ss)
	for _, r := range rep.Routines {
		if r.Parent != "" {
			t.Fatalf("constant scaffolding reported as a bounded procedure:\n%s", dump(rep))
		}
	}
}

func TestConstantOpeningOfVaryingWorkIsNotAProcedure(t *testing.T) {
	// A routine found in stated, consistent runs whose steps take nothing a
	// caller could vary, and are 30% of what those requests did: the
	// constant opening of varying work (open the browser, print the dir).
	mk := func(coverage float64) model.Routine {
		return model.Routine{SourceRole: model.RoleScheduled, Consistency: 1, Coverage: coverage,
			Contract: model.Contract{Goal: model.GoalStated, Effect: model.EffectReadOnly}}
	}
	low := mk(0.3)
	routine.Decide(&low, &model.Draft{}, nil, 0, 0)
	if low.Suitability == model.SuitUseful || routine.FirstReason(low) != "constant_part_of_larger_work" {
		t.Fatalf("suitability %q reasons %v", low.Suitability, low.Reasons)
	}
	// The same constant steps that ARE the work (a scheduled fetch and log)
	// stay a procedure.
	high := mk(1)
	routine.Decide(&high, &model.Draft{}, nil, 0, 0)
	if high.Suitability != model.SuitUseful {
		t.Fatalf("a constant procedure that is the whole task: %q %v", high.Suitability, high.Reasons)
	}
}

func TestValuesComposedFromTheRequest(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.0..v1.2.1":                true,  // a range of two requested tags
		"involvedObject.name=billing-4": true,  // a selector around a requested name
		"src/billing/init_4.go":         false, // a file the agent picked under a named area
		"needle42=billing-4":            false, // an unrequested value beside a requested one
		"billing-4":                     false, // not composed (inRequest handles it)
	} {
		if got := routine.ComposedFromRequest(v, "why is deployment billing-4 on v1.2.0 not v1.2.1"); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}
}
