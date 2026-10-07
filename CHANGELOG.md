# TAP runtime migration history

This records caller-facing changes from the tagged source history. It is
not a claim that every client passed acceptance, or that every historical
release is compatible. For the complete source change, follow each compare
link. Runner versions, primitive versions, and the manifest API are distinct;
see [version review](docs/versioning.md).

## 0.2.6

- `tap_load` now returns a bounded flat preview of possible host connections
  and base write gates using the same admission resolver as execution. It
  shows missing, ambiguous and refused aliases without running tools,
  prompting or saving choices. Dynamic dispatcher effects are checked again
  during execution. Clients without live inventory report it as unavailable.

[0.2.5 to 0.2.6 source changes](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.2.5...v0.2.6).

## 0.2.2

- `tap_evidence` now includes the executed package digest, declared permissions
  from a saved manifest, and resolved identities for journaled tool attempts.
  Exact saved YAML is opt-in with `include_manifest: true`. Large fields are
  explicitly omitted and legacy snapshots remain unavailable. Runtime request
  arguments and replies are excluded. Existing resume and unknown-write
  behavior is unchanged.

[0.2.1 to 0.2.2 source changes](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.2.1...v0.2.2).

## 0.2.1

- Added read-only `tap diff [--json] OLD NEW`: exact package identities,
  manifest field changes, entrypoint hashes, authority additions, and
  same-version changed bytes. Exit 1 means review required, not a proof of
  incompatibility. No package is executed or approved by this comparison.
- Added this runtime migration history and runnable primitive version examples.

[0.2.0 to 0.2.1 source changes](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.2.0...v0.2.1).

## 0.2.0

**Migration from 0.1.26 and earlier:** `tap_run` can return while a primitive
is still running. After 20 seconds it returns a handle; call `tap_result`
with that handle until the run ends. Do not restart the run or perform its
steps by hand. Refresh the client's tool list after upgrading, including
any explicit tool allowlist. Fast runs still return their result inline.

This behavior first appeared in **0.1.27**. Between 0.1.27 and 0.2.0 only
the README and npm version changed; 0.2.0 did not introduce another runner
implementation change. Tool consumers upgrading across this boundary must
support the handoff rather than assume every `tap_run` response is final.

[0.1.26 to 0.2.0 source changes](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.26...v0.2.0)
([0.1.27 to 0.2.0](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.27...v0.2.0)).

## 0.1.26

Codex authoring and reuse guidance was revised, including how the agent
records the earlier request when saving a primitive. Terminal setup output
was shortened. No change to the manifest API.

[0.1.25 to 0.1.26](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.25...v0.1.26).

## 0.1.25

Discover, setup, and upgrade gained terminal progress animations.

[0.1.24 to 0.1.25](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.24...v0.1.25).

## 0.1.24

**Input migration:** for a primitive whose input schema names properties
or required fields, `tap_run.args` must contain exactly one JSON object
encoded as a string. Missing required fields and other argument shapes
are refused before execution. Replace flag-style arguments with, for
example, `args: ["{\"name\":\"reader\"}"]`, using the actual schema from
`tap_load`. This check validates the object shape and required field
presence; it is not full JSON Schema validation. Programs with no named
input fields retain their argument convention.

`--version` and its aliases print the version. Repeated-task matching was
also revised.

[0.1.23 to 0.1.24](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.23...v0.1.24).

## 0.1.23

Added `tap upgrade` for npm installations and guidance for other install
paths. Unknown bare subcommands now give command help rather than an
attempt to open a primitive folder. Use `./folder` for a relative package
path when it could be confused with a command.

[0.1.22 to 0.1.23](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.22...v0.1.23).

## 0.1.22

Search matches individual query words. Saved primitives can record the
earlier request; the server can suggest authoring when a task recurs.

[0.1.21 to 0.1.22](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.21...v0.1.22).

## 0.1.21

Setup reconnects saved primitives and installs authoring guidance. A pinned
tool can bind under a different server connection name when exactly one
server offers that tool; ambiguity still requires a choice. Review the
resolved server when moving a package between clients. Claude connector
discovery waits before treating a connection as missing.

[0.1.20 to 0.1.21](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.20...v0.1.21).

## 0.1.13 through 0.1.20

Behavior changes that can make an earlier workflow refuse or stop:

- **0.1.14:** clients without elicitation require owner trust for the exact
  package digest before execution. Trusting a package does not authorize
  writes or fetch origins. Review it and follow the explicit owner trust
  procedure in [headless trust](docs/headless-and-sharing.md).
- **0.1.16:** browser connector attempts are counted even when they fail,
  batch requests are bounded, and stalled VS Code tool calls have a
  two-minute deadline. Do not depend on unlimited retries or stalled calls.
- **0.1.18:** finite CLI approval ceilings are consumed rather than renewed.
  A run that exhausts the approved number stops for further authorization.
- **0.1.19:** bare Windows volume roots are refused.
- **0.1.20:** releases include the signed VSIX built from a clean export.

0.1.13 added explicit digest-scoped headless fetch grants and repaired
installer invocation. 0.1.15 improved primitive identification in the server
chooser; 0.1.17 corrected Discover help behavior. None of these version
labels independently proves a package or host connection is safe.

[0.1.12 to 0.1.20 source history](https://github.com/Telara-Labs/TAP-Runtime/compare/v0.1.12...v0.1.20).
For earlier versions, use the [full release history](https://github.com/Telara-Labs/TAP-Runtime/releases)
and compare the exact tags you intend to adopt.
