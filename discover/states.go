package discover

// A routine is judged on separate dimensions, never one verdict:
//
//   - SourceRole: who the work is for.
//   - Suitability: whether it is a useful, bounded procedure.
//   - DraftStatus: how far drafting got, and what blocks it.
//   - OutcomeEvidence: what the history shows about whether it worked.
//   - Validation: whether an exact package passed held-out cases.
//   - Value: whether a saving was measured against a baseline.
//
// Each count in the report is kept per dimension, and "needs authoring"
// is a claim about a useful procedure, never a bin for patterns whose
// usefulness is unknown.

// Source roles.
const (
	RoleUser           = "user_task"
	RoleScheduled      = "scheduled"
	RoleInfrastructure = "infrastructure"
	RoleHarness        = "harness"
	RoleUnknown        = "unknown"
)

// Suitability values.
const (
	SuitUseful        = "useful_procedure"
	SuitInvestigation = "unbounded_investigation"
	SuitInsufficient  = "insufficient_evidence"
	SuitInvalid       = "invalid_source"
)

// Draft statuses.
const (
	DraftNotAttempted = "not_attempted"
	DraftNeedsAuthor  = "needs_authoring"
	DraftComplete     = "structurally_complete"
	DraftBlocked      = "blocked_export"
)

// Outcome evidence values.
const (
	OutcomeReported   = "reported"
	OutcomeToolOK     = "observed_tool_success"
	OutcomeVerified   = "independently_verified"
	OutcomeEvFailed   = "failed"
	OutcomeEvUnknown  = "unknown"
	ValueUnmeasured   = "unmeasured"
	ValueEstimated    = "estimated"
	ValidationNotRun  = "not_run"
	EffectReadOnly    = "read_only"
	EffectWrites      = "writes"
	EffectUnknown     = "unknown"
	InputCaller       = "caller"
	InputPriorOutput  = "prior_output"
	InputConstant     = "constant"
	InputComposed     = "composed"
	InputUnresolved   = "unresolved"
	GoalStated        = "stated_template"
	GoalSelfContained = "self_contained"
	GoalUnknown       = "unknown"
)

// Contract is what the evidence establishes about a routine's task.
type Contract struct {
	// Goal says how the goal is evidenced: the requests share a template
	// (stated_template), the procedure's inputs and output define it on
	// their own (self_contained), or neither (unknown).
	Goal   string          `json:"goal"`
	Inputs []ContractInput `json:"inputs"`
	// Scope lists authority-bearing constants (a kube context, a namespace,
	// a host) that are part of the procedure's identity.
	Scope  []string `json:"scope,omitempty"`
	Effect string   `json:"effect"`
	// Output is "report" (what the steps print) or "state_change".
	Output string `json:"output"`
	// Judgment names steps whose content the agent decided per run.
	Judgment []string `json:"judgment,omitempty"`
	// Boundary is set when all judgment comes after the replayable steps:
	// the procedure runs them and hands back to the agent there.
	Boundary string `json:"boundary,omitempty"`
	// Approvals counts user approvals observed between the steps of the
	// recorded runs; none transfers to a new run.
	Approvals int `json:"approvals,omitempty"`
}

// ContractInput is one input and where its value comes from.
type ContractInput struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Source string `json:"source"`
	// From is the step (1-based) whose output supplied a prior_output value.
	From int `json:"from,omitempty"`
}

// legacyStates fills the dimensions from the single decision the rules made
// before they were separated. It reproduces those claims unchanged:
// needs_authoring was a claim of usefulness.
func (rt *Routine) legacyStates(d *Draft) {
	rt.SourceRole = map[string]string{"user": RoleUser, "automated": RoleScheduled, "scheduled": RoleScheduled, "bookkeeping": RoleInfrastructure, "harness": RoleHarness}[rt.Kind]
	if rt.SourceRole == "" {
		rt.SourceRole = RoleUnknown
	}
	switch rt.Decision {
	case "primitive":
		rt.Suitability, rt.DraftStatus = SuitUseful, DraftComplete
	case "needs_authoring":
		rt.Suitability, rt.DraftStatus = SuitUseful, DraftNeedsAuthor
	default:
		rt.DraftStatus = DraftNotAttempted
		rt.Suitability = SuitInsufficient
		if rt.Failed == CheckReplays {
			rt.Suitability = SuitInvestigation
		}
	}
	if d != nil && len(d.Blocked) > 0 {
		rt.DraftStatus = DraftBlocked
	}
	switch {
	case rt.Runs > 0 && rt.FailedRuns == rt.Runs:
		rt.OutcomeEvidence = OutcomeEvFailed
	case rt.Runs > 0 && rt.UnknownRuns == 0:
		rt.OutcomeEvidence = OutcomeToolOK
	default:
		rt.OutcomeEvidence = OutcomeEvUnknown
	}
	rt.Value = ValueUnmeasured
	if rt.Measured > 0 {
		rt.Value = ValueEstimated
	}
	rt.Family = rt.ID
	rt.Contract.Goal, rt.Contract.Effect, rt.Contract.Output = GoalUnknown, EffectUnknown, "unknown"
}
