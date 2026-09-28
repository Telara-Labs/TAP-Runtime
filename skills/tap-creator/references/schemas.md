# TAP artifact schemas

Exact field names for every artifact `tap-creator` emits. `tap validate` and the registry trust
these byte-for-byte — do not rename, nest differently, or add undeclared keys. Where a field is
marked **closed**, unknown sibling keys are rejected outright (mirrors skill-creator's
`ALLOWED_PROPERTIES` retrofit — declare the allowlist up front instead of discovering frontmatter
sprawl later).

## Contents

- [primitive.yaml](#primitiveyaml) — the manifest
- [Credential slot](#credential-slot)
- [workflow.yaml](#workflowyaml) — the compiled-to-`WorkflowDefinition` source
- [schemas/input.json, schemas/output.json](#schemasinputjson-schemasoutputjson)
- [requirements-checklist.json](#requirements-checklistjson)
- [tests/contract.test.yaml](#testscontracttestyaml)
- [Fixtures](#fixtures)

---

## primitive.yaml

Top-level keys (**closed** — this is the full allowlist):

```yaml
apiVersion: primitives.telara.dev/v1     # current convention; see naming note below
kind: Primitive

metadata:
  name: string                # kebab-case, no publisher prefix (publisher is a separate field)
  publisher: string            # reverse-DNS, domain-verified, e.g. dev.telara
  version: string              # semver
  description: string          # <=1024 chars; shape+effect only, see SKILL.md step 3 rules
  output_description: string   # <=1024 chars; same rules
  license: string               # SPDX identifier, e.g. Apache-2.0
  source: string                 # git URL
  artifactDigest: string          # sha256:...; literal "sha256:TBD-at-publish" pre-publish
  forkOf: string | null

interface:
  inputSchema: {"$ref": "schemas/input.json"}
  outputSchema: {"$ref": "schemas/output.json"}

requirements:
  browser: {...}               # omit entirely for non-browser primitives; see below
  network:
    egressHosts: [string | {slot: string}]   # exact hostnames, OR an egress-host SLOT (v1 ruling 2)
                                               # for a per-tenant SaaS host with no fixed literal
                                               # (e.g. Jira/Confluence's *.atlassian.net) — bound at
                                               # install exactly like a browser origin slot.
                                               # [] for browser-only primitives.
  credentials: [CredentialSlot] # [] for public-web-only primitives
  reasoning:                    # omit entirely if no reason: step exists
    provider: caller             # always "caller" in v0.1 — never a primitive-held key
    capabilities: [string]        # e.g. structured-output
    maxTokens: integer
    dataClasses: [string]          # OPTIONAL — data-sensitivity tokens the lease is exposed to,
                                     # e.g. ci-metadata, public-web-content, internal-document-content
                                     # (registered v1; see requirements-taxonomy.md). Normative here
                                     # in the manifest -- policy must not need to read workflow.yaml
                                     # to know what data classes a primitive's reasoning touches.
                                     # Step-level `reason.dataClasses` in workflow.yaml is an
                                     # optional refinement, not a substitute.

effects:
  class: read | write | destructive | financial | identity-admin
  idempotent: boolean
  # NOTE: there is no `destructive: boolean` field. It was removed in v1 (ruling 1) as redundant
  # with `class` -- `class: destructive` carries what the old boolean meant. `tap validate` rejects
  # the key outright with "removed in v1; class carries it" if you still write it.

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: integer
  resumable: boolean            # compiler-derived; false for read-class v0.1 primitives
  may_suspend: [string]          # compiler-derived; [] unless approval steps exist
  requires_features: [string]    # capability-negotiation tokens, see requirements-taxonomy.md
  triggers: [manual | schedule | event]   # OPTIONAL — machine-checkable execution-mode metadata
                                            # (v1 ruling 5); e.g. a monitoring-shaped primitive meant
                                            # to run on a recurring schedule declares
                                            # triggers: [schedule, manual]

suggested_mode: autonomous | supervised   # non-binding publisher hint, never enforced alone
```

`requirements.browser` (only present when a `browser:` workflow step exists):

```yaml
browser:
  session: none | authenticated
  origins:
    - slot: string               # ORIGIN SLOT — bound at install like a credential; OR:
    # - "https://exact.origin"   #   a literal origin, for a primitive scoped to one fixed site
  navigation: same_origin_only | unrestricted
  downloads: boolean
  uploads: boolean
  clipboard: boolean
```

**Naming note:** the current, decided convention (per the TAP README decisions log) is
`apiVersion: primitives.telara.dev/v1` with an explicit `metadata.publisher` field and a bare
`metadata.name` — the full identifier is `<publisher>/<name>`. The first example package
(`gitlab-pipeline-triage`) predates this decision and uses `v1alpha1` with a dotted
`metadata.name` (e.g. `gitlab.pipeline-triage`) and no `publisher` field; treat that shape as
legacy, not a second valid form. Author new primitives with the `v1` + `publisher` shape shown
above (this is what both templates in this skill use).

## Credential slot

```yaml
- slot: string              # abstract name used inside workflow.yaml's `credential:` field
  integration: string        # catalog integration id, e.g. gitlab, jira, confluence
  actions: [string]           # class/operation/resource tokens, e.g. pipelines.read, jobs.read
  identity: user_delegated | service   # OPTIONAL — only when target semantics differ by actor
  accepts: [string]            # OPTIONAL — auth-method allowlist for the rare surface that
                                 # behaves differently per method (e.g. [oauth2]); using this
                                 # without a documented reason is a publish-time lint warning
```

**Never** add a bare `type:` field naming an auth mechanism (`oauth2`, `personal_access_token`,
`api_key`, `basic`) to a credential slot — that is the overfit this schema exists to prevent (see
SKILL.md step 3 and `03-primitive-structure.md` §3.0). The sole exception is
`type: browser_session`, which is a distinct capability class, not an auth flavor, and belongs
under `requirements.browser`, not `requirements.credentials`, in the v1 shape above.

## workflow.yaml

Top-level keys (**closed**):

```yaml
apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  <name>:
    type: string | int | bool | object | string[] | int[]   # object added v1 ruling 5
    required: boolean            # default false
    default: <matching type>
    description: string
    min: number                   # numeric types only
    max: number                   # numeric types only
    examples: [<matching type>]

# RESERVED: `now` (v1 ruling 3). An ISO-8601 datetime the runtime auto-stamps into the execution
# context every run -- available as {from: inputs.now} WITHOUT ever declaring it in `inputs:`.
# If you do declare it (e.g. to attach a description), it must never be `required: true` --
# `tap validate` rejects that, since a caller never supplies it. This is what makes staleness/
# monitoring primitives (e.g. linear-cycle-health-monitor) authorable deterministically instead of
# needing a caller-supplied threshold for "now".

steps:
  - id: string                    # unique; referenced by other steps as steps.<id>
    api: {...}                     # exactly one of api/browser/transform/reason/when/approval
    browser: {...}
    transform: {...}
    reason: {...}
    when: <condition>              # optional guard; step's own output resolves to null if skipped
    retry:
      max_attempts: integer
      initial_backoff_seconds: number
      retry_on: [timeout, unavailable, rate_limit]   # never leave this empty — see workflow-authoring.md

outputs:
  <name>: {from: steps.<id>.<path>}
```

Per-step-type shapes are in `workflow-authoring.md` — this file only fixes field names.

## schemas/input.json, schemas/output.json

Plain JSON Schema, draft 2020-12. Required top-level:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["..."],
  "properties": { "...": { "type": "...", "description": "..." } }
}
```

Every field with a non-obvious meaning gets a `description` — these are scanned under the same
attack-surface rules as `metadata.description` (shape/effect only, no IDs, no live examples).

## requirements-checklist.json

```json
{
  "artifact": "tap-creator requirements classification — every observed call classified as declared or rejected; tap validate cross-checks this file against the manifest and fixtures",
  "source_transcript": "string — one line naming the session this was mined from",
  "tool_calls_observed": [
    {
      "call": "string — e.g. 'gitlab list_pipelines'",
      "classification": "declared | rejected",
      "requirement": "string — REQUIRED when classification is declared; the credential/egress/reasoning requirement it maps to",
      "reason": "string — REQUIRED when classification is rejected; why it didn't earn a place in the compiled path"
    }
  ],
  "hosts_contacted": [
    {"host": "string", "classification": "declared | rejected"}
  ],
  "writes_observed": [
    {"call": "string", "classification": "declared | rejected", "reason": "string"}
  ],
  "unclassified_remaining": 0
}
```

Field rules:

- `tool_calls_observed[]` and `writes_observed[]`: every entry has **either** `requirement`
  (declared) **or** `reason` (rejected), never both, never neither.
- `unclassified_remaining` must be `0` before step 2 of the loop begins. This is the field `tap
  validate` (and this skill) treat as the blocking gate — not the presence of the file itself.
- `writes_observed` is `[]` for a read-class primitive, but the key is still present — an absent
  key reads as "nobody looked," not "nothing happened."

## tests/contract.test.yaml

```yaml
cases:
  - name: string                       # descriptive, unique
    fixtures: fixtures/<file>.json      # OR omit for input-only negative cases
    fixture_variant: string              # OPTIONAL — selects a named variant inside the fixture file
    input: {...}                          # OPTIONAL override of default input
    now: string                            # OPTIONAL — ISO-8601 datetime; pins the reserved `now`
                                             # input for this case (v1 ruling 3), injected as
                                             # inputs.now without needing a workflow.yaml declaration
    lease_mock: string                     # OPTIONAL — key inside the fixture file for reason: steps
    override_step: {id: string, egress_host: string}   # OPTIONAL — for undeclared-egress negative cases
    expect:
      output_schema_valid: boolean
      lease_invoked: boolean
      lease_max_tokens_respected: integer
      pagination_pages_walked: integer
      error: schema_invalid | egress_blocked | origin_blocked | credential_missing
      steps_executed: integer
      output: {<dot.path>: <expected value>}
    assert:
      - string   # free-text assertions the grader checks against actual output, e.g.
                 # "failed_jobs contains {name: ..., category: ...}"
```

`expect.output` entries are exact-value checks; `assert` entries are the grader's job — use them
for containment/ordering/negative-containment checks that a flat key-path can't express (see
`agents/grader.md`).

## Fixtures

No fixed schema — fixtures mirror whatever shape the real tool call returned, with secrets
redacted and values frozen to a stable, non-volatile state. Two conventions are load-bearing:

- Multi-page API fixtures nest under a `pages: [...]` array, each page carrying its own
  `_pagination` envelope (see `gitlab-pipeline-triage/tests/fixtures/failed_pipeline.json`), so
  contract tests can assert `pagination_pages_walked`. **`_pagination`'s minimal contract is pinned
  (v1 ruling 7):** `{has_more: boolean, next_cursor: string | null}` — extra integration-specific
  fields (e.g. a total count) are permitted alongside these two, but every fixture must carry at
  least these two, in every page, so pagination-walk assertions are portable across integrations.
- A fixture file may carry named variants alongside the primary payload (e.g.
  `reasoning_lease_response`, `reasoning_lease_response_low_confidence`, `variant_with_empty_rows`)
  — reference them via `lease_mock:` / `fixture_variant:` in the test case rather than duplicating
  near-identical fixture files.
