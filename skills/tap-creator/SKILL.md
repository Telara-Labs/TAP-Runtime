---
name: tap-creator
description: >-
  Turn a just-completed agent task into a governed TAP (Trusted Agent Primitives) package —
  manifest, workflow.yaml, Starlark transforms, redacted fixtures, and contract tests, gated
  through `tap validate` -> `tap test` -> `tap publish`. Use this skill whenever the user says
  "turn this into a primitive," "make this repeatable," "compile this workflow," "package this
  as a TAP primitive/tool," "add a credential slot," "write a workflow.yaml," "cache this so we
  don't re-reason every time," or "fork/harden this primitive." Also use it proactively — without
  the user naming TAP — whenever they just finished a multi-step API, browser, or cross-tool task
  (pipeline triage, ticket/PR audits, changelog watching, standup digests, cycle-health checks) and
  ask to save it, automate it, schedule it, or turn it into a tool. If a task involved 2+ tool
  calls and the user wants to run it again, this skill applies — do not wait for a more explicit
  request.
---

# tap-creator

Compiles a just-completed task into a **primitive**: a typed, permission-declared package that
`tap validate` / `tap test` / `tap publish` can gate before it ever reaches a runtime. The product
is not the compiled code — it is the **manifest a security reviewer can trust at a glance**. Every
step below exists to keep that manifest honest against what the workflow actually does.

Read `references/schemas.md` before writing any artifact — it has the exact field names every
downstream consumer (`tap validate`, the registry, this skill's own gates) trusts byte-for-byte.
Unknown top-level keys in `primitive.yaml` are rejected; don't improvise new ones.

## Before you start: is this actually a primitive?

Primitives are for **repeated, boundable** work: a fixed tool sequence, a fixed extraction, a
fixed judgment — the kind of thing worth compiling because it will run "a million times." If the
just-completed task was genuinely one-off, ad hoc, or requires open-ended judgment on every run,
say so and don't force it into this shape. When in doubt, prefer building the primitive anyway —
even a thin one — over skipping it; TAP composes, and a narrow read-only primitive is cheap
insurance against the same reasoning being redone next week.

## The loop (do not skip or reorder steps 1-7)

```
1. Extract from the transcript  -> requirements-checklist.json   [BLOCKING gate]
2. Pick a skeleton (api|web)     -> scaffold from templates/
3. Write the contract            -> primitive.yaml + schemas/{input,output}.json
4. Author the workflow           -> workflow.yaml + src/*.star
5. Generate fixtures             -> tests/fixtures/*.json
6. Write contract tests          -> tests/contract.test.yaml
7. Gate: tap validate -> tap test -> tap publish   [BLOCKING, in order]
8. Iterate only the failing artifact, rerun the gate that failed, stop when clean or stalled
```

### Step 1 — Extract from the transcript, never from an interview

Read `agents/extractor.md` and do this yourself or spawn a subagent with those instructions. The
source of truth is **the transcript of the task just completed** — every tool call, every host
contacted, every credential used, every correction the user made, every place you improvised. Do
not interview the user for requirements you can derive by re-reading what actually happened; only
ask the user to fill genuine gaps (e.g., "was `get_job` necessary, or did `list_jobs` already carry
what you needed?").

Classify **every single observed tool call** as either `declared` (it earns a place in the
compiled path and a line in `requirements`) or `rejected` (it was exploration, a dead end, or
out-of-scope, with a one-line reason). Write this to `requirements-checklist.json` using the exact
schema in `references/schemas.md#requirements-checklistjson`.

**Do not proceed to step 2 until `unclassified_remaining` is 0 in that file.** This is a file
artifact, not a note to yourself — `tap validate` cross-checks it against the manifest and
fixtures, and an unclassified call is exactly the gap that turns into an undeclared-egress finding
at publish time. Look at `examples/gitlab-pipeline-triage/requirements-checklist.json` in the TAP
docs if you want the canonical shape: two rejected calls (`get_job` — redundant signal, dropped;
`retry_pipeline` — write-class, out of scope) sitting right alongside the declared ones, each with
a reason a reviewer can act on.

### Step 2 — Pick a skeleton

- **API primitive** (pure `api:`/`transform:`/`reason:` steps against integrations behind
  mcp-gateway): copy `templates/primitive-skeleton-api/`.
- **Web primitive** (a `browser:` step against a page with no API): copy
  `templates/primitive-skeleton-web/`.
- **Mixed** (API + browser, e.g. a UI-only analytics panel joined with API metadata): start from
  `primitive-skeleton-web/` and add an `api:` step from the other skeleton's workflow.yaml.

If the `tap` CLI is present, `tap init <name>` (one line) scaffolds this exact template — byte-
identical to `templates/primitive-skeleton-api/` (or `--web` for the web skeleton), REPLACE_*
placeholders and all, since the CLI embeds a manually-synced copy of these same files (verified at
G0; the two trees must be kept in sync by hand — see `telara-tap/cli/internal/scaffold/templates/
SYNC_NOTE.md`). Use it, then apply this skill's authoring guidance on top of what it produces. If
it isn't available yet, copy the template directory verbatim; the shape is identical either way.

Name the package `<publisher>/<name>` — reverse-DNS, domain-verified publisher (e.g. `dev.telara`)
plus a kebab-case name — per the TAP naming decision. `metadata.publisher` and `metadata.name` are
separate fields in the manifest; the full id is never baked into a single string field.

### Step 3 — Write the contract (typed I/O, never observed literals)

Read `agents/contract-writer.md`. Two rules carry the most weight here:

1. **Generalize every literal that crossed a tool boundary.** A specific project ID becomes
   `project_id: string`; a specific board name becomes an enum only if the task is genuinely
   bounded to a fixed set, otherwise a free `string`. The test for "did I overfit": would this
   input schema reject the *next* legitimate caller's value? If yes, it's too narrow. This is the
   "used a million times" rule — a primitive that only works for the one input observed during
   exploration is useless.
2. **Credential slots declare capability, never auth mechanism.** A slot names the integration and
   the `actions` it needs (`pipelines.read`, `jobs.read` — the catalog's class/operation/resource
   vocabulary), and nothing about *how* the credential authenticates. **Do not** write
   `type: oauth2` or `type: personal_access_token` into a slot just because that's what you used
   while exploring — that is the single most common authoring overfit TAP exists to prevent, and it
   silently locks out every tenant whose fleet uses a different auth method for the same
   integration. The only two legitimate mechanism constraints are `type: browser_session`
   (a genuinely distinct capability class) and an optional `identity: user_delegated|service`
   constraint when the target system's semantics differ by acting identity. Anything narrower than
   that is a publish finding, not a style choice. See `references/schemas.md#credential-slot` and
   `03-primitive-structure.md` §3.0 in the TAP docs for the full rationale.

**Invent-vs-generalize rule (resolves the extractor's "never invent" vs. this step's "generalize"
tension — G0 ruling, CHANGELOG.md v1 ruling 8):** authors MAY generalize values the transcript
exercised into caller-facing typed knobs; MUST NOT add capabilities/steps/inputs the transcript
never performed. Turning the one project ID you saw into a `project_id: string` field is
generalizing an *observed* value into a typed knob — sanctioned, in fact required by rule 1 above.
Adding a caller-facing filter, a new credential action, or a whole extra step because it seems like
a caller would probably want it — something the transcript never actually exercised — is inventing
a capability, not generalizing one, and it does not belong in the manifest. Every declared input
must be wired to at least one step or output (the CLI's `unused-input` reverse lint catches the
dead ones tap validate can see; this rule is why the lint exists).

`metadata.description` and `output_description` become the projected MCP tool's description, which
makes them attack surface (tool-poisoning target). Enforce, don't just suggest:

- Describe shape and effect only — no IDs, no live URLs, no example tenant values, no tokens.
- Never write "if this fails, try..." — that teaches the calling model to route around a bug
  instead of you fixing the workflow.
- Everything claimed must be a subset of what `requirements`/`effects`/`outputSchema` actually
  declare — a description that claims more than the manifest grants is a publish-blocking finding.
- Cap at 1024 characters; same rules apply to every per-field `description` inside the schemas.

Write `schemas/input.json` and `schemas/output.json` as JSON Schema draft 2020-12 (`$schema` line
required), then `primitive.yaml`'s `interface` block `$ref`s them — do not inline the schemas into
the manifest.

### Step 4 — Author the workflow

Read `references/workflow-authoring.md` before writing `workflow.yaml` — it condenses the
compiler's sharp edges (unresolved `from:` references, missing END node, non-mutually-exclusive
branches) that `tap validate` lints for. The short version:

- Prefer the deterministic-first ladder for every step: **(1) deterministic transform, (2) cached
  API/browser action, (3) schema-constrained extraction, (4) bounded reasoning, (5) human
  approval** — in that order, never skipping to reasoning because it's easier to write. If a
  signature table or known-string match can make the call, it must, and only genuinely unmatched
  cases reach a `reason:` step (see the gitlab-pipeline-triage example's `signatures.yaml` +
  `classify.star` pattern: known GitLab `failure_reason` strings matched first, the reasoning lease
  invoked only for what's left, and its output passed through a deterministic confidence gate
  before being trusted).
- Starlark `transform:` steps follow the host interface exactly: step params arrive as
  **predeclared globals**, the step's output is whatever you assign to `result`, and there is
  **zero I/O** — no imports, no filesystem, no network, by construction. Don't write a transform
  that "just calls an API real quick" — that's a new `api:` step, not a Starlark builtin.
- **Compose before you fork.** If a step's job is "do what primitive X already does," reference it
  with a `primitive:` step (compiles to the child's projected tool, inline at compile time) instead
  of copy-pasting its workflow. Composition takes the union of *declared* requirements for review,
  never the union of *executable* authority — a parent can't use a child to gain undeclared access,
  and a child can't inherit ambient access from the parent. Fork only when the child's manifest
  can't express what you need even after asking whether the child should grow a new capability.
- A `reason:` step is legitimate to author now even though the runtime's per-node model routing is
  still landing — declare `requires_features: [lease]` and it simply won't be discoverable on a
  runtime that can't enforce it yet. Authoring is decoupled from what today's runtime can run.

### Step 5 — Generate fixtures from the real run

Read `agents/fixture-generator.md`. Fixtures are frozen request/response pairs **pulled from the
actual exploration run**, not invented. Rules that are non-negotiable:

- Redact every secret (tokens, cookies, emails if not load-bearing) but **preserve shape** — same
  field names, same types, same cardinality (empty list, single page, multi-page, error response).
- Never pin a fixture on a volatile value (a live count, "current" timestamp, "latest" anything) —
  pin to a **stable, closed state** (a merged MR, a completed pipeline, a past sprint) so the
  fixture doesn't silently drift out from under the test.
- Any `reason:` step needs a `lease_mock` fixture (a canned lease response) — without one the
  primitive can never be tested offline, since the reasoning lease is a runtime capability, not
  something the package can call directly.
- Cover at minimum: the "nothing happened" / green case, the multi-page case if pagination is in
  play, and one genuinely unmatched case if a `reason:` step exists.

### Step 6 — Write contract tests with dual-mandate grading discipline

Read `agents/grader.md` before writing `tests/contract.test.yaml`, and read it again after writing
the tests — it grades the *tests*, not just the primitive. **A passing test on a weak assertion is
worse than useless: it creates false confidence that later blocks a real bug report.** Every
package needs, at minimum:

- ≥1 happy-path case with `assert:` entries that would fail on "schema-shaped but semantically
  wrong" output (not just "output_schema_valid: true" — that passes for a wrong-but-shaped answer).
- ≥1 negative requirements case: the primitive must error **cleanly and by name** (`schema_invalid`,
  `egress_blocked`, `origin_blocked`) when a declared credential, host, or input is missing or out
  of bounds — never degrade silently. This is what proves the manifest's `requirements` block is
  load-bearing, not decorative.
- If there's a `reason:` step: one case exercising `lease_invoked: true` with the mock, and one
  exercising the deterministic confidence gate (a low-confidence lease response gets demoted, never
  trusted raw).

### Step 7 — Blocking gate sequence, in order

```
tap validate   # schema + requirements-completeness: every fixture-visible network call and
               # credential use must map to a declared requirement, or this fails
tap test       # contract tests green, offline, deterministic — no live credentials
tap publish    # content-hash + push; only after validate and test are both clean
```

Run them **in this order, every time**, and treat every failure as blocking — there is no
advisory/warning mode for these three, matching `package_skill.py`'s own discipline. If `tap
validate` reports an undeclared requirement, the fix is almost always to either declare it
properly in `primitive.yaml` or remove the step that reaches it — never to suppress the finding.
If the `tap` CLI isn't built yet in this environment, still produce every artifact these commands
would check (the requirements-checklist cross-check, the schema validation, the fixture-to-egress
mapping) and say explicitly in your final message which gate you could not run and why.

### Step 8 — Iterate narrowly

When a gate fails, fix only the artifact it's complaining about (a missing `egressHosts` entry, a
weak assertion, an under-specified schema field) and rerun that gate — don't rewrite the whole
package on every failure. Stop when the full sequence is clean, or when you've made no forward
progress across two attempts and need the user's input on a genuine ambiguity (e.g., "should
`retry_pipeline` actually be in scope as a v0.2 write-class sibling, or stay rejected?").

## Reference material (read on demand, not upfront)

- `references/schemas.md` — exact-field-name JSON Schemas for `primitive.yaml`, `workflow.yaml`,
  `requirements-checklist.json`, `tests/contract.test.yaml`, and the fixture conventions. This is
  the one file every other step points back to for field names.
- `references/requirements-taxonomy.md` — canonical vocabulary for credential actions, egress,
  browser requirements, effect classes, `requires_features` tokens, and the declared/rejected
  classification categories used in the checklist.
- `references/workflow-authoring.md` — condensed workflow.yaml authoring rules: step types →
  compile targets, binding/expression rules, the compiler's sharp edges, and the lint suite `tap
  validate` runs.
- `agents/extractor.md` — subagent instructions for step 1.
- `agents/contract-writer.md` — subagent instructions for step 3.
- `agents/fixture-generator.md` — subagent instructions for step 5.
- `agents/grader.md` — subagent instructions for steps 6 and 8 (dual mandate: grade the primitive,
  critique the tests).
- `templates/primitive-skeleton-api/` — starting layout for API-only primitives, shaped after
  `examples/gitlab-pipeline-triage/` in the TAP docs.
- `templates/primitive-skeleton-web/` — starting layout for browser primitives, shaped after
  `examples/web-changelog-watch/` in the TAP docs, including the origin-slot pattern and the
  compile-and-repair drift loop.

## Things that are explicitly out of scope for this skill

- **Binding credentials to a slot** — that's a governed platform action (`tap bind` doesn't exist
  in the CLI on purpose); this skill only ever declares abstract slots.
- **Approving write/destructive primitives** — author them, declare `effects.class` honestly, and
  let the platform's trust×effect matrix decide whether they need human approval. Don't soften a
  write-class primitive's declared effect to make it auto-adopt.
- **Implementing the `tap` CLI itself** — reference its commands (`tap init`, `tap validate`,
  `tap test`, `tap publish`) by name only; a parallel workstream owns building it.
- **Signing or publishing without the gate sequence** — never hand-wave step 7, even under time
  pressure; an unvalidated primitive with an honest-looking manifest is exactly the failure mode
  the manifest exists to prevent.
