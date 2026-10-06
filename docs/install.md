# Installing the runner

## What the machine needs

- **An agent that can lend its connections**, for a primitive that calls
  tools: Claude Code, Codex, VS Code (with the extension below), Gemini CLI
  (experimental), Goose or the Kilo CLI (both experimental, see below). `tap install` also connects the TAP MCP server to Cursor,
  Windsurf and Copilot CLI. Registration alone does not prove execution;
  their native connection handoff remains limited (docs/bridge-research.md).
  The source HTTP frontend can instead use an explicitly configured generic
  MCP backend, as described below. Claude Code, Codex and Cursor have passed
  the same real read workflow through that frontend; controlled writes and
  the remaining clients are still pending. The runner also runs from a
  terminal with no agent, for primitives that use no tools.
- **The connections a primitive uses**, already connected in that client (for
  example Gmail), and any host program it declares (`git`, `kubectl`).
- **Network on first use**, to download interpreters. `tap fetch`
  downloads them all ahead of time.

No Telara account, registry or gateway is needed.

## TAP Local collection

The MCP server is named `tap`. Its fixed tools are `tap_search`, `tap_load`,
`tap_run`, `tap_status`, and `tap_evidence`. Search finds installed v3
primitives in the user's TAP collection and saved primitive folders for
Claude Code and Codex. Ordinary `SKILL.md` folders are not treated as TAP
primitives. Load returns the manifest's declared inputs and effects. Run uses
the exact `publisher/name@version` and digest returned by search; if the
package changes, search again. Status and evidence read the local run record.

For an additional collection root, start the server with
`tap serve --catalog-root DIR`. This is an explicit local directory, not a
remote registry. The runner does not publish packages through MCP.

## Ordinary requests and saved skills

Save a primitive with `tap discover save <package-dir>`. Saving installs one
package in the shared TAP collection and creates discoverable skill pointers
for connected supported clients. A package merely sitting in a project folder
is not installed globally; setup does not scan arbitrary project directories.

`tap setup` connects detected clients and reconciles pointers for primitives
already in the collection. It also writes a `tap-author` skill into each
connected client's skills folder; it tells the agent to offer a primitive only
for a recurring multi-step procedure, to ask before saving, and how to write,
check and save one. A `tap-author` folder TAP did not write is kept, and
`tap remove` deletes only the one it wrote. Run it again after adding a client. It preserves
foreign skill folders, reports collisions, and refreshes TAP-owned pointers
without duplicating executable packages. `tap install --client <agent>` does
the same reconciliation for that client; `--print` changes nothing and
`--remove` does not create or refresh pointers.

With the saved skill available, ask the normal task question without naming
TAP. Skill selection and `tap_search` let the agent find an applicable
primitive, inspect it and execute its exact ref/digest. The client's model
still chooses tools; verify actual `tap_run` receipts before claiming it used
one. Clients without a supported tool-connection bridge are not made runnable
by registration alone. Start a fresh client session after setup.

## Standard MCP execution with a configured backend

Clients that cannot lend their own tool connections can use an explicitly
configured downstream MCP server. Its inventory and calls go through the
existing generic bridge; the primitive and its digest stay the same:

```
tap serve --mcp-url https://your-backend.example/mcp --mcp-header-file /private/backend-headers
```

The header file holds lines such as `Authorization: Bearer ...`. Keep it
outside the project and accessible only to its owner. This does not inherit
connections authenticated only inside another client.

If a package pins the connection name configured in its original client,
pass that same logical name with `--mcp-server-name` (for example `telara`).
Without it, the generic bridge uses the backend's advertised `serverInfo.name`.
This labels the configured connection; it does not rename tools or choose
another backend.

For clients using Streamable HTTP, the released runner also supports:

```
tap serve --http-listen 127.0.0.1:8765 \
  --http-token-file /private/tap-token \
  --mcp-url https://your-backend.example/mcp \
  --mcp-header-file /private/backend-headers
```

Connect to `/mcp` with `Authorization: Bearer <token from tap-token>`. The
incoming token must contain at least 32 characters; its file must be private.
Incoming and backend credentials are separate. Requests with an Origin header
are refused unless that exact origin is supplied with `--http-origin`.
Each client gets a separate session and approval channel. Cancellation uses
`notifications/cancelled`; a disconnected response does not retry the workflow.

Web clients additionally need a reachable HTTPS endpoint or their supported
secure tunnel. This transport currently uses fixed bearer authentication;
it does not provide an OAuth authorization server. A client that cannot
send that credential or display MCP elicitation has not passed write
acceptance. Do not expose an unauthenticated endpoint to work around it.

The client acceptance gate is discovery/load, one `tap_run`, actual read and
controlled write, declared approvals/limits/errors, and independent destination
verification in every claimed client. A result containing refused calls or unknown
outcomes reports an MCP error and retains those outcomes in its run record. Protocol tests and registration do not
satisfy it. The opt-in native-client read harness is:

```
GOWORK=off go test ./host -run '^TestLiveClientAcceptanceClaudeCodeAndCodex$' -count=1 -v -live-client-acceptance
```

It uses the installed Jira primitive, disables its write follow-up, and retains
MCP transcripts and journals in the printed temporary evidence directory.

## One instruction

### npm

```
npm install -g @telaralabs/tap
```

The global package supplies the `tap` command, including `tap discover`,
`tap serve`, and the other CLI commands. When npm permits install scripts,
its install step runs `tap install --client detected`, which connects the MCP
server to every agent installed here.
If scripts are disabled, run `tap setup` after installation; recent npm
versions can use `npm install -g --allow-scripts=@telaralabs/tap @telaralabs/tap` for
one-command setup. An ordinary project dependency install keeps registration
explicit: run `npm exec -- tap setup` in that project. VS Code uses the
separate extension below. Browser-only chats do not inherit a local MCP
registration.

The package is published from a signed five-platform release. Find assets
for the same version as your package on the
[GitHub releases page](https://github.com/Telara-Labs/TAP-Runtime/releases).

### Release installer

macOS and Linux:

```
curl -fsSL <release>/install.sh | sh
```

Windows (PowerShell):

```
irm <release>/install.ps1 | iex
```

`<release>` is the address of a release's files, such as
`https://github.com/Telara-Labs/TAP-Runtime/releases/download/v0.1.15`.
Choose the version you intend to install.

The script downloads the runner for this machine and checks it against a
sha256 written into the script when the release was built. If it doesn't
match, the download is deleted and nothing is installed. It then runs
`tap install --client detected`, which connects the runner as an MCP server
named `tap` to every agent installed here:

| Agent | How |
|---|---|
| Claude Code | `claude mcp add --scope user tap -- <path>/tap serve` |
| Codex | `codex mcp add tap -- <path>/tap serve` |
| Copilot CLI | `copilot mcp add tap -- <path>/tap serve` |
| Cursor (app and CLI) | one entry merged into `~/.cursor/mcp.json` |
| Windsurf | one entry merged into `~/.codeium/windsurf/mcp_config.json` |
| Gemini CLI | `~/.gemini/settings.json`, with the hook its bridge needs |
| Goose | one extension merged into `~/.config/goose/config.yaml` |
| Kilo CLI, OpenCode | one `mcp` entry merged into `~/.config/kilo/kilo.json` or `~/.config/opencode/opencode.json` |
| VS Code | the TAP extension below |

A JSON or YAML file is changed only by adding (or, with `--remove`,
removing) the `tap` entry: other servers and settings keep their values and
order (and, in YAML, their comments), the original is kept once as
`<file>.tap-backup`, a file that does not parse is left alone, and running
it again changes nothing.

**Goose: the runner is the only approval.** The runner reaches Goose's
connections through Goose's ACP request `_goose/unstable/tools/call`, which
Goose marks unstable and runs only in auto mode. A primitive's tool calls
therefore show no Goose approval prompt, whatever mode Goose is set to.
The runner's own gate still applies. Goose annotates no tool, so each one is
treated as a write and needs the runner's approval, which `tap serve` asks
for through the agent. A tool set to `never_allow` in Goose is refused.
Tested with Goose 1.53.0.

**Kilo CLI: the runner is the only approval, and tools are pinned.** For a
primitive's tool calls the runner starts its own `kilo serve` (with Kilo's
experimental flag and a one-time password, on that server only) and calls
`POST /experimental/mcp/call-tool`, which runs the tool through Kilo's own
MCP connection with no model turn and no Kilo approval prompt. Kilo
annotates nothing, so each call needs the runner's approval. Kilo lists no
MCP server's tools, so a primitive run through Kilo must pin each tool it
uses (`pin: {server, tool}`), as for Gemini CLI. A tool switched off
(`tools: {"server_tool": false}`) or denied (`permission: {"server_tool":
"deny"}`) in Kilo's configuration is refused. The Kilo Code VS Code
extension cannot lend its connections. Tested with Kilo CLI 7.8.3.

Options: `--client AGENTS|none` and `--dir DIR` (default `~/.local/bin`).
Windows takes `-Client` and `-Dir`, and installs to
`%LOCALAPPDATA%\Programs\tap`.

To connect an already-downloaded runner by hand:

```
tap install --client detected            # every agent installed here
tap install --client claude-code         # --scope local|user|project
tap install --client cursor,windsurf
tap install --client all --print         # show what would change, change nothing
tap install --client cursor --remove
tap remove                               # disconnect from every agent
```

## When two servers offer the same tool

If two connected servers offer a tool that fits what a primitive needs equally
well, the runner does not choose between them: a server you do not trust could
offer a tool named like the one you mean. Through `tap_run`, in a client that
can ask, you are asked which server to use. Otherwise the run is refused and
the message gives the command:

```
tap bind --client claude-code gmail.threads.search "claude.ai Gmail"
tap bind --list
tap bind --client claude-code --forget gmail.threads.search
```

The choice is kept on this machine, per client, in `tap/bindings.json` in your
user config directory, outside the primitive's manifest. A package can still
explicitly pin a server with `pin.server`, which narrows its portability.
Without that pin, the same capability can bind to different compatible
connections in different clients.

## Gemini CLI

**Experimental.** Hook execution was tested against a stand-in, not live Gemini.
The existing `oauth-personal` profile in Gemini CLI 0.62.0 returned
`IneligibleTierError`, so native hook approval and tool filtering could not be
verified with that account.

```
tap install --client gemini
```

Gemini CLI has no way for a program to call its tools, but its hooks can ask
it to make a call. So the runner registers two things in
`~/.gemini/settings.json`: itself as the MCP server `tap`, and an `AfterTool`
hook. When the model calls `tap_run`, the hook has Gemini make each tool call
the primitive asks for, with Gemini's own connection and its own approval,
carries each result to the runner, and ends with `tap_result`. The model sees
only the primitive's output. A copy of the settings file as it was is kept
beside it as `settings.json.tap-backup`.

Gemini does not tell other programs which tools it has, so on Gemini a
primitive must pin each tool it uses: `pin: {server: <server>, tool: <tool>}`,
with the server's name as it appears in Gemini's settings.

## VS Code (GitHub Copilot)

**Preview.** Extension source is in [`vscode/`](../vscode/README.md).
The published v0.1.15 release has no VSIX asset. A locally packaged VSIX can
be installed from VS Code's Extensions view, under "Install from VSIX...".

It registers the runner with VS Code as the MCP server "TAP Runtime", so there is
nothing to add to `mcp.json`, and it lets the runner call the tools VS Code
exposes through its public tool API. In VS Code 1.140, the Local chat harness
exposed the tested real MCP tools, while the newer Copilot SDK harness omitted
MCP tools from `lm.tools`. Connecting a server alone does not establish its
visibility through this extension. Calls use VS Code's connections. The runner obtains required approval
itself; extension-dispatched calls cannot rely on VS Code's native confirmation.
Tools bind by name and schema, as on Claude Code and Codex. Native Copilot on
v0.1.15 rendered separate package-trust and fetch-origin forms; an approved
public GET passed and a later declined fetch was refused before execution.

The extension finds `tap` in `~/.local/bin`, on PATH, or at the
`tapRuntime.path` setting. macOS and Linux only for now.

## claude.ai, in the browser

**Preview.** See [web.md](web.md).
The local stdio MCP is available to a web conversation only when that client
has a supported desktop companion that starts and connects the local server.
The Artifact preview described in web.md is a separate path.

## Checking a release yourself

Each release holds `SHA256SUMS`, its signature `SHA256SUMS.sig`, and the
public key that made it, `SHA256SUMS.pub`. The key in the release shows which
key signed it. It proves nothing on its own: check against a copy of the key
from somewhere you already trust.

```
go run ./release verify --dir <downloaded release> --pub <trusted key file>
```

## Codex

- In `codex exec`, Codex cancels an MCP tool call that needs approval. It
  reports `user cancelled MCP tool call`. Permit the outer `tap_run` tool for
  that exec invocation:

  ```
  codex exec -c 'mcp_servers.tap.tools.tap_run.approval_mode="approve"' "..."
  ```

  This configures Codex's outer tool gate. Package trust, fetch-origin approval
  and an ambiguous tool binding remain separate runner gates; see
  [TAP Local](../README.md#tap-local-your-primitives-as-an-mcp-server).
  An interactive Codex session asks instead.
- In the tested Codex `read-only` sandbox, an MCP-started runner could download
  interpreters while a shell-started runner could not. Network access depends
  on the session's sandbox configuration. If downloading is unavailable, run
  `tap fetch` once in your own terminal first.

## Building from source

```
export GOWORK=off
go build -o tap ./host
GOOS=wasip1 GOARCH=wasm go build -o sh.wasm ./guest-sh
```

A runner built from source doesn't know where a release's bash interpreter
is, so it can't download one. Put `sh.wasm` in its interpreter store
(`tap fetch` prints where that is), or pass `--interpreters DIR`.

The module is `github.com/Telara-Labs/TAP-Runtime`, fetched through the
public Go module proxy; no credentials are needed:

    go install github.com/Telara-Labs/TAP-Runtime/host@latest

The installed program is named `host`; rename it to `tap`.

## Tested on

| Platform | State |
|---|---|
| macOS arm64 | full source suite and native Claude/Codex reads; full v0.1.14 launch package passed locally with isolated HOME/PATH and existing Go caches |
| Linux amd64, arm64 | source suite in CI; clean native v0.1.14 launch package passed, including public npm, installer and BusyBox/truncation checks |
| Windows amd64 | portable source suites in native CI; clean v0.1.14 launch package passed, including actual PowerShell installation and npm/README flow |
| macOS amd64 (Intel) | clean native v0.1.14 launch package passed, including public installer and npm/README flow |

The four clean hosted v0.1.14 results are in
[this workflow](https://github.com/Telara-Labs/TAP-Runtime/actions/runs/37373338835).
The Apple Silicon hosted job was cancelled before any steps; its local
functional pass is a separate receipt. These are version-specific launch
checks, not the complete host suite or a five-platform v0.1.15 acceptance run.
Windows ACL behavior remains unverified.
