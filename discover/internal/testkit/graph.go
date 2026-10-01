package testkit

import (
	"strings"

	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// GraphCandidateFor selects span proposals from the sessions and returns the logic
// candidate whose actions match, failing the test if none does.
func GraphCandidateFor(t *testing.T, ss []trace.Session, actions ...string) (model.LogicCandidate, []model.SpanProposal) {
	t.Helper()
	ps := retrieval.SelectSpanProposals(ss)
	for _, c := range retrieval.GroupLogicCandidates(ps) {
		if len(c.Actions) != len(actions) {
			continue
		}
		match := true
		for i := range actions {
			// An action matches its name, or its name followed by the
			// choices the corpus evidence made part of it.
			match = match && (c.Actions[i] == actions[i] || strings.HasPrefix(c.Actions[i], actions[i]+"#"))
		}
		if match {
			return c, ps
		}
	}
	t.Fatalf("no logic candidate %v among %+v", actions, retrieval.GroupLogicCandidates(ps))
	return model.LogicCandidate{}, nil
}
