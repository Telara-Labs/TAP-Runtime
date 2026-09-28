# tap

The authoring-side CLI for TAP (Trusted Agent Primitives). Covers the
source side of the primitive lifecycle -- scaffold, validate, test, diff,
explain -- for engineers' repos and coding agents. It does not bind
credentials, manage keys, or execute in production; see
[`telara-documentation/architecture/tap/04-cli.md`](../../telara-documentation/architecture/tap/04-cli.md)
for what is deliberately out of scope.

## Install

```
cd telara-tap/cli
go build -o tap ./cmd/tap
```

Requires Go 1.24+ (developed against 1.26). No external services, no
network access, no credentials -- `tap validate` and `tap test` run
entirely offline against files in the package directory.

## Commands

### `tap init <name> [--web] [--dir <path>]`

Scaffolds a package that validates and tests clean out of the box.

- `<name>` is either `local-name` or `publisher/local-name` (reverse-DNS,
  per the [naming convention](../../telara-documentation/architecture/tap/README.md)).
- `--web` scaffolds the browser-step shape (an origin slot + `browser:` +
  `transform:` step) instead of the default api shape (a single
  `transform:` step).
- `--dir` overrides the target directory (default: the local part of
  `<name>`).

### `tap validate [dir] [--json]`

Lints `primitive.yaml` + `workflow.yaml` against the spec:

- **Manifest schema**: a closed field allowlist at every nesting level
  (unknown keys are rejected), required fields, `effects.class` in
  `{read, write}`, entrypoint existence.
- **JSON-Schema validity** of `interface.inputSchema` / `outputSchema`
  (`$ref` or inline), compiled under JSON Schema 2020-12.
- **Description rules** (03 §3.1): `metadata.description` /
  `output_description` and every per-field schema `description` are
  scanned for length (≤ 1024 chars), `{{` template syntax, embedded
  URLs/IDs/tokens, imperative injection patterns, and invisible/homoglyph
  Unicode.
- **Credential-slot rules** (03 §3.0): a slot must declare `actions`
  (capability), not just a `type`; an unjustified `accepts:` auth-method
  pin is a warning, not an error.
- **Workflow lint suite** (09 §10): every `from:` reference resolves to a
  declared input or step; every `src/`-referenced file (`expression_from`,
  `*_from` params, `plan_from`) exists; `when:`/branch conditions match the
  executor's real grammar (`==`/`!=` with a literal RHS, or bare
  truthiness); `retry_on` is non-empty when explicitly present; no `{{`
  anywhere; no banned node types (`document_parse`, `ocr`); step dependency
  graphs (`from:`/`after:`/`when:`) are acyclic; branch steps have exactly
  one `default:`; `format: iso_date` inputs carry `examples`.
- **Cross-package checks**: every credential slot / origin slot a step
  references is declared in `requirements`; a write-verb-shaped tool inside
  a `effects.class: read` manifest is an `effect-mismatch`.

Findings use the failure taxonomy from `04-cli.md` §3: `schema`,
`undeclared-requirement`, `effect-mismatch`, `permission-expansion`,
`provenance` -- each with a stable `blocker` name and, where useful, a
resolution hint. `--json` emits `{dir, ok, findings[]}`.

### `tap test [dir] [--json] [-k <substring>] [--bind slot=origin ...]`

Executes `tests/contract.test.yaml` offline against `tests/fixtures/*`:

- `api:`/`browser:` steps are replayed from fixtures keyed by tool name
  (api) or step id / `browser_extract` (browser); `paginate: all` walks a
  fixture's `pages: [...]` envelope to exhaustion.
- `transform:` steps **actually execute** their Starlark program via
  `go.starlark.net` -- params arrive as predeclared globals, `*_from` params
  load a YAML data file and bind it to the global named by stripping
  `_from` (unwrapping a single-key top-level map matching that name), and
  the `result` global becomes the step's output. No I/O or imports are
  exposed; a 2M-step execution ceiling is enforced per step.
- `when:` conditions are evaluated against real bindings; a skipped step's
  output resolves to `null` downstream, matching the executor's documented
  behavior.
- `lease_mock:` supplies a `reason:` step's output from a named fixture
  key; a `reason:` step reached with no `lease_mock` for that case leaves
  its output `null` rather than failing the case (some cases only care
  about an earlier step's outcome).
- `fixture_variant:` selects a `variant_<name>` block from the fixture file
  in place of the primary record.
- `expect:` blocks are asserted: `output_schema_valid`, `lease_invoked`,
  `lease_max_tokens_respected`, `lease_input_max_chars`,
  `pagination_pages_walked`, `drift_event_emitted`, `browser_retries`,
  `steps_executed`, `error` (one of `schema_invalid` / `egress_blocked` /
  `origin_blocked`), dot-path `output:` values (`a.b.0.c`, `.length`), and a
  small `assert:` DSL (`<path> [NOT] contains {k: v, ...}`, and the literal
  `<path> sorted by <field> descending, undated last`).
- `-k <substring>` runs only cases whose name contains the substring.
- `--bind slot=origin` (repeatable) supplies an offline binding for a
  browser origin slot, so `origin_blocked` cases are testable without a
  real platform bind -- see **DECISION** below. A contract file may also
  declare a `bindings:` top-level map as a checked-in default; `--bind`
  overrides it.

Report is per-case pass/fail with failure messages (schema mismatches show
expected vs. actual). `--json` emits the full `RunReport`.

### `tap diff <dirA> <dirB> [--json]`

Interface diff (input/output schema property sets) and PERMISSION diff:
credential slots added/removed, per-slot actions added/removed, egress
hosts added/removed, origin slots added/removed, reasoning capabilities
added, `maxTokens` delta, and `effects.class` read→write. Any *addition*
is a permission expansion, printed with a `+` and flagged; exit code 1 with
a `[permission-expansion]` note if one is detected (mirrors the
`--acknowledge-expansion` gate `04-cli.md` describes for `tap publish`).

### `tap dev [dir] [--case <name>] [--bind slot=origin ...] [--json]`

Replays one contract-test case (by name, or the first case) and prints a
step-by-step trace: each step's type, skip/run status, resolved bindings,
which manifest requirements it exercised (credential slot, origin slot,
reasoning lease), and its output.

### `tap explain [dir] [--json]`

Renders `primitive.yaml` as a human-readable permission panel: description,
effects, credentials + actions, egress hosts, browser session/origins/
navigation/downloads/uploads/clipboard, reasoning lease (provider,
maxTokens, capabilities), execution runtime/entrypoint/timeout/
`requires_features`.

### `tap publish [dir]`

Stub. Always exits non-zero: *"publish requires a TAP registry (not yet
available); package is publish-ready if `tap validate` and `tap test`
pass"* -- per `04-cli.md` §2, publish/bind/key-management are explicitly
platform actions, not CLI ones, until the registry ships.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success (validate/test found no errors; diff found no expansion) |
| 1 | findings/failures (validate errors, test-case failures, diff permission-expansion, publish stub) |
| 2 | usage error (bad flags/args) |
| 3 | internal error (package didn't load, file I/O failure, etc.) |

Flags may be given before or after positional arguments (`tap init <name>
--web` and `tap init --web <name>` both work).

## DECISIONS (not dictated by the spec docs)

- **Bind-simulation for `tap test`**: implemented as a repeatable
  `--bind slot=origin` CLI flag (plus an optional `bindings:` block in
  `contract.test.yaml` that the flag overrides), rather than requiring a
  real platform bind. This is what makes `web-changelog-watch`'s
  `url_outside_bound_origin_blocked_at_step` case testable offline; without
  any binding for a slot, tap skips the origin check for that slot rather
  than failing closed, so packages that don't care about origin
  enforcement in a given test run still execute normally.
- **Missing-input auto-fill only applies when a case omits `input:`
  entirely.** A case that provides a partial `input:` (the
  `missing_required_input_fails_at_gate` negative test) is used exactly as
  written -- tap never fills in a field the test author deliberately left
  out. A case with no `input:` at all gets manifest/workflow defaults, then
  the first declared `examples[0]` for a still-missing required field,
  then (only for a `url`-shaped field tied to a single bound origin slot)
  the bound origin itself.
- **`assert:` is an intentionally minimal DSL**, not a general expression
  language: `<path> [NOT] contains {k: v, ...}` and one literal template
  (`<path> sorted by <field> descending, undated last`). It covers both
  golden packages; anything richer should become a first-class `expect:`
  key instead of DSL creep.
- **`resolve.AllowGlobalReassign = true`** for the Starlark sandbox.
  `go.starlark.net` disables top-level variable reassignment by default;
  both example packages' transforms reassign a top-level variable
  sequentially (a normal, deterministic pattern for a pure function body,
  not a hermeticity concern -- each `Exec` call gets a fresh thread and
  fresh globals).

See the final task report (delivered alongside this change) for the full
FINDINGS list, including the invalid-YAML `assert:` line quirk both example
packages share (tap preprocesses it rather than editing the examples) and
the `normalize.star` `since`-filter bug that was fixed because it
contradicted that package's own contract test.
