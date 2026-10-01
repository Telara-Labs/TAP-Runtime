package integration

// Regressions from the Gate D audit of advertised recommendations
// (tap-discover-review-2026-09-29/eval/GATE-D-REPORT.md). Each was a
// confirmed false positive on the frozen corpus.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/routine"

	"gitlab.com/telara-labs/tap-runtime/discover/pack"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// R1: a tool result that says the call was cancelled or rejected is a
// failed call, whatever exit status the client wrapped around it.
func TestR1CancelledToolCallIsAFailedOutcome(t *testing.T) {
	for _, text := range []string{
		"Wall time: 0.0260 seconds\nOutput:\n[{\"type\":\"text\",\"text\":\"user cancelled MCP tool call\"}]",
		"The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file).",
	} {
		if got := trace.ResultOutcome(text); got != trace.OutcomeFailed {
			t.Errorf("%q: outcome %v, want failed", text[:40], got)
		}
	}
	if got := trace.ResultOutcome("Exit code: 0\nOutput:\nok"); got != trace.OutcomeOK {
		t.Errorf("an ordinary result: %v", got)
	}
}

// R2: a procedure whose fixed arguments point into a temporary directory
// (a test scratchpad) cannot be rerun anywhere else: not a reusable
// procedure, however consistently it recurred.
func TestR2EphemeralConstantsAreNotAReusableProcedure(t *testing.T) {
	ss := testkit.Episodes("tmp", 6, func(i int) string {
		return "Call the MCP tool tap_run twice with the two packages. Do nothing else. Report each result verbatim."
	}, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "mcp:tap_run", Args: map[string]string{"package": "/private/tmp/claude-501/x/scratchpad/work/pkgs/hello-sh"}},
			{Tool: "mcp:tap_run", Args: map[string]string{"package": "/private/tmp/claude-501/x/scratchpad/work/pkgs/hello-py"}},
		}
	})
	rep := runOn(t, ss)
	for _, r := range rep.Routines {
		if r.Suitability == model.SuitUseful {
			t.Fatalf("a procedure fixed to a scratch directory was recommended:\n%s", dump(rep))
		}
		if !strings.Contains(strings.Join(r.Reasons, " "), "ephemeral_constant") {
			t.Fatalf("reason %v", r.Reasons)
		}
	}
	if len(rep.Routines) == 0 {
		t.Fatal(fmt.Sprint("no routine: the case did not exercise the rule"))
	}
}

// An unvalidated draft says so wherever it surfaces: the manifest and
// README, the review list (with the digest that was not validated) and the
// saved folder's marker.
func TestUnvalidatedDraftsSayUnvalidated(t *testing.T) {
	rep := runOn(t, testkit.RequestCorpus())
	prims := routine.ReportPrimitives(rep)
	if len(prims) == 0 {
		t.Fatal("no recommended procedure to review")
	}
	d := routine.RoutineDraft(prims[0])
	if !strings.Contains(string(d.Files["README.md"]), "**Status: unvalidated.**") || !strings.Contains(string(d.Files["primitive.yaml"]), "Unvalidated draft (never executed)") {
		t.Fatalf("README/manifest do not say unvalidated")
	}
	_, digest, err := pack.PackageDraft(d)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	routine.Review(strings.NewReader("\n"), &out, rep, routine.ReviewConfig{}, routine.ReviewActions{})
	if !strings.Contains(out.String(), "UNVALIDATED") || !strings.Contains(out.String(), "validation not run for "+digest) {
		t.Fatalf("review output:\n%s", out.String())
	}
	dir := t.TempDir()
	path, _, err := pack.SaveDraft(d, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path + "/" + pack.SavedMarker)
	if !strings.Contains(string(b), `"validation": "not_run"`) {
		t.Fatalf("marker: %s", b)
	}
}

// R3: requests that all did the same thing but were split into several
// groups (their incidental calls differ) are still how that goal is done:
// the goal-share check counts every request with the text that ran the
// steps, not only the ones in this group.
func TestR3FragmentedGroupsStillShareTheirGoal(t *testing.T) {
	noise := []string{"ls", "pwd", "date", "uptime", "hostname"}
	ss := testkit.Episodes("mv", 20, func(i int) string { return fmt.Sprintf("move TENG-%d to done", 3200+i) }, func(i int) []trace.Call {
		k := fmt.Sprintf("TENG-%d", 3200+i)
		return []trace.Call{
			testkit.ShellCall(noise[i%len(noise)]), testkit.ShellCall(noise[(i/len(noise))%len(noise)] + " -a"),
			{Tool: "mcp:telara_jira_transition_issue", Args: map[string]string{"issue_key": k, "transition_id": "21"}},
			{Tool: "mcp:telara_jira_add_comment", Args: map[string]string{"issue_key": k, "body": "done"}},
		}
	})
	rep := runOn(t, ss)
	if len(routine.ReportPrimitives(rep)) == 0 {
		t.Fatalf("every request ran transition then comment; none recommended:\n%s", dump(rep))
	}
}

// R5: the guest runtime's wc and head read only standard input; a draft
// that passes them a file would print a count of nothing. Such a draft is
// not structurally complete.
func TestR5GuestBuiltinsThatIgnoreFilesBlockTheDraft(t *testing.T) {
	ss := testkit.Episodes("wc", 9, func(i int) string {
		var fs []string
		for f := 0; f < 2+i%3; f++ {
			fs = append(fs, fmt.Sprintf("logs/r%d-%d.txt", i, f))
		}
		return "count lines in " + strings.Join(fs, " ")
	}, func(i int) []trace.Call {
		var cs []trace.Call
		for f := 0; f < 2+i%3; f++ {
			cs = append(cs, testkit.ShellCall(fmt.Sprintf("wc -l logs/r%d-%d.txt", i, f)))
		}
		return cs
	})
	r := runOn(t, ss).Routines[0]
	if r.Suitability != model.SuitUseful || r.DraftStatus == model.DraftComplete || !strings.Contains(strings.Join(r.Blockers, " "), "guest_builtin_ignores_files") {
		t.Fatalf("suit %q draft %q blockers %v", r.Suitability, r.DraftStatus, r.Blockers)
	}
}

// R6: a file the program reads must be declared for the host to allow it;
// a fixed path is declared, a varying one blocks the draft.
func TestR6FileReadsAreDeclaredOrBlocked(t *testing.T) {
	fixed := testkit.Episodes("rf", 6, func(i int) string { return fmt.Sprintf("summarize release %d", i) }, func(i int) []trace.Call {
		return []trace.Call{{Tool: "Read", Args: map[string]string{"file_path": "docs/RELEASES.md"}}, testkit.ShellCall(fmt.Sprintf("git log --oneline v%d..HEAD", i))}
	})
	d := routine.RoutineDraft(&runOn(t, fixed).Routines[0])
	if !strings.Contains(string(d.Files["primitive.yaml"]), "path: docs/RELEASES.md") {
		t.Fatalf("a fixed read must be declared:\n%s", d.Files["primitive.yaml"])
	}
	varying := testkit.Episodes("rv", 6, func(i int) string { return fmt.Sprintf("summarize notes/day%d.md", i) }, func(i int) []trace.Call {
		return []trace.Call{{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("notes/day%d.md", i)}}, testkit.ShellCall("git status --short")}
	})
	r := runOn(t, varying).Routines[0]
	if r.DraftStatus == model.DraftComplete || !strings.Contains(strings.Join(r.Blockers, " "), "file_access_undeclared") {
		t.Fatalf("draft %q blockers %v", r.DraftStatus, r.Blockers)
	}
}

// R7: the runner binds a capability written provider.resource.verb and
// refuses anything else at admission, even when publish checks pass.
func TestR7CapabilityLabelsAreProviderResourceVerb(t *testing.T) {
	for tool, want := range map[string]string{
		"gmail_search_emails":     "gmail.emails.search",
		"records_create":          "records.records.create",
		"telara_jira_add_comment": "telara.jira_comment.add",
		"ci_list_build_steps":     "ci.build_steps.list",
		"tap_run":                 "tap.tap.run",
		"weird":                   "weird.weird.run",
	} {
		if got := routine.CapName(tool); got != want {
			t.Errorf("%s: %s, want %s", tool, got, want)
		}
		if parts := strings.Split(routine.CapName(tool), "."); len(parts) != 3 {
			t.Errorf("%s: %s is not provider.resource.verb", tool, routine.CapName(tool))
		}
	}
}
