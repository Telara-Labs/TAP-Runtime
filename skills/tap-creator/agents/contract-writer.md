# Contract-writer agent

Turns a completed `requirements-checklist.json` into the typed interface and requirements
declarations of `primitive.yaml` — step 3 of the tap-creator loop.

## Role

You take the extractor's classified requirements and the deterministic-path sketch, and produce:

- `schemas/input.json` / `schemas/output.json` — the typed I/O contract.
- `primitive.yaml`'s `metadata`, `interface`, `requirements`, and `effects` blocks.

You do not write `workflow.yaml` — that consumes what you produce here, but the actual step
sequence and Starlark transforms are authored afterward, against `references/workflow-authoring.md`.

## Inputs you receive

- `requirements-checklist.json` (must have `unclassified_remaining: 0` — if it doesn't, stop and
  say so; do not proceed on an incomplete extraction).
- The deterministic-path sketch from the extractor.
- The chosen skeleton (`primitive-skeleton-api` or `primitive-skeleton-web`).

## Process

### Step 1: Generalize every observed literal into a typed field

For every value that crossed a tool boundary during exploration (a specific project ID, a specific
board name, a specific date), ask: is this value fixed forever, or does it vary per caller? If it
varies, it's an input field with a generic type (`string`, `int`) and a `description` that explains
its *shape* (e.g. "numeric project ID or URL-encoded path"), never its observed value. Use `enum`
only when the set of valid values is genuinely closed and known (e.g. a fixed status vocabulary the
integration itself defines) — an enum built from "the three values I happened to see" is exactly
the "used a million times" failure the anti-overfit rule targets. When unsure, prefer the wider
type; a schema that's too permissive is a `tap validate` non-issue, a schema that's too narrow
rejects the next legitimate caller outright.

### Step 2: Verify filter semantics against the source system

For **every** filter — any field that narrows, excludes, or scopes what the compiled path returns,
whether it's a caller-facing input or a fixed value the workflow always passes — read the source
system's own documentation (or catalog tool schema) for that field's *full* semantics before
assuming a filter does what its name suggests. A filter that reads as "excludes X" in the tool's
one-line description can have a narrower or differently-scoped real meaning than the name implies.

This is the `allow_failure` class of bug: `gitlab-mr-review-queue`'s regeneration acceptance run
used GitLab's `scope: [failed]` job-listing parameter, trusting the parameter name to mean "jobs
that failed the pipeline" — but a job marked `allow_failure: true` still reports GitLab-API status
`failed` and is still returned by `scope: [failed]`; it just doesn't block the pipeline's overall
status. The ground-truth package instead filtered `allow_failure` jobs out client-side in Starlark,
where a fixture could actually prove the exclusion — the regenerated version had **no fixture able
to catch the misreport**, because the bug was invisible from the API parameter's name alone. Read
the field/parameter's documented behavior, not its label, and prefer a client-side filter you can
fixture-test over a server-side parameter you're trusting blind.

Write down, for every filter in the manifest/workflow: which source-system doc or catalog field you
checked, and whether the filter is applied server-side (parameter) or client-side (Starlark). If a
filter can't be verified against real documentation, treat that as a checklist gap, not a detail to
paper over.

### Step 3: Write the credential slots — capability, never mechanism

For every `declared` credential requirement in the checklist, write a slot with `slot` (a short
name used inside `workflow.yaml`), `integration`, and `actions` (the narrowest set the compiled path
actually calls, per `references/requirements-taxonomy.md` §2). **Do not** add a `type:` field
naming the auth method you happened to use while exploring. If you notice yourself wanting to write
`type: oauth2` because that's what the exploration session authenticated with, stop — that
observation belongs nowhere in the manifest. The only sanctioned mechanism-shaped fields are
`type: browser_session` (under `requirements.browser`, a real capability class) and the optional
`identity: user_delegated|service` constraint, and only when the target system's semantics
genuinely differ by acting identity.

If the checklist shows a `rejected` write-class call, do **not** add a credential scope for it —
its absence from `requirements.credentials` is the manifest's own evidence that the write path was
considered and excluded, matching `effects.class`.

### Step 4: Write the reasoning block, only if a `reason:` step is planned

Include `requirements.reasoning` (`provider: caller`, `capabilities`, `maxTokens`) only when the
deterministic-path sketch actually needs a bounded-reasoning step for genuinely unmatched cases.
If every classification/extraction can be done deterministically, omit the block entirely —
authoring a lease "just in case" is the same overfitting mistake in a different field.

### Step 5: Set `effects` honestly

`class` must match what the compiled steps actually do, not what would be convenient for
auto-adoption. A primitive that only reads is `read`; do not downgrade a primitive that has even
one write-class step in its compiled path (as opposed to its rejected exploration) to `read` to
dodge the approval gate — that's an `effect-mismatch` finding waiting to happen at `tap validate`,
and worse, it's the exact failure mode 06-security.md's trust×effect matrix exists to prevent.

### Step 6: Write descriptions against the safety checklist

Run `metadata.description`, `output_description`, and every schema-field `description` through
`references/requirements-taxonomy.md` §7 before finalizing. In particular:

- Say what the primitive fetches/classifies/returns and what it explicitly never does (mirrors the
  "read-only; never retries, cancels, or triggers" pattern in the gitlab example) — the negative
  half of the description is as load-bearing as the positive half for a tool-selecting model.
- Don't reference any ID, live URL, or example value from the exploration session.
- Keep `output_description` scoped to describing shape and documented empty/null cases (e.g. "X is
  empty when nothing matched") — never a claim about accuracy or completeness the schema can't
  back up.

## Output

- `schemas/input.json`, `schemas/output.json` (draft 2020-12, per `references/schemas.md`).
- `primitive.yaml` with `metadata`, `interface`, `requirements`, `effects` filled in (leave
  `execution` and `artifactDigest` to the skeleton defaults / publish-time values).

## Self-check before handing off to workflow authoring

- [ ] Every `declared` entry in the checklist has a corresponding requirement in the manifest.
- [ ] No `rejected` entry has a matching requirement (that would mean it snuck back in).
- [ ] Every filter's real source-system semantics were checked, not assumed from its name (the
      `allow_failure` class of bug).
- [ ] No credential slot names an auth mechanism.
- [ ] `effects.class` matches the compiled path, not the exploration path.
- [ ] Every description passes the §7 checklist.
