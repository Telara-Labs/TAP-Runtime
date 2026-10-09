# TAP runtime migration history

This records caller-facing changes from the tagged source history. It is
not a claim that every client passed acceptance, or that every historical
release is compatible. For the complete source change, follow each compare
link. Runner versions, primitive versions, and the manifest API are distinct;
see [version review](docs/versioning.md).

## Unreleased

- **The runner provides the browser.** A primitive declares one tool,
  `{alias: browser, capability: tap.browser.use, effect: write}`, and calls
  `navigate`, `read` (a page function that returns JSON), `act` (`click`,
  `scroll_into_view`, `insert_text`), `wait` and `close`. The runner finds the
  browser the client already lends (Claude in Chrome, Codex's browser, or a
  configured Playwright MCP server, including one started with `--extension`,
  and Playwright tools listed with a prefix, as VS Code does), keeps the first
  that passes the primitive's `ready` check (for example, signed in to the
  site), opens and closes a tab of its own, and handles each browser's limits:
  Codex evaluates page code read-only and stops it after about three seconds,
  so its actions go through its own locator; Claude in Chrome cuts long
  results, so long answers are read back in parts. A primitive no longer
  carries code for each client. Navigation and actions need approval; reads
  do not. `insert_text` replaces an element's content the way typing would,
  so a primitive can fill a field and read it back before it submits.
  Claude Code is started with `--chrome` only for a primitive that uses the
  browser or pins Claude in Chrome. Codex lends its own browser only to a
  runner started inside a Codex turn; otherwise a configured Playwright
  server is used, and a refused Codex call returns a short message instead
  of the tool's whole manual.

- **Capabilities resolve on every client without pins.** On clients that do
  not list their tools to the runner (Gemini CLI), each declared capability
  is resolved on its own: a mapping kept from an earlier run, then servers in
  the client's own configuration that fit it (a Playwright or Claude in
  Chrome server for the browser, a Telara gateway's catalog for its
  operations), then one question to the agent, "which of your tools does
  this?", answered with one tool name. No tool list is sent to the model.
  Answers are checked against the client's connected servers and kept per
  client (`tap bind`); a tool's effect comes only from its server, never from
  the answer. Nothing found stops the run with `blocked:` and the
  capability's name. Pins still work, as overrides. Gemini's servers are read
  from `GEMINI_CLI_HOME` and `GEMINI_CLI_SYSTEM_SETTINGS_PATH` when set, as
  Gemini itself does. A pin on a gateway's `telara_execute_action` now also
  lends that gateway's catalog, so its reads are checked instead of refused;
  a capability that names the dispatcher (`gitlab.execute_action`) is refused
  with the operation to declare instead.

- **Primitives that call tools now run from more agents, unmodified.**
  - GitHub Copilot CLI: each tool runs through Copilot's own headless session
    and connections, with no model turn. Copilot applies the person's
    permission rules; when Copilot asks, the runner answers only for the call
    it is making, which the person has already allowed through TAP's prompt.
    Interactive Copilot CLI shows that prompt; under `copilot -p` nothing can
    be asked and a write is refused before it reaches Copilot.
  - OpenCode and Kilo: `tap install` adds a small plugin, `tap-relay.js`, to
    the client's plugin folder. Inside the person's session it connects to
    the MCP servers the session is configured with, using the session's
    resolved configuration and the sign-ins the client stored, and runs the
    runner's calls there. The runner reaches it over a Unix socket only that
    user can open, checks the socket's owner and the session's folder, sees
    server names, tool lists and results but never headers, environment
    values or tokens, and is refused when no TAP run is in progress in that
    session.
  - Crush and Cursor, and OpenCode or Kilo outside a session: the runner
    connects to the servers in the client's own configuration that carry no
    secret. A server with environment values, headers or a sign-in, a
    disabled one, or (Cursor) one not approved in Cursor is not used, and the
    runner says why.
  - The runner no longer starts a Kilo server of its own: it set Kilo's
    password and feature flag in the environment and listened on a TCP port.
    Kilo no longer needs `KILO_EXPERIMENTAL_MCP_APPS`.
  - VS Code names an MCP tool `mcp_<server>_<tool>`; the runner now reads it
    as server and tool, as on other clients, so a pin such as
    `{server: Playwright, tool: browser_navigate}` binds there.

- **The person confirms changes in their own client.**
  - VS Code: the runner's question is shown by VS Code, in the chat or as a
    notification with a form. A write the person allowed was made and one
    they declined was refused.
  - Copilot CLI: the question appears as a form in the terminal (enter
    accepts, ctrl+d declines), with the same results.
  - Gemini CLI: `tap install --client gemini` adds a `BeforeTool` hook that
    makes Gemini ask, in its own confirmation, before `tap_save` and before
    the first run of a primitive that is not yet trusted, showing what it
    declares, even outside yolo mode. Under `gemini -p` nothing can be asked
    and the hook asks nothing. TAP's tools accept the `wait_for_previous`
    argument Gemini CLI 0.63 adds to every call; before, `tap_save` and
    `tap_run` refused those calls.
  - Goose: `goose session` shows TAP's questions; the first-run question now
    names what it agrees to. Time the person spends answering no longer counts
    toward the 20-second handoff (except in Codex, whose tool call stops at
    about 30 seconds); before, Goose's model received a still-running handle
    instead of the result and ended its turn.

- **Authors draft in their workspace.** Agents draft primitives in
  `.tap/drafts/` inside the workspace instead of `~/tap-drafts`. `tap
  discover brief` writes there by default, and `brief`, `validate`, `save`
  and `tap_save` keep everything under `.tap/drafts/` out of git. Agents that
  may only touch their workspace (Kilo, Crush and OpenCode run modes) refused
  the old folder, so their saves never happened. Saving refuses a package
  that holds an authoring brief, which quotes the person's session history,
  whether named `BRIEF.md`, `brief.json` or renamed. The asked-before note in
  `tap_search` points at the new folder.

- **The authoring guidance covers any automatable work.** The tap-author
  skill, the guide and the `tap discover brief` instructions now cover the
  browser, desktop apps, shell and files, MCP connectors, HTTP APIs and steps
  that need a person, with these rules: design what the program does, what it
  hands back and the evidence that completes each step; write it for any
  client, with each client's tool optional and `blocked` when none is bound,
  never scripting one client's own objects; take inputs in the words a person
  uses (a name, a ticket key) and find the rest in the program; declare the
  operation (`gitlab.list_projects`), not a gateway's dispatcher; test normal,
  empty, changed-count, missing-prerequisite, ambiguous, slow-loading and
  no-backend cases through the real runner before saving; make it fast (count
  round trips, index once per run, batch, loop inside the backend, no fixed
  waits); and publish improved versions from real runs without reporting
  anything unresolved as found. The guide also explains browser preflight,
  identity and pagination evidence, bounded retries, virtualized lists,
  reversed scrollers and background tabs, and records what each client lends.

- **`tap_run` reports what a run cost.** Each result ends with a line of tool
  calls, failed calls, refusals, input items and duration. When a run made at
  least 50 tool calls and more than 20 per input item, one more line says the
  author can publish a faster version and names the run to read with
  `tap_evidence`.

- **The docs state only what was run.** The README's agent table and the
  client notes in the install guide, web guide, bridge research and threat
  model now give results on the current source, per agent, or say what was
  not run. The install guide lists all seven MCP tools and no longer says
  Claude Code binds tools by schema; Claude Code gives the runner names only.

- Saving an agent-authored primitive uses the host's standard confirmation
  policy, without an extra required checkbox. A canceled or failed prompt is
  reported as a client failure instead of saying the person refused. Codex
  bridge calls retain per-run caller metadata so native browser tools can
  reach the caller's live UI; CLI runs resolve the host's actual turn context.

- Repeated tasks in distinct sessions only seconds apart are recognized. Search
  reads fresh incremental history and excludes the session with the pending TAP
  search, rather than discarding every session in a 30-second startup window.
  Concurrent searches that cannot be distinguished do not suggest a save.

- When one shared runner answers several sessions, a new session no longer
  reuses a read of the agent's history taken before an earlier ask was
  written. Before, a task asked again within about a minute could be
  reported as new, and each session now judges what is earlier from its own
  start.

- Aider history discovery refreshes directory listings captured during recent
  writes, so timestamp aliases cannot keep deleted paths or hide new history.
  Stable directories retain their cached listings across searches and restarts.

- In an agent that cannot show TAP's prompts, `tap_save` now saves when the
  agent's own settings let it write files without asking. Before, it always
  refused and pointed to `tap discover save`, which Gemini CLI's own policy
  then blocked, so nothing could be saved there.

- Security hardening keeps existing execution and cache behavior: authoring
  briefs remove complete private-key blocks and named credentials in structured
  arguments and environment values; HTTP failure diagnostics omit URL secrets;
  command classification keeps both global flags and launcher subcommands;
  host programs follow run cancellation, and native guest diagnostics are
  bounded. Builds use patched Go and gRPC dependencies, with vulnerability
  checks in CI. Private run records retain their existing evidence semantics.

- Reading agent history no longer holds it whole. `tap discover` and the
  asked-before note of `tap_search` read every session they read before;
  results from SQLite stores are decoded a row at a time, stores are read in
  batches, sessions are passed on as they are read, and the history cache
  keeps one file per session or conversation instead of one file per agent.
  On one machine a full read of a 15.8 GB Cursor store fell from 1.97 GB to
  127 MB of memory, and a cached Codex read from 807 MB to 108 MB. A first,
  uncached read of a large file-based history now takes longer (Codex: 19 s
  to about 30 s) in exchange for about a third of the memory. The cache is
  rebuilt once in the new layout; the old per-agent files are removed.
- `tap serve` now uses the history cache `tap discover` keeps, so the
  asked-before note reads only what changed since the last read.

- The example gallery index now lists all six runnable language examples,
  including Go and C++ through the compiled WASI Preview 1 path available
  since 0.2.10, and distinguishes author builds from execution.

- In an agent that cannot show TAP's prompts (OpenCode, Kilo, Crush, Gemini
  CLI, `goose run`), a primitive that only reads now runs when the agent's own
  settings let its model do the same without asking. That covers web reads,
  as before, and now read-only programs such as `git log` where the agent
  runs shell commands unasked. Gemini CLI counts in yolo mode. When it does
  not run, the refusal names the agent setting that asked.

- Failed MCP calls now preserve the tool's original error through Claude and
  Codex. An upstream permission error is no longer reported as a JSON result
  contract violation, and failed writes are never automatically retried.

- TAP now tells agents to call `tap_search` at the start of every request,
  even one that names a specific commit, ticket or file. Asked why they had
  skipped it, agents quoted the old wording ("Not for one-off requests", "a
  task that takes several tool calls"). When a task was asked before, the
  search result now gives the exact line to end the answer with, offering to
  save it.

- Every agent session on a machine is now answered by one shared runner.
  An agent starts `tap serve` once per session, and some start dozens at a
  time; each used to be a full runner that read the agent's history on its
  own, and a burst of forty held tens of gigabytes. `tap serve` is now a
  relay of about 15 MB that passes the session to the shared runner,
  with the folder and environment the agent started it in, so each run still
  works in its session's own project and asks its approvals of that session
  only. The relay starts the runner when none is running; the runner stops
  after ten minutes with no sessions, and a relay whose runner stops starts
  another and goes on. Nothing changes in an agent's configuration.
  `--config-dir`, `--http-listen` and the new `--own-process` keep a session
  in a process of its own, as before.
- Two `tap serve` processes no longer read an agent's history at the same
  time: the second waits and reads only what changed since the first.
- On macOS and Linux, a release's `tap serve` now replaces itself with a
  relay of under 4 MB built into the runner, instead of relaying as the
  whole runner (about 15 MB). Forty open sessions hold about 150 MB of
  relays instead of 600 MB. Windows, and a runner built from source, relay
  as before.
- A process reads one agent's history at a time, and gives the memory a
  history read or a burst of runs used back to the system when it ends.
  In a burst of forty sessions from every supported agent, the shared
  runner's peak fell from 2.5 GB to 1 GB, and its memory after a burst
  of runs from 600 MB to under 100 MB.
- `tap serve` now keeps compiled interpreters in the user cache directory,
  as `tap run` does, and a process compiles each interpreter once. Before,
  every `tap_run` compiled its interpreter again, which took seconds of CPU
  per run and, with many sessions at once, much longer.

- A repeat `tap discover` run now reads only what changed for every agent,
  not only for agents that keep one file per session. Cursor finds the
  conversations written since the last run from its key index and reads only
  those. OpenCode, Kilo, Goose, Crush and Zed read only the sessions or
  threads whose update time, row count or row IDs changed. Cursor CLI,
  Antigravity, Copilot CLI, Windsurf, Continue and Cline reuse a session file
  while its size, modification time and the settings it was read with are
  unchanged. Aider's search of the home folder reads only folders that
  changed. Each run reads a few cached sessions again and compares them; if
  they differ, discover reads that agent in full and says so.

- On an agent that cannot lend its connections (Windsurf, Zed), `tap_save`
  refuses a primitive that requires a connection, and a run of one says how to
  fix it: declare a program such as `git` under `commands:` and a web read
  under `fetch:`. Before, such a primitive saved and was then refused on every
  run.

- A search for a task the person asked before now finds it when the agent
  words it differently: "readiness" and "ready" count as the same word.
  Before, a Codex search for "release readiness" was told the task was new.

- `tap setup` now installs the tap-author skill into OpenCode and Crush too.
  Before, it was installed only where TAP can also use the agent's own
  connections, so OpenCode and Crush never searched TAP before answering and
  never offered to save a task asked for again.

- `tap discover` reads every agent's history at once and parses session
  files in parallel, leaving one CPU free. Each file is read once. Parsed
  sessions are cached in `~/.tap/discover/cache` (private to the user) and
  reused while a file's size and modification time are unchanged, so a
  repeat run parses only new or changed sessions; a different `tap` build
  starts a fresh cache. An agent whose history cannot be read is skipped
  with its reason instead of ending the run. SQLite read deadlines grow with
  the store's size, and Cursor's queries read only the records they need, so
  a large Cursor store is read instead of timing out.

- MCP inspection tools (`tap_search`, `tap_load`, `tap_status`, `tap_evidence`)
  return compact readable text by default. `detail: true` returns the complete
  JSON contract; existing JSON parsers must opt in. Load retains exact package
  identity, all input constraints, permissions and connection warnings, with
  full output schemas and capability definitions available on request. TAP
  sends one representation per response. Execution results, approvals and
  journal semantics are unchanged.

## 0.2.10

- Discover and agent-authored saves require a versioned changelog, reject
  changed bytes under an existing version and version regressions, and retain
  prior versions for explicit reuse. `tap_save` checks a frozen package before
  asking for consent. Initial generated packages include a changelog.
- Compiled WASI Preview 1 `.wasm` packages use the existing permission broker,
  approvals, contracts and journal. Authors explicitly build before saving;
  `tap discover build --approve-build` verifies repeatable output and records
  freshness against source, manifest and changelog. Save and run never compile.
- The release publisher extracts its clean source archive from a temporary file,
  avoiding a pipe teardown hang during signed release preparation.

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
