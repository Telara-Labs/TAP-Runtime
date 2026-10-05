package trace_test

import (
	"fmt"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// The choice rule reads the corpus only: the same argument name is a choice
// in one corpus and data in another.
func TestChoiceIsDecidedByEvidenceNotNames(t *testing.T) {
	call := func(v string) trace.Call {
		return trace.Call{Tool: "mcp:tool", Args: map[string]string{"action": v}}
	}
	session := func(id, request string, vals ...string) trace.Session {
		s := trace.Session{Client: "c", ID: id, Requests: []string{request}}
		for _, v := range vals {
			s.Calls = append(s.Calls, call(v))
		}
		return s
	}
	cases := []struct {
		name   string
		ss     []trace.Session
		choice bool
	}{
		{"few values reused", func() []trace.Session {
			var ss []trace.Session
			for i := 0; i < 9; i++ {
				ss = append(ss, session(fmt.Sprint(i), "do it", []string{"get", "add", "close"}[i%3]))
			}
			return ss
		}(), true},
		{"caller supplied each value", []trace.Session{session("a", "deploy to prod", "prod"), session("b", "deploy to staging", "staging")}, false},
		{"a new value every run", []trace.Session{session("a", "x", "one"), session("b", "x", "two"), session("c", "x", "three")}, false},
		{"too little evidence splits", []trace.Session{session("a", "x", "one"), session("b", "x", "two")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := trace.NewChoices(tc.ss)
			if got := ch.Selector(tc.ss[0].Calls[0], "action"); got != tc.choice {
				t.Fatalf("choice=%v, want %v", got, tc.choice)
			}
		})
	}
	var none *trace.Choices
	if !none.Selector(call("anything"), "action") {
		t.Fatal("without evidence a plain word must stay a choice")
	}
}

// A shell first word is a subcommand only when the program used it in two
// or more sessions; a one-off word (a search pattern) is data.
func TestShellSubcommandNeedsRecurrence(t *testing.T) {
	sh := func(id, cmd string) trace.Session {
		return trace.Session{Client: "c", ID: id, Requests: []string{"x"}, Calls: []trace.Call{{Tool: "shell", Command: cmd}}}
	}
	ss := []trace.Session{sh("a", "git status"), sh("b", "git status"), sh("c", "grep alpha f.txt"), sh("d", "grep beta f.txt")}
	ch := trace.NewChoices(ss)
	if !ch.Selector(ss[0].Calls[0], "argv_0") {
		t.Fatal("a recurring subcommand is part of the operation")
	}
	if ch.Selector(ss[2].Calls[0], "argv_0") {
		t.Fatal("a one-off pattern is data")
	}
}
