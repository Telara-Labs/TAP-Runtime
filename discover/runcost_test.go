package discover

import (
	"math"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestRunCostSavesAllButOneTurn(t *testing.T) {
	st := func(turn int, total float64) trace.Step {
		return trace.Step{Label: "sh:git status", Tokens: trace.Usage{Cached: total}, Turn: turn, Measured: true, Turns: 1}
	}
	// An edit decided per run is not replayed, so it saves nothing.
	edit := trace.Step{Label: "patch:update", Tokens: trace.Usage{Cached: 500}, Turn: 9, Measured: true, Turns: 1}
	if run, saved, ok := runCost([]trace.Step{st(1, 100), edit, st(2, 100)}); !ok || run.Total() != 200 || math.Abs(saved.Total()-100) > 1e-9 {
		t.Fatalf("with an edit: run %v saved %v ok %v", run, saved, ok)
	}
	if _, _, ok := runCost([]trace.Step{edit}); ok {
		t.Fatal("a run with nothing replayable has no saving")
	}
	run, saved, ok := runCost([]trace.Step{st(1, 100), st(2, 100), st(3, 100)})
	if !ok || run.Total() != 300 || math.Abs(saved.Total()-200) > 1e-9 {
		t.Fatalf("run %v saved %v", run, saved)
	}
	if _, _, ok := runCost([]trace.Step{st(1, 1), {}}); ok {
		t.Fatal("an unmeasured step must make the run unmeasured")
	}
}
