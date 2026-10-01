package retrieval

import (
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// SpanInlineCode reports a shell call that hands a program that runs code
// (an interpreter or shell) a multi-line script written into the command.
func SpanInlineCode(c trace.Call) bool {
	if c.Tool != "shell" || !strings.Contains(c.Command, "\n") {
		return false
	}
	f := strings.Fields(c.Command)
	return len(f) > 0 && shellparse.ProgramCommandRunsCode(f[0], f[1:])
}

func SpanSyntheticRequest(s trace.Session, req int) bool {
	return req >= 0 && req < len(s.Requests) &&
		(req < len(s.RequestRoles) && s.RequestRoles[req] == "synthetic_context" ||
			history.IsClaudeContinuationSummary(s.Requests[req]))
}

func AssessSpanTask(s trace.Session, req int, nodes []SpanNode, set []int, inputs []model.SpanInput) model.SpanTaskReview {
	r := model.SpanTaskReview{Source: "user", Input: "unresolved", Output: "unobserved", Stop: "single_pass"}
	if req < len(s.RequestRoles) && s.RequestRoles[req] == "scheduled" {
		r.Source = "scheduled"
	}
	if SpanSyntheticRequest(s, req) {
		r.Source = "synthetic_context"
		r.Reasons = append(r.Reasons, "synthetic_request")
	}
	for _, i := range set {
		if SpanInlineCode(nodes[i].Call) {
			// Code written into the call for this run is a decision the
			// agent made, like an edit: a judgment step, not a replay.
			r.Reasons = append(r.Reasons, "inline_code_written_per_run")
			break
		}
	}
	// Work the agent started on its own is still repeated work: whether the
	// request named it never decides readiness.
	known, unknown := false, false
	for _, in := range inputs {
		if in.Source == "unknown" {
			unknown = true
		} else {
			known = true
		}
	}
	switch {
	case unknown:
		// A value with no known source is the caller's input, not a blocker.
		r.Input = "caller_or_result"
	case known:
		r.Input = "caller_or_result"
	default:
		// A zero-argument command can have a fixed, visible scope. The task
		// match above still has to establish why it was run.
		r.Input = "fixed_scope"
	}
	goodOutput, failed := false, false
	for _, i := range set {
		c := nodes[i].Call
		if c.Outcome == trace.OutcomeFailed || SpanOversizeResult(c.Output) {
			failed = true
		}
		if strings.TrimSpace(c.Output) != "" {
			goodOutput = true
		}
	}
	if failed {
		r.Reasons = append(r.Reasons, "failed_or_oversized_call")
	}
	if goodOutput && !failed {
		r.Output = "tool_result_observed"
	} else {
		r.Reasons = append(r.Reasons, "output_not_observed")
	}
	composition := SpanComposition(nodes, set)
	for _, repeat := range composition.Repetition {
		if repeat.Kind != "for_each" {
			r.Stop = "unproven_repeat"
			r.Reasons = append(r.Reasons, "stop_condition_unproven")
			break
		}
		r.Stop = "bounded_input_list"
	}
	r.Ready = len(r.Reasons) == 0
	// A result-derived multi-call component may be worth authoring even when
	// its enclosing user request is an investigation. Keep its missing task
	// contract visible instead of calling it a complete procedure.
	r.Component = len(set) > 1 && len(composition.Edges) > 0 && (r.Source == "user" || r.Source == "scheduled") &&
		!failed && goodOutput && !SpanHasReason(r.Reasons, "stop_condition_unproven")
	return r
}

func SpanHasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func SpanOversizeResult(output string) bool {
	text := strings.ToLower(output)
	return strings.Contains(text, "response exceeded") || strings.Contains(text, "output exceeds") ||
		strings.Contains(text, "result too large")
}

// ReviewSpanProposals keeps the broad causal inventory available for audits
// while giving the human queue only proposals with an observable task contract.
func ReviewSpanProposals(ps []model.SpanProposal) []model.SpanProposal {
	out := make([]model.SpanProposal, 0)
	for _, p := range ps {
		if p.Review.Ready {
			out = append(out, p)
		}
	}
	return out
}

func ReviewSpanComponents(ps []model.SpanProposal) []model.SpanProposal {
	out := make([]model.SpanProposal, 0)
	for _, p := range ps {
		if p.Review.Component && !p.Review.Ready {
			out = append(out, p)
		}
	}
	return out
}
