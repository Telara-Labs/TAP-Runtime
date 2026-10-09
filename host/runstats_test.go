package main

import (
	"strings"
	"testing"
	"time"
)

func TestInputItemsCountsTheLongestListOrOne(t *testing.T) {
	for _, c := range []struct {
		args []string
		want int
	}{
		{nil, 1},
		{[]string{`{"browser":"chrome","recipients":[{"name":"a"},{"name":"b"},{"name":"c"}],"tags":["x"]}`}, 3},
		{[]string{`{"candidate":"abc"}`}, 1},
		{[]string{`{"items":[]}`}, 1},
		{[]string{"one", "two"}, 2},
		{[]string{"not json"}, 1},
	} {
		if got := inputItems(c.args); got != c.want {
			t.Errorf("inputItems(%q) = %d, want %d", c.args, got, c.want)
		}
	}
}

// The hint must stay quiet on small runs and on runs whose calls track their
// inputs, and speak on a run that walks a whole list per item.
func TestImproveHintFiresOnlyOnCallHeavyRuns(t *testing.T) {
	for _, c := range []struct {
		name          string
		calls, inputs int
		want          bool
	}{
		{"tiny run, high ratio", 12, 1, false},
		{"just under the floor", slowCallFloor - 1, 1, false},
		{"search per item, about 10 calls each", 100, 10, false},
		{"exactly the per-item limit", 200, 10, false},
		{"one item walked a long list", 150, 1, true},
		{"five items, walk repeated per absent name", 243, 5, true},
		{"many items, few calls each", 400, 40, false},
	} {
		got := improveHint(c.calls, c.inputs, "") != ""
		if got != c.want {
			t.Errorf("%s: improveHint(%d, %d) fired=%v, want %v", c.name, c.calls, c.inputs, got, c.want)
		}
	}
}

func TestRunStatsTextReportsCostAndNamesTheAuthorsFix(t *testing.T) {
	res := &Result{RunID: "20261009T125508Z-e9a69550", Calls: 243, FailedCalls: 2, Refused: 1, Replayed: 3, Inputs: 5, Elapsed: 333*time.Second + 40*time.Millisecond}
	text := runStatsText(res)
	for _, want := range []string{
		"[stats: 243 tool call(s), 2 failed, 1 refused, 5 input item(s), 5m33s, 3 answered from the record]",
		"243 tool calls for 5 input item(s), about 49 per item",
		"author can publish a faster version",
		"without reporting anything unresolved as found",
		"tap_evidence on run 20261009T125508Z-e9a69550",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("stats text lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "\n") != 2 {
		t.Errorf("want the stats line and one hint line:\n%s", text)
	}

	quiet := runStatsText(&Result{Calls: 3, Inputs: 0, Elapsed: 2 * time.Second})
	if quiet != "\n[stats: 3 tool call(s), 0 failed, 0 refused, 1 input item(s), 2s]" {
		t.Errorf("small run = %q", quiet)
	}
}
