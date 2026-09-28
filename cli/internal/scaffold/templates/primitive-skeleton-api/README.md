# REPLACE_PUBLISHER/REPLACE_NAME

REPLACE — one paragraph: what it does, read-only vs write, what it deliberately never does.

**Why this primitive exists:** REPLACE — name the repeated manual task this compiles (mirrors the
"pipeline babysitting is one of the most-repeated agent tasks observed" framing — say what token
spend / latency this compiled path avoids on every pass after the first).

## Design notes

- **Deterministic first, reasoning last:** REPLACE — name the structured field(s) the deterministic
  step matches against, and (if applicable) what's left over for the bounded reasoning fallback.
- REPLACE any other authoring decisions worth a reviewer's attention (why a call was rejected, why
  a particular confidence threshold was chosen, etc.) — pull these from `requirements-checklist.json`
  rather than re-deriving them.

## Usage (once installed and bound)

Projected MCP tool: `primitive.REPLACE_PUBLISHER/REPLACE_NAME@0.1.0`

```json
{"REPLACE_INPUT_FIELD": "REPLACE_EXAMPLE_SHAPE"}
```

REPLACE — natural composition targets (a scheduled wrapper, a write-class v0.2 sibling behind
approval, etc.), if any are apparent.

## SPEC-FEEDBACK (optional)

If authoring this package against the current workflow-spec/primitive-structure docs surfaced a
format gap or ambiguity, record it here the way the TAP example packages do — this is exactly the
battle-testing the docs ask for. Delete this section if nothing came up.
