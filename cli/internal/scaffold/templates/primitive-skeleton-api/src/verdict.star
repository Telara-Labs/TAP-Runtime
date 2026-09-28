# verdict.star — merge deterministic (+ optional reasoned) classifications into the final output
# shape. Predeclared params: `classified`, `reasoned` (None when the optional reason: step is
# skipped OR deleted — handle that explicitly, never assume it ran). Output via `result`. Pure
# transform: zero builtins, no I/O possible.
#
# If you deleted the optional reason: step entirely, delete the `reasoned` param from this step's
# `params:` in workflow.yaml too, and simplify `_reasoned_records` below to always return [].
#
# Gate rule (if a reason: step exists): a reasoning result below your chosen confidence threshold
# should be DEMOTED to an explicit "unknown"/"gate_demotion" state — model output is bounded by a
# deterministic gate here, never trusted raw. See gitlab-pipeline-triage's verdict.star in the TAP
# docs for the worked pattern.

def _reasoned_records(reasoned):
    if reasoned == None:
        return []
    # REPLACE: apply your confidence gate and shape reasoned records to match the classified shape.
    return reasoned

result = {
    "REPLACE_TOP_LEVEL_FIELD": classified + _reasoned_records(reasoned),
}
