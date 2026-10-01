package model

// LogicCandidate is a recurring, parameterized execution shape that an agent
// can author as a primitive. It is evidence of repeated logic, not evidence
// that replaying the recorded calls verbatim is safe or useful.
type LogicCandidate struct {
	ID         string       `json:"id"`
	Key        string       `json:"key"`
	Actions    []string     `json:"actions"`
	Edges      []string     `json:"edges,omitempty"`
	Parameters []string     `json:"parameters,omitempty"`
	Proposals  int          `json:"proposals"`
	Executions int          `json:"executions"`
	Sessions   int          `json:"sessions"`
	Evidence   []string     `json:"evidence"`
	Cautions   []string     `json:"cautions,omitempty"`
	Example    SpanProposal `json:"example"`
	Members    []string     `json:"members"`
}

// LogicFunnel is an observed result-flow root with its distinct follow-up
// operations. Branches may occur in different requests and in different
// orders; they are not a claim that all branches belong in one package.
type LogicFunnel struct {
	ID           string              `json:"id"`
	Root         string              `json:"root"`
	Sessions     int                 `json:"sessions"`
	Branches     []LogicFunnelBranch `json:"branches"`
	CandidateIDs []string            `json:"candidate_ids"`
	Members      []string            `json:"members"`
}

type LogicFunnelBranch struct {
	Action       string   `json:"action"`
	Slots        []string `json:"slots"`
	Sessions     int      `json:"sessions"`
	ForEach      bool     `json:"for_each,omitempty"`
	CandidateIDs []string `json:"candidate_ids"`
}
