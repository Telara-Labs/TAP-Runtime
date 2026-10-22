package history

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestClaudeSubagentTranscriptsAreLinkedToTheirParent(t *testing.T) {
	r := ClaudeCode{Dir: "testdata/conversation/claude"}
	read := func(each func(func(trace.Session) error) error) map[string]trace.Session {
		out := map[string]trace.Session{}
		if err := each(func(s trace.Session) error { out[s.ID] = s; return nil }); err != nil {
			t.Fatal(err)
		}
		return out
	}
	ids := func(m map[string]trace.Session) []string {
		var out []string
		for id := range m {
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		name string
		got  map[string]trace.Session
		want []string
	}{
		{"plain", read(func(y func(trace.Session) error) error { return r.Each(time.Time{}, y) }), []string{"c1"}},
		{"options without subagents", read(func(y func(trace.Session) error) error { return r.EachWith(time.Time{}, ReadOptions{}, y) }), []string{"c1"}},
		{"subagents", read(func(y func(trace.Session) error) error {
			return r.EachWith(time.Time{}, ReadOptions{Subagents: true, Conversation: true}, y)
		}), []string{"agent-a1", "c1"}},
	}
	for _, tc := range cases {
		if got := ids(tc.got); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: sessions %v, want %v", tc.name, got, tc.want)
		}
	}
	all := cases[2].got
	sub := all["agent-a1"]
	if want := (&trace.SessionLink{Relation: "subagent", Session: "c1"}); !reflect.DeepEqual(sub.Parent, want) {
		t.Errorf("parent = %+v, want %+v", sub.Parent, want)
	}
	if all["c1"].Parent != nil {
		t.Errorf("the parent session has a parent: %+v", all["c1"].Parent)
	}
	if len(sub.Calls) != 1 || sub.Calls[0].Session != "agent-a1" || sub.Calls[0].Outcome == trace.OutcomeFailed {
		t.Errorf("subagent calls = %+v", sub.Calls)
	}
	if len(sub.Assistant) != 1 || sub.Assistant[0].Text != "foo is imported in main.go." || !sub.Assistant[0].Final {
		t.Errorf("subagent prose = %+v", sub.Assistant)
	}
}
