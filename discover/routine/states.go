package routine

import (
	"gitlab.com/telara-labs/tap-runtime/discover/model"
)

// Outcome evidence values.
const (
	InputCaller      = "caller"
	InputPriorOutput = "prior_output"
	InputConstant    = "constant"
	InputComposed    = "composed"
	InputUnresolved  = "unresolved"
)

// legacyStates fills the dimensions from the single decision the rules made
// before they were separated. It reproduces those claims unchanged:
// needs_authoring was a claim of usefulness.
func RoutineLegacyStates(rt *model.Routine, d *model.Draft) {
	rt.SourceRole = map[string]string{"user": model.RoleUser, "automated": model.RoleScheduled, "scheduled": model.RoleScheduled, "bookkeeping": model.RoleInfrastructure, "harness": model.RoleHarness}[rt.Kind]
	if rt.SourceRole == "" {
		rt.SourceRole = model.RoleUnknown
	}
	switch rt.Decision {
	case "primitive":
		rt.Suitability, rt.DraftStatus = model.SuitUseful, model.DraftComplete
	case "needs_authoring":
		rt.Suitability, rt.DraftStatus = model.SuitUseful, model.DraftNeedsAuthor
	default:
		rt.DraftStatus = model.DraftNotAttempted
		rt.Suitability = model.SuitInsufficient
		if rt.Failed == model.CheckReplays {
			rt.Suitability = model.SuitInvestigation
		}
	}
	if d != nil && len(d.Blocked) > 0 {
		rt.DraftStatus = model.DraftBlocked
	}
	switch {
	case rt.Runs > 0 && rt.FailedRuns == rt.Runs:
		rt.OutcomeEvidence = model.OutcomeEvFailed
	case rt.Runs > 0 && rt.UnknownRuns == 0:
		rt.OutcomeEvidence = model.OutcomeToolOK
	default:
		rt.OutcomeEvidence = model.OutcomeEvUnknown
	}
	rt.Value = model.ValueUnmeasured
	if rt.Measured > 0 {
		rt.Value = model.ValueEstimated
	}
	rt.Family = rt.ID
	rt.Contract.Goal, rt.Contract.Effect, rt.Contract.Output = model.GoalUnknown, model.EffectUnknown, "unknown"
}
