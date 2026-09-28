# workflow.yaml authoring rules

Condensed from the TAP workflow-spec draft. `workflow.yaml` is authoring syntax that a compiler
turns into a `WorkflowDefinition` run by the existing production DAG executor — every rule below
exists because the compiler or `tap validate` actually checks it, not as style guidance.

## 1. Step types → what they compile to

| YAML step | Compiles to | Author it when |
| --- | --- | --- |
| `api:` | TOOL node | Any catalog integration call. `tool:` is **unprefixed** (`list_pipelines`, not `gitlab_list_pipelines` — the executor adds the integration prefix). `credential:` names the abstract slot from `primitive.yaml`, never a concrete ID. |
| `browser:` | TOOL node (browser-step integration) | Reading a page with no API, per `requirements.browser`. |
| `transform:` | TRANSFORM node (Starlark) | Any computation — filter, map, join, arithmetic, regex. Reference the program with `expression_from: src/<file>.star`, never inline a multi-line expression in the YAML. |
| `reason:` | MODEL node (the lease) | Genuinely ambiguous extraction/classification only, after signature/known-string matching has had first crack at it. |
| `when:` / guard on a step | BRANCH-derived conditional edge | Skipping a step conditionally. A skipped step's output resolves to `null` for anything downstream that references it — write your transform to handle that (`reasoned == None` pattern, not an unguarded field access). |
| `primitive:` | TOOL node (child projection) | Composing another primitive instead of re-implementing its steps. |
| `approval:` | APPROVAL node | v0.2 write-effect gating only — not part of a v0.1 read-class primitive. |

`code:` steps are the same engine as `transform:` (Starlark, tool-fused builtins keep per-call
gateway enforcement inside the program) — author them the same way; there is no separate
`code.container` authoring path in v0.1.

## 2. Deterministic-first ladder (apply per step, not just per workflow)

1. Deterministic transformation (a Starlark rule, a signature/known-string table).
2. Cached API or browser action.
3. Schema-constrained extraction.
4. Bounded model reasoning (the lease).
5. Human approval.

Concretely: if a classification problem can be solved by matching a structured field against a
known list of strings, write that as a `signatures.yaml`-style data table plus a `transform:` step
— **never** reach for `reason:` to do known-string lookup, and never write a regex/pattern-match
`transform:` to do genuinely semantic judgment. Both directions are wrong for the same reason: using
the cheap tool for a job it can't actually do, or the expensive one for a job it doesn't need to.
Only the residue that the deterministic step can't classify goes to `reason:`, and its output
should pass through a deterministic confidence gate before being trusted (a below-threshold lease
result gets demoted to an explicit `unknown`/`gate_demotion` state, never silently accepted).

## 3. Bindings and expressions

Authoring syntax is `{from: ...}`; the compiler rewrites it to the executor's real dialect:

| You write | Compiles to |
| --- | --- |
| `inputs.x` | `trigger.x` |
| `steps.fetch.items.0.id` | `nodes.fetch.result.items.0.id` |
| a literal value | `literal_value` — literals win over expressions, never combine both on one field |

Rules that `tap validate` enforces:

- Every `from:` reference must resolve against a declared step or input at authoring time —
  dangling references are the single most valuable lint the compiler runs (an unknown-prefix
  expression silently resolves to a literal string at the executor level otherwise, which is a
  silent-failure trap).
- Paths are dot-plus-non-negative-integer-index only — no wildcards, slices, or `[-1]`. Anything
  requiring that goes in a `transform:` step instead.
- Never write `{{ }}` or `${ }` wrappers — bare paths only, even though the executor tolerates the
  old wrapper syntax for backwards compatibility.
- Never reference `workflow.*` or `outputs.*` — both are accidental executor aliases, not real
  paths, and will not do what they look like they do.
- Output bindings (inside a step's own `params:` that reference its *own* result) use bare paths
  only — `steps.*`/`trigger.*` prefixes are meaningless there.

## 4. Compiler contract — the sharp edges to write around

- **Always end with a terminal `outputs:` block that references real step paths** — the compiler
  emits an END node from it; a workflow with no reachable outputs never assembles named results.
- **Every branch/condition list needs exactly one `default:`** — the compiler makes cases mutually
  exclusive even though the underlying executor would otherwise multi-fire every truthy condition.
- **`retry.retry_on` must never be empty** — an empty list retries every error class in the
  executor, including ones that should fail fast (e.g. `schema_invalid`). Default to
  `[timeout, unavailable, rate_limit]` unless you have a specific reason to add more.
- **Retries inside a composed (`primitive:`) step are hoisted to the parent** — inner subflow nodes
  never retry on their own; don't rely on an inner step's `retry:` block firing when it's called
  through composition.
- **Never author `document_parse` or `ocr` node types** — they validate but always fail at
  execution in the current engine.
- Depth cap for composed/inlined primitives is 8 — a composition chain deeper than that is a design
  smell (probably should be a shared library transform, not nested primitives).

## 5. Pagination

Use `paginate: all` on any `api:` step whose tool returns a `_pagination` envelope, and declare the
matching `requires_features` pagination token (see `requirements-taxonomy.md` §6). Contract tests
for a paginated step should assert `pagination_pages_walked` against a fixture that actually spans
multiple pages — a single-page fixture can't catch a pagination bug.

## 6. Lint suite (what `tap validate` actually checks)

- Every `from:` resolves to a declared step or input.
- Every `tool:`/param referenced exists in the pinned catalog version the package was authored
  against (catalog drift is re-checked on catalog upgrades, not just at authoring time).
- Every egress host and credential action implied by the steps appears in `primitive.yaml`'s
  `requirements` — this is the check that makes `requirements-checklist.json` load-bearing rather
  than documentation.
- Branch cases are mutually exclusive with exactly one default.
- No `{{` anywhere in the file.
- No banned node types (`document_parse`, `ocr`).
- Any `format: iso_date` input param carries `examples:` (inherited catalog convention).
- A liveness simulation over branch combinations — the runtime validator only checks static
  reachability, so an unreachable join across a particular branch combination is a lint finding
  here, not a runtime surprise (`WORKFLOW_DEADLOCK`).

## 7. File-inclusion convention

Keep transform expressions and data tables (signature lists, extraction plans) in `src/`, and
reference them from `workflow.yaml` with `expression_from:` / `plan_from:` / `signatures_from:`
rather than inlining multi-line YAML strings. This keeps the workflow file readable and lets the
compiler inline the referenced file at compile time. Name transform files for what they do
(`classify.star`, `verdict.star`, `normalize.star`), not generically (`transform1.star`).
