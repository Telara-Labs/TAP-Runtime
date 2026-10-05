package primitive

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// issueFlows is runs of create then the given follow-ups, n runs each.
func issueFlows(n int, follow ...string) []trace.Session {
	var ss []trace.Session
	k := 0
	for _, fu := range follow {
		for i := 0; i < n; i++ {
			k++
			key := fmt.Sprintf("KEY-%d0", k)
			ss = append(ss, session(fmt.Sprint("s", k), []string{"file it"},
				call("mcp:issue_create", map[string]string{"summary": "a b"}, `{"key":"`+key+`"}`, 0, 0),
				call("mcp:"+fu, map[string]string{"issue_key": key, "note": "x y"}, `{"ok":true}`, 0, time.Second)))
		}
	}
	return ss
}

func TestDeclinedStaysHiddenUntilSomethingNewAppears(t *testing.T) {
	dir := t.TempDir()
	first := Discover(issueFlows(2, "issue_comment", "issue_transition"), nil)
	if len(first.Families) != 1 {
		t.Fatalf("fixture: %d families", len(first.Families))
	}
	if err := appendLedger(dir, entryFor(first.Families[0], "deny")); err != nil {
		t.Fatal(err)
	}
	shown, hidden := Triage(Discover(issueFlows(3, "issue_comment", "issue_transition"), nil), LoadLedger(dir))
	if len(shown) != 0 || hidden["deny"] != 1 {
		t.Fatalf("declined family came back with more runs: shown %d hidden %v", len(shown), hidden)
	}
	shown, _ = Triage(Discover(issueFlows(2, "issue_comment", "issue_transition", "issue_link"), nil), LoadLedger(dir))
	if len(shown) != 1 || shown[0].Status != StatusNewSince || shown[0].Earlier != "deny" || len(shown[0].FollowUps) != 1 || shown[0].FollowUps[0].Steps[0] != "mcp:issue_link" {
		t.Fatalf("new follow-up not shown alone: %+v", shown)
	}
}

func TestRulesChangeShowsADecisionAgain(t *testing.T) {
	dir := t.TempDir()
	res := Discover(issueFlows(2, "issue_comment"), nil)
	e := entryFor(res.Families[0], "accept")
	e.Rules = "older-rules"
	if err := appendLedger(dir, e); err != nil {
		t.Fatal(err)
	}
	shown, _ := Triage(res, LoadLedger(dir))
	if len(shown) != 1 || shown[0].Status != StatusReevaluated {
		t.Fatalf("stale decision not re-shown: %+v", shown)
	}
}

func TestUndoProposesAgainAndATornLineIsSkipped(t *testing.T) {
	dir := t.TempDir()
	res := Discover(issueFlows(2, "issue_comment"), nil)
	if err := appendLedger(dir, entryFor(res.Families[0], "accept")); err != nil {
		t.Fatal(err)
	}
	// A crash mid-write leaves a partial last line.
	f, _ := os.OpenFile(ledgerPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"fingerprint":"mcp:x|read","follow`)
	f.Close()
	if shown, hidden := Triage(res, LoadLedger(dir)); len(shown) != 0 || hidden["accept"] != 1 {
		t.Fatalf("accepted family re-asked: %d shown", len(shown))
	}
	// The next decision is appended after the torn line, not onto it.
	var out bytes.Buffer
	if err := Revisit(strings.NewReader("1 u\nq\n"), &out, dir, false); err != nil {
		t.Fatal(err)
	}
	if shown, _ := Triage(res, LoadLedger(dir)); len(shown) != 1 || shown[0].Status != StatusNew {
		t.Fatalf("undo did not propose it again:\n%s", out.String())
	}
}
