package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestSetupOutcomeAndSummary(t *testing.T) {
	for _, c := range []struct {
		rc    int
		said  string
		state int
		want  string
	}{
		{0, "Claude Code: added tap to ~/.claude.json. Start a new Claude Code session to use it.\n", 2, "connected"},
		{0, "Codex: ~/.codex/config.toml already as wanted.\n", 2, "already connected"},
		{0, "Cursor: skipped, cursor is not on this machine.\n", 3, "not installed"},
		{1, "not changed: permission denied\nmore detail\n", 4, "not changed: permission denied"},
	} {
		state := setupOutcome(c.rc, c.said)
		if state != c.state {
			t.Errorf("setupOutcome(%d, %q) = %d, want %d", c.rc, c.said, state, c.state)
		}
		if got := setupSummary(state, c.said); got != c.want {
			t.Errorf("setupSummary(%d, %q) = %q, want %q", state, c.said, got, c.want)
		}
	}
}

// Drawing the diagram while agents connect keeps one line per agent, and a
// failed agent's full message is printed once the diagram is done.
func TestSetupBoardDrawsEveryAgentAndKeepsFailures(t *testing.T) {
	var errs bytes.Buffer
	b := &setupBoard{names: []string{"Claude Code", "Codex", "Cursor"}, state: make([]int, 3), said: make([]*bytes.Buffer, 3)}
	for i, msg := range []string{"Claude Code: added tap.\n", "not changed: denied\nfull reason\n", "Cursor: skipped, cursor is not on this machine.\n"} {
		out, _ := b.begin(i, nil, nil)
		out.Write([]byte(msg))
		frame := strings.Join(b.render(i), "\n")
		if !strings.Contains(frame, "connecting…") {
			t.Fatalf("agent %d connecting shows no pulse:\n%s", i, frame)
		}
		rc := 0
		if i == 1 {
			rc = 1
		}
		b.mu.Lock()
		b.state[i] = setupOutcome(rc, b.said[i].String())
		b.mu.Unlock()
	}
	final := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(strings.Join(b.render(-1), "\n"), "")
	for _, want := range []string{"TAP ━┳", "Claude Code", "✓ connected", "✗ not changed: denied", "· not installed"} {
		if !strings.Contains(final, want) {
			t.Errorf("final diagram lacks %q:\n%s", want, final)
		}
	}
	b.live = nil
	if !stopFailures(b, &errs) {
		t.Error("a connected agent was not reported")
	}
	if !strings.Contains(errs.String(), "full reason") {
		t.Errorf("failure detail not printed: %q", errs.String())
	}
}
