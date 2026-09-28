# REPLACE_PUBLISHER/REPLACE_NAME

REPLACE — one paragraph: what page/site this watches, what it returns, and the fact that it's
read-only and stays within the bound origin only.

**The economics in one line:** REPLACE — name the compiled path's cost (zero reasoning tokens per
pass) versus what triggers the fallback lease (a site change / drift), and note that the same drift
event opens a repair proposal so the *next* pass is compiled again.

## How the pieces demonstrate the web-TAP design

- **Origin slot** (`REPLACE_ORIGIN_SLOT`): bound at install like a credential — the input `url` is
  runtime-checked against the bound origin(s); `origin_blocked` fires before any navigation.
- **Browser requirements are granular**: `session: none`, `same_origin_only` navigation, no
  downloads/uploads/clipboard unless the task genuinely needs one flipped on (and if so, say why).
- **Compile-and-repair loop**: `src/extract_plan.yaml` is the compiled path; drift triggers bounded
  text capture, a schema-constrained lease extraction to keep the current pass alive, and a repair
  proposal for review — the compiled path is never hot-patched in place.
- **Anti-bot pages are drift, not retry fodder** — REPLACE if this primitive's target has such
  pages; otherwise note that none were observed.
- **Provenance per entry**: every entry says whether cached selectors or the reasoning fallback
  produced it.

## Usage (once installed with `REPLACE_ORIGIN_SLOT` bound)

Projected MCP tool: `primitive.REPLACE_PUBLISHER/REPLACE_NAME@0.1.0`

```json
{"url": "REPLACE_EXAMPLE_URL_SHAPE", "since": "REPLACE_ISO_DATE"}
```

## SPEC-FEEDBACK (optional)

If authoring this package surfaced a format gap or ambiguity in the current workflow-spec/
primitive-structure docs, record it here. Delete this section if nothing came up.
