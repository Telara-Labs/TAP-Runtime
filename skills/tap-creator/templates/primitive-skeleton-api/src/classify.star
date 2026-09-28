# classify.star — deterministic signature/known-string matching.
# TAP transform host interface: step params arrive as predeclared globals (here: `items`, and
# `signatures` if you're using a signature table — see signatures_from in workflow.yaml). The
# value assigned to `result` is the step's output. No I/O, no imports — Starlark is sandboxed by
# construction; the runtime enforces step/time ceilings.
#
# Deterministic-first rule (references/workflow-authoring.md §2): this file does known-string /
# known-pattern lookup against STRUCTURED fields the source system already returns. It is NOT the
# place for semantic judgment — anything this can't classify goes to unclassified, and only a
# reason: step (if one exists) should attempt semantic classification of the leftover.

def _match(item):
    # REPLACE: match `item`'s structured fields against a known set of values/patterns.
    # Return a category string on match, or None.
    return None

def _classify(items):
    classified = []
    unclassified = []
    for i in items:
        category = _match(i)
        if category != None:
            classified.append({
                # REPLACE with the fields your output schema requires
                "evidence": "REPLACE_MATCHED_RULE_NAME",
                "category": category,
                "confidence": 1.0,
                "classified_by": "signature",
            })
        else:
            unclassified.append(i)
    return {
        "classified": classified,
        "unclassified": unclassified,
        "has_unclassified": len(unclassified) > 0,
    }

result = _classify(items)
