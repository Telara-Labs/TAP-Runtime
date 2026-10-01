package discover

import (
	"fmt"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/eval"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// A new holdout must share no lineage and no template with an earlier
// sample: a scheduled prompt that differs only in its run stamp is the same
// template, and must not appear on both sides.
func TestSampleHoldoutExcludesEarlierLineagesAndTemplates(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	var ss []trace.Session
	add := func(id, req string) {
		ss = append(ss, trace.Session{Client: "claude-code", ID: id, Start: t0, Requests: []string{req},
			Calls: []trace.Call{{Tool: "shell", Command: "ls", Time: t0}, {Tool: "shell", Command: "pwd", Time: t0}}})
	}
	add("old", "Automation: hourly monitor. Last run: 2026-09-01T09:00Z. Check the queue and record the pass.")
	add("same-template", "Automation: hourly monitor. Last run: 2026-09-02T11:00Z. Check the queue and record the pass.")
	for i := 0; i < 20; i++ {
		add(fmt.Sprintf("other-%02d", i), fmt.Sprintf("please look at issue number %c and tell me what is wrong with it", 'a'+i))
	}
	c := eval.NewCorpus(ss)
	o := eval.HoldoutOptions{Seed: 7, N: 10, Exclude: []eval.EpisodeKey{{Client: "claude-code", Session: "old", Request: 0}}}
	got := eval.SampleHoldout(c, o)
	if len(got) != 10 {
		t.Fatalf("drew %d, want 10", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		if e.Session == "old" || e.Session == "same-template" {
			t.Errorf("drew %s, which shares the excluded episode's template", e.Session)
		}
		if seen[e.Session] {
			t.Errorf("drew session %s twice", e.Session)
		}
		seen[e.Session] = true
	}
	again := eval.SampleHoldout(c, o)
	for i := range got {
		if got[i].ID != again[i].ID {
			t.Fatal("the same seed must draw the same holdout")
		}
	}
}

// A frozen reader drops, and names, a session that changed since the freeze
// only when asked to; otherwise the comparison fails.
func TestFrozenReaderDropsChangedSessionsOnlyWhenAsked(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	a := trace.Session{Client: "claude-code", ID: "a", Start: t0, Calls: []trace.Call{{Tool: "shell", Command: "ls"}}}
	b := trace.Session{Client: "claude-code", ID: "b", Start: t0, Calls: []trace.Call{{Tool: "shell", Command: "pwd"}}}
	m := history.BuildManifest([]trace.Session{a, b}, t0.Add(time.Hour))
	b.Calls = append(b.Calls, trace.Call{Tool: "shell", Command: "date"}) // appended after the freeze
	inner := testkit.FakeReader{Sessions: []trace.Session{a, b}, Name: "claude-code"}
	if _, err := (history.FrozenReader{Inner: inner, Manifest: m}).Read(time.Time{}); err == nil {
		t.Fatal("a changed session must fail the read by default")
	}
	var dropped []string
	got, err := history.FrozenReader{Inner: inner, Manifest: m, DropChanged: true, Dropped: &dropped}.Read(time.Time{})
	if err != nil || len(got) != 1 || got[0].ID != "a" || len(dropped) != 1 {
		t.Fatalf("got %v %v, dropped %v", got, err, dropped)
	}
}
