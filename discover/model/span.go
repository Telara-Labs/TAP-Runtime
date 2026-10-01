package model

import (
	"time"
)

// SpanProposal is a causal slice of a task's recorded calls. It is deliberately
// unassessed: a data-flow or shared-input shape does not prove the task was
// useful, safe to replay, or complete. Calls are one-based within Request and
// may be non-contiguous; a brief includes exactly these calls.
type SpanProposal struct {
	ID             string          `json:"id"`
	Client         string          `json:"client"`
	Session        string          `json:"session"`
	Request        int             `json:"request"`
	Task           string          `json:"task"`
	Status         string          `json:"status"`
	Kind           string          `json:"kind"`
	CodeShape      string          `json:"code_shape,omitempty"`  // normalized inline code, retrieval only
	CodeFamily     string          `json:"code_family,omitempty"` // broad call-order motif, not equivalence
	CodeScope      string          `json:"code_scope,omitempty"`  // direct or embedded shell snippet
	Calls          []int           `json:"calls"`
	CallHashes     []string        `json:"call_hashes"`
	Tools          []string        `json:"tools"`
	Inputs         []SpanInput     `json:"inputs,omitempty"`
	Effect         string          `json:"effect"`
	GoalKey        string          `json:"goal_key"`
	ShapeKey       string          `json:"shape_key"`
	Composition    SpanComposition `json:"composition"`
	Review         SpanTaskReview  `json:"review"`
	ContextRequest int             `json:"context_request,omitempty"`
	EvidenceScore  int             `json:"evidence_score"`
	Start          time.Time       `json:"start,omitempty"`
}

// SpanInput records provenance, not a potentially secret value. FromCall is
// one-based within the request; FromRequest identifies a previous user turn.
type SpanInput struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	FromCall    int    `json:"from_call,omitempty"`
	FromRequest int    `json:"from_request,omitempty"`
}

// SpanGroup is a review queue bucket, not a claim that its members implement
// one interchangeable procedure. The key includes goal, ordered call shape,
// input provenance and observed effect; it does not use tool-label overlap.
type SpanGroup struct {
	ShapeKey  string       `json:"shape_key"`
	Proposals int          `json:"proposals"`
	Sessions  int          `json:"sessions"`
	Example   SpanProposal `json:"example"`
	Members   []string     `json:"members"`
}
