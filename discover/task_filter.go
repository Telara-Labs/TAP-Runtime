package discover

import "strings"

// SpanTaskReview is a deterministic admission check for the human review
// queue. Ready means the recorded trace has a task-shaped contract, not that
// it is useful, safe, or a validated primitive.
type SpanTaskReview struct {
	Ready     bool     `json:"ready"`
	Component bool     `json:"component"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Output    string   `json:"output"`
	Stop      string   `json:"stop"`
	Reasons   []string `json:"reasons,omitempty"`
}

func spanSyntheticRequest(s Session, req int) bool {
	return req >= 0 && req < len(s.Requests) &&
		(req < len(s.RequestRoles) && s.RequestRoles[req] == "synthetic_context" ||
			isClaudeContinuationSummary(s.Requests[req]))
}

func assessSpanTask(s Session, req int, nodes []spanNode, set []int, inputs []SpanInput) SpanTaskReview {
	r := SpanTaskReview{Source: "user", Input: "unresolved", Output: "unobserved", Stop: "single_pass"}
	if req < len(s.RequestRoles) && s.RequestRoles[req] == "scheduled" {
		r.Source = "scheduled"
	}
	if spanSyntheticRequest(s, req) {
		r.Source = "synthetic_context"
		r.Reasons = append(r.Reasons, "synthetic_request")
	}
	request := s.Requests[req]
	if spanOpenEndedTask(request) {
		r.Reasons = append(r.Reasons, "judgment_boundary_unresolved")
	}
	if len(set) == 1 {
		// A direct tool invocation with its raw result is already available
		// to the agent. It contains no captured composition or transformation.
		r.Reasons = append(r.Reasons, "single_tool_passthrough")
	}
	if spanHasReference(spanWords(request)) {
		for i := req - 1; i >= 0; i-- {
			if !spanSyntheticRequest(s, i) && !isHarness(s.Requests[i]) && strings.TrimSpace(s.Requests[i]) != "" {
				request = s.Requests[i] + " " + request
				break
			}
		}
	}
	userIntent := false
	for _, i := range set {
		if spanDirectIntent(s.Requests, req, nodes[i]) || spanOperationIntent(request, spanActionRole(nodes[i])) {
			userIntent = true
		}
	}
	if !userIntent {
		r.Reasons = append(r.Reasons, "no_requested_operation")
	}
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
		r.Input = "unresolved"
		r.Reasons = append(r.Reasons, "input_provenance_unknown")
	case known:
		r.Input = "caller_or_result"
	default:
		// A zero-argument command can have a fixed, visible scope. The task
		// match above still has to establish why it was run.
		r.Input = "fixed_scope"
	}
	goodOutput, failed := false, false
	for _, i := range set {
		c := nodes[i].call
		if c.Outcome == OutcomeFailed || spanOversizeResult(c.Output) {
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
	composition := spanComposition(nodes, set)
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
		!failed && goodOutput && !spanHasReason(r.Reasons, "stop_condition_unproven")
	return r
}

func spanHasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

// Match the requested operation and resource, rather than a provider name or
// concrete identifier alone. This extends direct-intent matching to result
// chains whose first call may obtain the input for later calls.
func spanOperationIntent(request, action string) bool {
	want, actual := spanWords(request), spanWords(action)
	verb := false
	for w := range want {
		if spanVerb(w) != "" && spanVerb(w) == spanOperationVerb(actual, spanNode{}) {
			verb = true
			break
		}
	}
	if !verb {
		return false
	}
	for w := range actual {
		if want[w] && !spanGenericWord(w) && spanVerb(w) == "" {
			return true
		}
	}
	return false
}

func spanOversizeResult(output string) bool {
	text := strings.ToLower(output)
	return strings.Contains(text, "response exceeded") || strings.Contains(text, "output exceeds") ||
		strings.Contains(text, "result too large")
}

func spanOpenEndedTask(request string) bool {
	w := spanWords(request)
	for _, cue := range []string{"why", "how", "investigate", "diagnose", "debug", "fix", "implement", "build", "design"} {
		if w[cue] {
			// A named collection step can still be a bounded subtask of a
			// larger investigation. The label must make that step explicit.
			if (cue == "investigate" || cue == "diagnose") && (w["collect"] || w["list"] || w["fetch"]) {
				continue
			}
			return true
		}
	}
	return false
}

// ReviewSpanProposals keeps the broad causal inventory available for audits
// while giving the human queue only proposals with an observable task contract.
func ReviewSpanProposals(ps []SpanProposal) []SpanProposal {
	out := make([]SpanProposal, 0)
	for _, p := range ps {
		if p.Review.Ready {
			out = append(out, p)
		}
	}
	return out
}

func ReviewSpanComponents(ps []SpanProposal) []SpanProposal {
	out := make([]SpanProposal, 0)
	for _, p := range ps {
		if p.Review.Component && !p.Review.Ready {
			out = append(out, p)
		}
	}
	return out
}
