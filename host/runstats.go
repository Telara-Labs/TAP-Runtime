package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// A primitive's speed is set by whoever wrote it: every tool call is a round
// trip from the program through the runner and the agent to the tool and
// back, measured at about 0.7 s (Claude in Chrome) to 1.4 s (Playwright) per
// call. So a tap_run result says what the run cost, and says so plainly when
// the cost looks like the program's shape rather than the work.
//
// The hint fires when both hold:
//   - at least slowCallFloor calls. 50 calls is 35 s or more of round trips
//     alone, so a small run never draws the hint, whatever its ratio.
//   - more than slowCallsPerInput calls per input item. A lookup that finds
//     an item through search took about 10 calls; one that walked a whole
//     list per item took 150 to 270. 20 per item leaves room for the first
//     shape and catches the second.
const (
	slowCallFloor     = 50
	slowCallsPerInput = 20
)

// inputItems counts what a run was asked to work on: the longest list among
// the top-level fields of the input object, or 1. Positional arguments that
// are not an input object count one each.
func inputItems(args []string) int {
	if len(args) == 1 {
		var obj map[string]any
		if json.Unmarshal([]byte(args[0]), &obj) == nil && obj != nil {
			n := 1
			for _, v := range obj {
				if list, ok := v.([]any); ok && len(list) > n {
					n = len(list)
				}
			}
			return n
		}
	}
	if len(args) == 0 {
		return 1
	}
	return len(args)
}

// runStatsText is the stats line, and the improvement hint when it applies,
// appended to a tap_run result.
func runStatsText(res *Result) string {
	inputs := res.Inputs
	if inputs < 1 {
		inputs = 1
	}
	text := fmt.Sprintf("\n[stats: %d tool call(s), %d failed, %d refused, %d input item(s), %s", res.Calls, res.FailedCalls, res.Refused, inputs, res.Elapsed.Round(100*time.Millisecond))
	if res.Replayed > 0 {
		text += fmt.Sprintf(", %d answered from the record", res.Replayed)
	}
	text += "]"
	if hint := improveHint(res.Calls, inputs, res.RunID); hint != "" {
		text += "\n[" + hint + "]"
	}
	return text
}

// improveHint names a call-heavy run and who can fix it. Empty otherwise.
func improveHint(calls, inputs int, runID string) string {
	if calls < slowCallFloor || calls <= slowCallsPerInput*inputs {
		return ""
	}
	hint := fmt.Sprintf("%d tool calls for %d input item(s), about %d per item: the primitive's author can publish a faster version, for example by reading the source once and matching every item against it, batching, or looping inside the backend, without reporting anything unresolved as found", calls, inputs, (calls+inputs/2)/inputs)
	if runID != "" {
		hint += "; tap_evidence on run " + runID + " shows each call"
	}
	return hint
}
