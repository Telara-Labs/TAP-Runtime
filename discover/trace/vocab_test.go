package trace

import (
	"reflect"
	"strings"
	"testing"
)

func TestStepEffect(t *testing.T) {
	slot := func(k, v string) Slot { return Slot{Key: k, Type: SlotText, Value: v} }
	for _, c := range []struct {
		st   Step
		want string
	}{
		{Step{Label: "Read"}, "read"},
		{Step{Label: "WebFetch"}, "read"},
		{Step{Label: "Edit"}, "write"},
		{Step{Label: "patch:apply"}, "write"},
		{Step{Label: "sh:cat"}, "read"},
		{Step{Label: "sh:rm"}, "write"},
		{Step{Label: "sh:cat", Compound: true, Raw: "cat a > b"}, "write"},
		{Step{Label: "sh:cat", Compound: true, Raw: "cat a 2>/dev/null"}, "read"},
		{Step{Label: "sh:sed", Slots: []Slot{slot("1", "-i.bak")}}, "write"},
		{Step{Label: "sh:sed", Slots: []Slot{slot("1", "s/a/b/")}}, "read"},
		{Step{Label: "sh:curl", Slots: []Slot{slot("-X=", "POST")}}, "write"},
		{Step{Label: "sh:curl", Slots: []Slot{slot("--request=#2", "head")}}, "read"},
		{Step{Label: "sh:curl", Slots: []Slot{slot("--data=", "{}")}}, "write"},
		{Step{Label: "sh:curl"}, "read"},
		// A program with subcommands is not inferred from a table.
		{Step{Label: "sh:git push"}, "unknown"},
		{Step{Label: "sh:make"}, "unknown"},
		// An MCP tool's name is not evidence of its effect.
		{Step{Label: "mcp:delete_everything"}, "unknown"},
		{Step{Label: "TodoWrite"}, "unknown"},
	} {
		if got := StepEffect(c.st); got != c.want {
			t.Errorf("StepEffect(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}

func TestHasFileRedirect(t *testing.T) {
	for raw, want := range map[string]bool{
		"echo hi > out.txt":    true,
		"echo hi >> log":       true,
		"cmd 2>/dev/null":      false,
		"cmd >/dev/null 2>&1":  false,
		"cmd 2>&1 | tee x":     false,
		"test 1 -gt 0 && echo": false,
	} {
		if got := HasFileRedirect(raw); got != want {
			t.Errorf("HasFileRedirect(%q) = %v", raw, got)
		}
	}
}

func TestDropCopiedCalls(t *testing.T) {
	ss := []Session{
		{Client: "codex", Calls: []Call{{ID: "a"}, {ID: "b"}, {}}},
		{Client: "codex", Calls: []Call{{ID: "a"}, {ID: "c"}, {}}},
		{Client: "claude-code", Calls: []Call{{ID: "a"}}},
	}
	DropCopiedCalls(ss)
	ids := func(s Session) (out []string) {
		for _, c := range s.Calls {
			out = append(out, c.ID)
		}
		return out
	}
	if got := ids(ss[0]); !reflect.DeepEqual(got, []string{"a", "b", ""}) {
		t.Errorf("first = %q", got)
	}
	if got := ids(ss[1]); !reflect.DeepEqual(got, []string{"c", ""}) {
		t.Errorf("second = %q", got)
	}
	// The same id from another client is a different call.
	if got := ids(ss[2]); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("other client = %q", got)
	}
}

func TestInResult(t *testing.T) {
	st := Step{OutIDs: []string{"PROJ-9"}, OutTokens: []string{"alice"}, Output: "created branch feature-x"}
	for v, want := range map[string]bool{
		"PROJ-9":    true,
		"alice":     true,
		"feature-x": true,
		"abc":       false, // too short to mean anything
		"two words": false,
		"missing":   false,
	} {
		if got := InResult(v, st); got != want {
			t.Errorf("InResult(%q) = %v", v, got)
		}
	}
}

func TestSmallVocabulary(t *testing.T) {
	if !Derived("url.host") || Derived("--flag.x") || Derived("query") {
		t.Error("Derived")
	}
	for label, want := range map[string]bool{"sh:ls": true, "mcp:x": true, "js:y": true, "Read": true, "WebFetch": true, "Edit": false, "TodoWrite": false} {
		if Replayable(label) != want {
			t.Errorf("Replayable(%q) = %v", label, !want)
		}
	}
	if got := OneLine("a\n  b\tc", 10); got != "a b c" {
		t.Errorf("OneLine = %q", got)
	}
	if got := OneLine("abcdefghij", 4); got != "abcd…" {
		t.Errorf("OneLine cut = %q", got)
	}
	a, b := EpisodeID("codex", "s1", 0), EpisodeID("codex", "s1", 1)
	if !strings.HasPrefix(a, "ep_") || len(a) != 15 || a == b || a != EpisodeID("codex", "s1", 0) {
		t.Errorf("EpisodeID %q %q", a, b)
	}
	if got := TextKey("Deploy build 42  to   Staging"); got != "deploy build # to staging" {
		t.Errorf("TextKey = %q", got)
	}
	if TextKey("hi 1") != "" {
		t.Error("TextKey of a short text should be empty")
	}
	word := Slot{Type: SlotWord, Value: "staging"}
	if !SelectorSlot(word) || !IsScopeSlot(Step{}, word) {
		t.Error("a short word is a selector")
	}
	for _, sl := range []Slot{{Type: SlotPath, Value: "a/b"}, {Type: SlotWord}, {Type: SlotWord, Value: "x", Sub: true}, {Type: SlotWord, Value: strings.Repeat("a", 41)}} {
		if SelectorSlot(sl) {
			t.Errorf("SelectorSlot(%+v)", sl)
		}
	}
}

func TestMatchAt(t *testing.T) {
	seq := []int{1, 9, 2, 9, 9, 3, 1, 2, 3}
	if got := MatchAt(seq, []int{1, 2, 3}, 3); !reflect.DeepEqual(got, []int{0, 2, 5}) {
		t.Errorf("gapped match = %v", got)
	}
	if got := MatchAt(seq, []int{1, 2, 3}, 1); !reflect.DeepEqual(got, []int{6, 7, 8}) {
		t.Errorf("tight window = %v", got)
	}
	if got := MatchAt(seq, []int{3, 1, 9}, 2); got != nil {
		t.Errorf("no match = %v", got)
	}
	if got := MatchAt(seq, []int{2}, 0); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("single = %v", got)
	}
}
