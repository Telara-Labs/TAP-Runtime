# Requirements taxonomy

Canonical vocabulary for everything that goes into `primitive.yaml`'s `requirements`/`effects`
blocks and `requirements-checklist.json`'s classifications. Use these terms verbatim — `tap
validate` and the registry match on exact strings, not synonyms.

## 1. Checklist classification

Every observed tool call, host contact, or write is exactly one of:

| Classification | Meaning | Required field |
| --- | --- | --- |
| `declared` | Earns a place in the compiled workflow and a line in `requirements` | `requirement` |
| `rejected` | Observed during exploration but not part of the compiled path | `reason` |

### Common rejection reasons (use these phrasings when they fit; write a specific one when they don't)

- **redundant-signal** — a broader call already carries this data (e.g. `list_jobs` already
  returns `failure_reason`; a per-job `get_job` fetch added nothing new).
- **write-class-out-of-scope** — the call mutates state and this primitive's `effects.class` is
  `read`; leave the action to the caller's own policy instead of declaring it.
- **exploration-only** — used once to understand the domain (e.g. listing all projects to find the
  right one) but not part of the repeatable path; the compiled workflow takes the resolved value as
  a typed input instead.
- **insufficient-actions-coverage** — the call needs a scope/action the credential slot
  deliberately doesn't declare (usually because it's out of scope, not because it was forgotten —
  say which).
- **superseded-by-transform** — a call was used to compute something a deterministic transform can
  derive from data already fetched.

## 2. Credential actions

Actions follow the catalog's `class/operation/resource` convention: `<resource>.<operation>`,
e.g. `pipelines.read`, `jobs.read`, `comments.read`, `issues.write`. Declare the narrowest set the
compiled workflow actually calls — a slot's `actions` list is itself a checkable claim (`tap
validate` fails if a fixture-visible call uses an action not in the list).

Never add an auth-mechanism field to a slot. See `schemas.md#credential-slot` and SKILL.md step 3
for the full rule; the only sanctioned exceptions are `type: browser_session` (a capability class)
and the optional `identity:` constraint (`user_delegated | service`) for target systems whose
semantics differ by acting identity (e.g. audit attribution — "who commented" matters).

## 3. Egress and browser requirements

- `network.egressHosts`: exact hostnames the compiled workflow's `api:` steps reach. Never a
  wildcard, never a CIDR in v0.1 — one entry per host actually contacted, sourced from
  `hosts_contacted` in the checklist.
- `requirements.browser.session`: `none` (public web, no login) or `authenticated` (out of scope
  for the current design per the TAP README decisions log — flag it to the user rather than
  authoring it if the task needs a logged-in session).
- `requirements.browser.origins`: a list of either literal origins or `{slot: <name>}` origin
  slots. Use a slot whenever the primitive should work against more than one instance of the same
  kind of site (bound per-install, same pattern as a credential slot); use a literal origin only
  when the primitive is permanently scoped to one exact site.
- `requirements.browser.navigation`: `same_origin_only` unless the task genuinely needs to follow
  links off-origin, which is rare enough to justify writing down why.
- `downloads` / `uploads` / `clipboard`: default `false`; flip only the ones actually exercised.
  "Browser access" as an undifferentiated blanket grant is not a valid declaration — every one of
  these sub-fields must be set deliberately, not left at a permissive default.

## 4. Reasoning lease vocabulary

- `capabilities`: what the lease needs from the model, e.g. `structured-output`. Keep this list to
  what's actually required — it's a runtime capability negotiation, not a wishlist.
- `dataClasses`: what kind of data crosses into the lease, e.g. `ci-metadata`,
  `public-web-content`, `public-profile-data`, `internal-document-content` (registered v1, for
  authenticated-content extraction like a Confluence page's own body/analytics — not public web
  content). This is what lets policy reason about a lease without reading the workflow — pick the
  most specific class that applies. Declare it in the **manifest's**
  `requirements.reasoning.dataClasses` (normative, v1 ruling 6) — a step-level
  `reason.dataClasses` in workflow.yaml is an optional refinement, not a substitute; policy must
  never need to read the workflow to know what data classes a primitive's reasoning touches.
- `maxTokens`: a hard ceiling, sized to the actual extraction/classification task, not a round
  number picked for comfort.

## 5. Effect classes

`effects.class` is one of, in ascending order of governance weight:

| Class | Meaning | Auto-adopt default (trusted publisher) |
| --- | --- | --- |
| `read` | Never mutates external state | Yes, if idempotent |
| `write` | Mutates external state, reversible | Approval required |
| `destructive` | Mutates external state, not reliably reversible | Approval + HITL always |
| `financial` | Moves money or financial commitments | Approval + HITL always |
| `identity-admin` | Changes identity/access/permissions | Approval + HITL always |

`idempotent` is an independent boolean that refines the class for the runtime's own checks (e.g. a
`write` action that's idempotent can retry safely). **There is no separate `destructive: boolean`
field** — it was removed in v1 (ruling 1) as redundant with `class`: the old boolean's meaning is
now entirely carried by `class: destructive`. `tap validate` rejects the key outright ("removed in
v1; class carries it") if a manifest still writes it. Never declare a class more permissive than
what the compiled workflow does — that's an `effect-mismatch` finding, not a judgment call.

## 6. `requires_features` tokens (capability negotiation)

Declare every engine capability the compiled workflow depends on — a runtime that can't enforce a
listed feature never discovers the primitive, which is the point (fail closed on missing
enforcement, not on missing functionality). Known tokens as of this writing:

| Token | Needed when the workflow has |
| --- | --- |
| `transform.starlark` | any `transform:` step |
| `lease` | any `reason:` step |
| `pagination.page` / `pagination.cursor` / `pagination.offset` / `pagination.relay` | a `paginate: all` step using that pagination style |
| `browser.read` | any `browser:` step |
| `origin_slots` | any `requirements.browser.origins` entry using `{slot: ...}` |
| `egress_slots` | any `requirements.network.egressHosts` entry using `{slot: ...}` (v1 ruling 2 — per-tenant SaaS hosts like `*.atlassian.net`) |

If a workflow needs a capability not in this table, say so explicitly rather than inventing a
token silently — new tokens are a spec change, not a per-package decision.

## 7. Description-safety checklist (§3.1 enforcement, run on every description field)

Before finalizing `metadata.description`, `output_description`, and any per-field schema
`description`, confirm:

- [ ] No IDs, live URLs, tokens, or specific tenant values — shape and effect only.
- [ ] No "if this fails, try..." or any instruction aimed at the calling model's behavior.
- [ ] Everything claimed is a strict subset of `requirements` + `effects` + `outputSchema`.
- [ ] Under 1024 characters, plain text, no invisible/homoglyph Unicode.
