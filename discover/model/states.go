package model

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

const OutcomeReported = "reported"

const OutcomeToolOK = "observed_tool_success"

const OutcomeVerified = "independently_verified"

const OutcomeEvFailed = "failed"

const OutcomeEvUnknown = "unknown"

const ValueUnmeasured = "unmeasured"

const ValueEstimated = "estimated"

const ValidationNotRun = "not_run"

const EffectReadOnly = "read_only"

const EffectWrites = "writes"

const EffectUnknown = "unknown"

const GoalStated = "stated_template"

const GoalSelfContained = "self_contained"

const GoalUnknown = "unknown"

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
