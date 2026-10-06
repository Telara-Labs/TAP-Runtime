package discover

import (
	"regexp"
	"strings"
	"testing"
)

var sgrCodes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plainBoard(b *readBoard, frame int) string {
	return sgrCodes.ReplaceAllString(strings.Join(b.render(frame), "\n"), "")
}

// The board shows each agent's own session count while it reads, a running
// total across agents, and the search's step with its session count.
func TestReadBoardShowsSessionProgress(t *testing.T) {
	b := &readBoard{clients: []string{"claude-code", "codex", "cursor"}, color: true,
		state: make([]int, 3), done: make([]int, 3), total: make([]int, 3), count: make([]int, 3)}
	b.reading(0)
	b.progress(0)(37, 112)
	got := plainBoard(b, 3)
	for _, want := range []string{"claude-code", " 37/112 sessions", "0/3 agents · 37 sessions", "cursor       · waiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("while reading, board lacks %q:\n%s", want, got)
		}
	}
	b.read(0, 110)
	b.reading(1)
	b.progress(1)(10, 57)
	b.read(1, 57)
	b.reading(2) // cursor reports no progress
	got = plainBoard(b, 4)
	for _, want := range []string{"✓ ━━━━━━━━━━━━  110 sessions", "2/3 agents · 167 sessions", "cursor       ", "reading…"} {
		if !strings.Contains(got, want) {
			t.Errorf("after two agents, board lacks %q:\n%s", want, got)
		}
	}
	b.read(2, 0)
	b.analysing(167)
	b.step("building call graphs", 80, 167)
	got = plainBoard(b, 5)
	if !strings.Contains(got, " 80/167 · building call graphs") {
		t.Errorf("search step not shown:\n%s", got)
	}
	b.step("grouping repeated chains", 0, 0)
	if got = plainBoard(b, 6); !strings.Contains(got, "167/167 · grouping repeated chains") && !strings.Contains(got, "80/167 · grouping repeated chains") {
		t.Errorf("a step without a count lost the bar:\n%s", got)
	}
	b.analysed(6)
	if got = plainBoard(b, -1); !strings.Contains(got, "✓ Found 6 repeated tasks in 167 sessions") {
		t.Errorf("final line missing:\n%s", got)
	}
}

// A nil board, which pipes get, accepts every call.
func TestNilReadBoardIsSilent(t *testing.T) {
	var b *readBoard
	b.reading(0)
	b.progress(0)(1, 2)
	b.read(0, 1)
	b.analysing(1)
	b.step("x", 1, 1)
	b.analysed(1)
	b.stop()
}
