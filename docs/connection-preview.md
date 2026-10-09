# Connection previews

Call `tap_load` with an installed primitive's exact `ref` and `digest` for a
compact text summary of inputs, permissions and connections. TAP uses standard
MCP text content, so clients do not need a custom renderer. Exact identities
and complete input constraints are retained; empty declaration sections and
output schemas are kept out of the default view. Each binding shows its
resolution status, base effect and any approval or runtime-check requirement.

For scripts or full manifest inspection, add `"detail": true`. This returns
the complete JSON contract, including `connection_preview`, output schemas,
capability definitions and pins. There is one payload per response, avoiding
duplicate text/structured content. This is an explicit change to the default
text format: existing JSON parsers must request `detail: true`.

```json
{
  "status": "available",
  "scope": "possible declared bindings; execution rechecks connections and per-call effects",
  "connections": [
    {
      "alias": "threads",
      "declared_effect": "read",
      "optional": false,
      "status": "resolved",
      "server": "mail",
      "tool": "search_threads",
      "annotated_effect": "read",
      "base_effect": "read",
      "base_approval_required": false,
      "requires_runtime_check": false
    }
  ],
  "truncated": false
}
```

These are possible bindings for declared aliases, not a prediction of which
branches the program will take. The host inventories connections once and
uses the same admission resolver as a run, including saved server choices,
contract checks, client denials and client approval rules. Inspection never
executes a tool, asks for approval, saves a choice or trusts a package.

Each row has one of these statuses.

| Status | Meaning |
| --- | --- |
| `resolved` | Admission found a usable binding in the current inventory |
| `unavailable` | No binding is available for the alias |
| `ambiguous` | Equally fitting servers need an owner choice |
| `unresolved` | Admission refused the alias, for example a denial or contract mismatch |

An unresolved row omits effect and approval conclusions. Optional aliases are
included even when a run would skip them. A missing or ambiguous required
alias can refuse a run; the overall preview's `available` status only means
the inventory could be inspected.

`base_effect` and `base_approval_required` describe admission's effect gate.
An unannotated tool is treated as at least a write. For a dispatcher,
`requires_runtime_check` is true and a known fixed `operation` is included.
The nested operation and arguments are checked during a real call; the base
effect alone is not approval for that operation. No catalog probes are
executed during preview.

Execution always resolves again. Connections, schemas, annotations and owner
choices can change after inspection. The preview does not approve any call
or guarantee that the program will run successfully or return a correct result.
It concerns tools; file, command and network declarations remain alongside it
in the ordinary `tap_load` response.

The preview includes at most 100 aliases and 16 KiB of encoded JSON. It omits
whole rows and marks `truncated` rather than shortening tool identities.
The complete `tap_load` response retains its existing 64 KiB limit. Live
inventory failures and clients that expose only declared pins report
`inventory_unavailable` with no claimed connections. Invalid tool declarations
report `invalid_declarations`. Host schemas, credentials, runtime payloads and
raw connection errors are excluded from preview output.
