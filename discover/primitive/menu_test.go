package primitive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// twoFamilies is a corpus with two proposed primitives.
func twoFamilies() Result {
	var ss []trace.Session
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("KEY-%d8", i)
		ss = append(ss, session(fmt.Sprint("a", i), []string{"file it"},
			call("mcp:issue_create", map[string]string{"summary": "a b"}, `{"key":"`+key+`"}`, 0, 0),
			call("mcp:issue_transition", map[string]string{"issue_key": key, "to": "doing"}, `{"ok":true}`, 0, time.Second)))
		id := fmt.Sprintf("9d3c1a2b-0000-4000-8000-00000000006%d", i)
		ss = append(ss, session(fmt.Sprint("b", i), []string{"start"},
			call("mcp:task_create", map[string]string{"goal": "ship it"}, `{"task_id":"`+id+`"}`, 0, 0),
			call("mcp:task_checkpoint", map[string]string{"task_id": id, "milestone": "planned"}, `{"ok":true}`, 0, time.Second)))
	}
	return Discover(ss, nil)
}

func runMenu(t *testing.T, res Result, input string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Menu(strings.NewReader(input), &out, res, MenuConfig{StateDir: dir, Home: t.TempDir(), Clients: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	return out.String(), dir
}

func TestMenuStartsWithSummaryAndQuitSavesNothing(t *testing.T) {
	res := twoFamilies()
	if len(res.Families) != 2 {
		t.Fatalf("fixture has %d families", len(res.Families))
	}
	out, dir := runMenu(t, res, "i\na\nq\n")
	if !strings.Contains(out, "Summary") || strings.Index(out, "Summary") > strings.Index(out, "Proposed primitives") || !strings.Contains(out, "Used 2 times") || !strings.Contains(out, "What it does") || !strings.Contains(out, "Potential savings") || !strings.Contains(out, "Needs attention") {
		t.Fatalf("card or summary missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "decisions.jsonl")); err == nil {
		t.Fatal("quitting before submit wrote decisions")
	}
}

func TestInspectPreviousChangesAChoiceBeforeSubmit(t *testing.T) {
	res := twoFamilies()
	// Accept #1, deny #2 (that opens the review), go back to #2, change it
	// to accept, review again, submit.
	out, dir := runMenu(t, res, "i\na\nd\nb\na\ns\n")
	if !strings.Contains(out, "Installed (2)") {
		t.Fatalf("not submitted:\n%s", out)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "accepted", "families", "*.json"))
	if len(files) != 2 {
		t.Fatalf("want both accepted after changing #2, got %d:\n%s", len(files), out)
	}
}

func TestApproveAllGoesThroughReview(t *testing.T) {
	res := twoFamilies()
	out, dir := runMenu(t, res, "a\ns\n")
	files, _ := filepath.Glob(filepath.Join(dir, "accepted", "families", "*.json"))
	if !strings.Contains(out, "Review") || len(files) != 2 {
		t.Fatalf("approve all: %d accepted\n%s", len(files), out)
	}
}

func TestFailedInstallDoesNotAcceptCandidate(t *testing.T) {
	res := twoFamilies()
	dir := t.TempDir()
	var out bytes.Buffer
	err := Menu(strings.NewReader("a\ns\n"), &out, res, MenuConfig{StateDir: dir, Home: t.TempDir(), Clients: "claude-code",
		Install: func(Family, []Primitive) (InstallResult, error) {
			return InstallResult{Reason: "branch rule unknown"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "accepted", "families", "*.json"))
	if len(files) != 0 || !strings.Contains(out.String(), "branch rule unknown") {
		t.Fatalf("failed install recorded as accepted: %d files, output %s", len(files), out.String())
	}
}

func TestUnresolvedPatternIsShownButCannotBeAccepted(t *testing.T) {
	res := twoFamilies()
	res.Families[0].APIMode = "needs_refinement"
	res.Families[0].APIReason = "no branch predicate"
	res.Families[0].Saved = trace.Usage{Fresh: 1234}
	res.Families[0].FollowUps[0].PotentialTokens = 1234
	res.Families[0].FollowUps[0].APIMode = "needs_refinement"
	res.Families[0].FollowUps[0].APIReason = "one recorded path needs a decision"
	var out bytes.Buffer
	dir := t.TempDir()
	err := Menu(strings.NewReader("i\na\nq\n"), &out, res, MenuConfig{StateDir: dir, Home: t.TempDir(), Clients: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "No executable API established") || !strings.Contains(got, "no branch predicate") || !strings.Contains(got, "Choose agent eval or continue") ||
		!strings.Contains(got, "~1.2k potential tokens") || !strings.Contains(got, "one recorded path needs a decision") {
		t.Fatalf("unresolved contract was hidden or accept was allowed: %s", got)
	}
	var listing bytes.Buffer
	listTable(&listing, style{width: 140}, res.Families, make([]string, len(res.Families)), -1)
	if !strings.Contains(listing.String(), "~1.2k") || strings.Contains(listing.String(), "│      —") {
		t.Fatalf("unresolved opportunity disappeared from list: %s", listing.String())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "accepted", "families", "*.json"))
	if len(files) != 0 {
		t.Fatalf("accepted %d unresolved patterns", len(files))
	}
}
