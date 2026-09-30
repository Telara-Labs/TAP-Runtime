package discover

// Regressions from the Gate D audit of advertised recommendations
// (tap-discover-review-2026-09-29/eval/GATE-D-REPORT.md). Each was a
// confirmed false positive on the frozen corpus.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// R1: a tool result that says the call was cancelled or rejected is a
// failed call, whatever exit status the client wrapped around it.
func TestR1CancelledToolCallIsAFailedOutcome(t *testing.T) {
	for _, text := range []string{
		"Wall time: 0.0260 seconds\nOutput:\n[{\"type\":\"text\",\"text\":\"user cancelled MCP tool call\"}]",
		"The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file).",
	} {
		if got := resultOutcome(text); got != OutcomeFailed {
			t.Errorf("%q: outcome %v, want failed", text[:40], got)
		}
	}
	if got := resultOutcome("Exit code: 0\nOutput:\nok"); got != OutcomeOK {
		t.Errorf("an ordinary result: %v", got)
	}
}

// R2: a procedure whose fixed arguments point into a temporary directory
// (a test scratchpad) cannot be rerun anywhere else: not a reusable
// procedure, however consistently it recurred.
func TestR2EphemeralConstantsAreNotAReusableProcedure(t *testing.T) {
	ss := eps("tmp", 6, func(i int) string {
		return "Call the MCP tool tap_run twice with the two packages. Do nothing else. Report each result verbatim."
	}, func(i int) []Call {
		return []Call{
			{Tool: "mcp:tap_run", Args: map[string]string{"package": "/private/tmp/claude-501/x/scratchpad/work/pkgs/hello-sh"}},
			{Tool: "mcp:tap_run", Args: map[string]string{"package": "/private/tmp/claude-501/x/scratchpad/work/pkgs/hello-py"}},
		}
	})
	rep := runOn(t, ss)
	for _, r := range rep.Routines {
		if r.Suitability == SuitUseful {
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
	rep := runOn(t, requestCorpus())
	prims := rep.Primitives()
	if len(prims) == 0 {
		t.Fatal("no recommended procedure to review")
	}
	d := prims[0].Draft()
	if !strings.Contains(string(d.Files["README.md"]), "**Status: unvalidated.**") || !strings.Contains(string(d.Files["primitive.yaml"]), "Unvalidated draft (never executed)") {
		t.Fatalf("README/manifest do not say unvalidated")
	}
	_, digest, err := d.Package()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	Review(strings.NewReader("\n"), &out, rep, ReviewConfig{}, ReviewActions{})
	if !strings.Contains(out.String(), "UNVALIDATED") || !strings.Contains(out.String(), "validation not run for "+digest) {
		t.Fatalf("review output:\n%s", out.String())
	}
	dir := t.TempDir()
	path, _, err := d.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path + "/" + SavedMarker)
	if !strings.Contains(string(b), `"validation": "not_run"`) {
		t.Fatalf("marker: %s", b)
	}
}
