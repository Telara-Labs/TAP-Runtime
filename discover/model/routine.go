package model

import (
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// Check names, in the order they run.
const (
	CheckReplays = "replays"
	CheckSameWay = "same way"
	CheckWorth   = "worth it"
)

// Funnel is what the request-level run found at each stage.
type Funnel struct {
	Sessions          int            `json:"sessions"`
	Calls             int            `json:"calls"`
	Requests          int            `json:"requests"`
	RequestsWithSteps int            `json:"requests_with_steps"`
	Groups            int            `json:"groups"`
	Routines          int            `json:"routines"`
	Removed           map[string]int `json:"removed"`
	Primitives        int            `json:"primitives"`
	// NeedsAuthoring counts routines that recur and replay but need a step
	// or a value's source written by hand before they can run.
	NeedsAuthoring int `json:"needs_authoring"`
	// Merged counts routines folded into another (same kind, same steps).
	Merged int `json:"merged"`
	// ByKind splits the primitives by who the work is for.
	ByKind map[string]int `json:"primitives_by_kind"`
	// Savings says how the token figures were obtained.
	Savings string `json:"savings"`
	// Per-dimension counts over routines not merged into another:
	// source role -> suitability, suitability -> draft status, and outcome,
	// validation and value. Each sums to the same total.
	ByRole       map[string]map[string]int `json:"by_role"`
	ByDraft      map[string]map[string]int `json:"by_suitability_draft"`
	ByOutcome    map[string]int            `json:"by_outcome"`
	ByValidation map[string]int            `json:"by_validation"`
	ByValue      map[string]int            `json:"by_value"`
}

// Routine is one group of requests that recurred, with its template and the
// verdict of the checks.
type Routine struct {
	Candidate
	Requests    int     `json:"requests"`
	Consistency float64 `json:"consistency"`
	// Coverage is how much of its requests the routine is: its steps over
	// all the replayable steps each request ran (median). A procedure is
	// most of its request; recurring calls inside a long investigation are
	// a small part of it.
	Coverage float64 `json:"coverage"`
	// Inputs lists each input and whether its values were in the request.
	Inputs []RoutineInput `json:"inputs"`
	// CoveredBy names the skill most of its requests loaded, if any.
	CoveredBy string `json:"covered_by,omitempty"`
	// Decision is "primitive" (ready to save), "needs_authoring" (it
	// recurs and replays, but a step's content or a value's source must be
	// written by hand) or "removed" (Failed names the check, Why says how).
	Decision string `json:"decision"`
	// ID is stable across runs for the same kind and step sequence.
	ID string `json:"id"`
	// Kind is who the work is for: "user" (asked for by a person),
	// "automated" (a program sent the same prompt each time), "scheduled"
	// (a Codex automation) or "bookkeeping" (Telara's own recording and
	// tool-discovery calls, which instructions make every agent do).
	Kind string `json:"kind"`
	// Statistics is "not_run": recurrence here is 3+ requests in 2+
	// sessions, not a significance test.
	Statistics string `json:"statistics"`
	// Validation is "not_run": discovery never executes a draft. A primitive
	// is validated by running it on held-out inputs with independent
	// checks; recurrence and passing the publish checks are not that.
	Validation string `json:"validation"`
	// MergedInto is the id of the routine this one duplicated (same kind,
	// same set of steps); a merged routine is not counted again.
	MergedInto string `json:"merged_into,omitempty"`
	// Loops lists steps most runs made several times with different values.
	Loops  []string `json:"loops,omitempty"`
	Failed string   `json:"failed,omitempty"`
	Why    string   `json:"why,omitempty"`
	// Runs counts the requests that ran its sequence; FailedRuns those where
	// a step failed (not evidence); UnknownRuns those whose client recorded
	// no result.
	Runs        int `json:"runs"`
	FailedRuns  int `json:"failed_runs"`
	UnknownRuns int `json:"unknown_runs"`
	// Example is one request's text, shortened.
	Example string `json:"example"`
	// The separate dimensions (see states.go).
	SourceRole      string   `json:"source_role"`
	Suitability     string   `json:"suitability"`
	DraftStatus     string   `json:"draft_status"`
	Blockers        []string `json:"blockers,omitempty"`
	OutcomeEvidence string   `json:"outcome_evidence"`
	Value           string   `json:"value"`
	// Reasons are stable codes for each dimension's value.
	Reasons []string `json:"reasons,omitempty"`
	// Family is the task family (goal and outcome); ID is this procedure.
	Family   string   `json:"family"`
	Contract Contract `json:"contract"`
	// Baseline names existing automation of the same work, if any.
	Baseline string `json:"baseline,omitempty"`
	// Parent is the routine this one is a bounded part of: a procedure
	// found inside requests whose whole was not one.
	Parent string `json:"parent,omitempty"`
	// Sources are the requests the routine was found in. They point into
	// this machine's history and are for local review only.
	Sources   []SourceRef         `json:"sources,omitempty"`
	Draft     *Draft              `json:"-"`
	Occ       [][]trace.Step      `json:"-"`
	LoopSpecs map[string]LoopSpec `json:"-"`
}

// SourceRef names one request in a client's session history. Ran is true
// when that request ran the routine's steps in its order and succeeded.
type SourceRef struct {
	Client  string `json:"client"`
	Session string `json:"session"`
	Request int    `json:"request"`
	Ran     bool   `json:"ran"`
}

type RoutineInput struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Explained float64 `json:"explained"` // share of runs whose request contained the value
}
