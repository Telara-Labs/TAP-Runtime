---
name: tap-primitive-refine
description: Refine a TAP Discover handoff into a contained, reviewable primitive when a user explicitly chooses Refine with a coding agent.
---

# Refine a Discover proposal

Read `HANDOFF.md`, `program-graph.json`, `EVIDENCE.json`, the generated program and manifest in the handoff folder. Read `tap-runtime/docs/writing-a-primitive.md` for the current package/runtime contract. The graph is evidence, not a specification of business intent; inspect the cited local client sessions when a binding, branch, effect or boundary is uncertain.

Keep the reusable execution chunk: variable resource values become typed invocation inputs or values from earlier results. Preserve the source and relationship of each input, operation order, loops and stop conditions. Do not turn a result-dependent step into an unrelated caller input, guess an unobserved branch, or merge different tool bindings merely because their labels sound alike. Resolve each uncertainty with trace evidence or ask the user for the missing decision.

Produce a contained TAP package with declared tool and effect reach. Show the user the changed inputs, ordered calls, effects, outputs, full code and manifest, and the exact new package digest. Compare reach and behavior with the original proposal. Run the relevant TAP manifest and real-runner checks on fresh values before claiming it works. An edited package needs a new user decision; this skill does not authorize installation or publication.
