---
name: tap-primitive-refine
description: Refine a TAP Discover handoff into a contained, reviewable primitive when a user explicitly chooses agent eval or Refine with a coding agent.
---

# Refine a Discover proposal

Start with the handoff folder. A primitive-menu handoff holds `HANDOFF.md` (outcome and reading order), `REFINE-PROMPT.md` (the full rules; follow them), `program-graph.json` (steps, bindings with evidence levels, edges, control edges, inputs), `QUESTIONS.md` (each open question with the executions it concerns), `EVIDENCE-INDEX.json` (every supporting execution with transcript line locators and line hashes) and `evidence/` (a readable excerpt per execution). An older generated-program handoff holds `HANDOFF.md`, `program-graph.json`, `EVIDENCE.json` and the generated program and manifest. Read `tap-runtime/docs/writing-a-primitive.md` for the current package/runtime contract.

Work from the questions first. Each names the step and argument, the evidence level (explicit, inferred, ambiguous, missing) and the executions to read; open them with `tap discover evidence <handoff-folder> <execution-id>`, which verifies the line hashes before printing. A text mention is not a binding. Say which executions you read; do not claim you read them all if you sampled.

Keep the reusable execution chunk: variable resource values become typed invocation inputs or values from earlier results. Preserve the source and relationship of each input, operation order, loops and stop conditions. Do not turn a result-dependent step into an unrelated caller input, guess an unobserved branch, or merge different tool bindings merely because their labels sound alike. Resolve each uncertainty with trace evidence or ask the user for the missing decision.

Produce a contained TAP package with declared tool and effect reach. Show the user the changed inputs, ordered calls, effects, outputs, full code and manifest, and the exact new package digest. Compare reach and behavior with the original proposal. Run the relevant TAP manifest and real-runner checks on fresh values before claiming it works. An edited package needs a new user decision; this skill does not authorize installation or publication.
