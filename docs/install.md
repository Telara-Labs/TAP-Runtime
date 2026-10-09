# Installing the runner

## What the machine needs

- **An agent that can lend its connections**, for a primitive that calls
  tools: Claude Code, Codex, VS Code (with the extension below), Gemini CLI
  (experimental), Goose or the Kilo CLI (both experimental, see below). `tap install` also connects the TAP MCP server to Cursor,
  Windsurf and Copilot CLI. Registration alone does not prove execution;
  their native connection handoff remains limited (docs/bridge-research.md).
  The source HTTP frontend can instead use an explicitly configured generic
  MCP backend, as described below. On v0.1.8, one two-read workflow passed
  once each from Claude Code, Codex, Cursor CLI, Copilot CLI, VS Code, Goose
  and Windsurf through that frontend. It has not been repeated on a later
  release, and no write was tested. The runner also runs from a
  terminal with no agent, for primitives that use no tools.
- **The connections a primitive uses**, already connected in that client (for
  example Gmail), and any host program it declares (`git`, `kubectl`).
- **Network on first use**, to download interpreters. `tap fetch`
  downloads them all ahead of time.

No Telara account, registry or gateway is needed.

## TAP Local collection

The MCP server is named `tap`. Its seven fixed tools are `tap_search`,
`tap_load`, `tap_run`, `tap_status`, `tap_evidence`, `tap_save` and
`tap_result`. Search finds installed v3
primitives in the user's TAP collection and saved primitive folders for
Claude Code and Codex. Ordinary `SKILL.md` folders are not treated as TAP
primitives. Load returns the manifest's declared inputs and effects. Run uses
the exact `publisher/name@version` and digest returned by search; if the
package changes, search again. Status and evidence read the local run record.

For an additional collection root, start the server with
`tap serve --catalog-root DIR`. This is an explicit local directory, not a
remote registry. The runner does not publish packages through MCP; `tap_save` only saves
a package into the local collection, after the person agrees in a prompt.

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
`tap remove` deletes only the one it wrote.

When `tap_search` finds no saved primitive, it says whether the person asked
for that kind of task in an earlier session. When the agent connects, `tap
serve` reads that agent's own history from the last 30 days on this machine,
in the background, and compares the search words with the request that opened
each earlier session; values such as commits and versions are ignored, and two
shared words must be uncommon in that history. Nothing is sent anywhere. To
avoid re-reading a long history on every start, the words of each opening
request and a 120-character excerpt are kept in a private file
(`tap-runtime/requests-<agent>.json` in the user cache directory, mode 0600);
later starts read only the session files changed since. Only a task asked for
before is offered for saving. Run it again after adding a client. It preserves
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
`https://github.com/Telara-Labs/TAP-Runtime/releases/download/vX.Y.Z`.
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
The runner's own gate still applies. Goose's tool list carries no
annotations, so each tool is treated as a write and needs the runner's
approval, which `tap serve` asks for through the agent.

`goose session` shows the runner's questions (MCP elicitation) in the
terminal as Yes/No prompts, with a number field where a change can be
allowed more than once: saving a primitive, its first run, and each change.
On Goose 1.53.0 these were shown and answered, and a file write, a Jira read
through a remote MCP server and a click through Playwright MCP ran only
after the person agreed. Time spent answering does not count toward the
runner's handoff, so the result still reaches the model.

`goose run` cannot ask anything. Goose itself confirms nothing without a
terminal: in `approve` and `smart_approve` modes `goose run` stops with an
error, and in `auto` mode it allows every tool (Goose's
`crates/goose-cli/src/session/mod.rs`). So under `goose run` the runner does
only what `auto` mode already lets the agent do unasked: saving, and a
primitive that only reads. Every other change is refused. A tool set to
`never_allow` in Goose is refused.

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
extension cannot lend its connections. The bridge was tested live with Kilo
CLI 7.8.3.

**Kilo CLI, Crush, OpenCode: drafts stay in the workspace.** These agents
may refuse, or ask before, touching a folder outside the one they work in;
`kilo run` refuses with nobody to ask. The tap-author skill therefore drafts
a primitive, its brief, cases and receipts in the workspace's `.tap/drafts/`,
which `tap discover brief`, `validate` and `save` keep out of git with a
`.gitignore`. Saving copies the package into the TAP collection, written by
the runner, so the agent never writes outside its workspace.

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

**Experimental.** Run live on Gemini CLI 0.63.0 with a Gemini API key, in
its interactive terminal in the default approval mode and headless (`-p`) in
yolo mode. Tool filtering (`includeTools`, `excludeTools`) has not been run.

```
tap install --client gemini
```

Gemini CLI has no way for a program to call its tools, but its hooks can ask
it to make a call. So the runner registers three things in
`~/.gemini/settings.json`: itself as the MCP server `tap`, an `AfterTool`
hook and a `BeforeTool` hook. When the model calls `tap_run`, the `AfterTool`
hook has Gemini make each tool call the primitive asks for, with Gemini's own
connection, carries each result to the runner, and ends with `tap_result`.
The model sees only the primitive's output. A copy of the settings file as it
was is kept beside it as `settings.json.tap-backup`.

**Approval is Gemini's own.** Gemini shows no other program's prompts (it
does not support MCP elicitation), so the runner uses Gemini's confirmation:

- Each tool call a primitive makes is a call Gemini makes, confirmed the way
  Gemini confirms a call from its model. In the default approval mode it asks
  before each one; "No" stops the call.
- Before `tap_save`, and before the first run of a primitive that is not yet
  trusted, the `BeforeTool` hook makes Gemini ask, even in yolo mode and
  after "allow for this session", with the runner's question in the dialog:
  what the primitive declares. Allowing a first run trusts that version and
  allows the `GET` and `HEAD` requests it declares.
- A change Gemini never sees (a file write, a program, a web write) cannot be
  confirmed there and is refused.
- Headless (`gemini -p`) nobody can answer, so the hook asks nothing. There a
  save works only in yolo mode, a primitive that only reads runs where
  Gemini's settings let it read unasked, and anything else needs `tap trust`.

In the default mode Gemini also asks before each of TAP's own tools, searches
included. Choosing "Allow all server tools for this session" for `tap` keeps
the save and first-run questions.

Gemini does not tell other programs which tools it has, so on Gemini a
primitive must pin each tool it uses: `pin: {server: <server>, tool: <tool>}`,
with the server's name as it appears in Gemini's settings.

## VS Code (GitHub Copilot)

**Preview.** Extension source is in [`vscode/`](../vscode/README.md).
Releases include `tap-vscode-<version>.vsix` (0.2.18 does). Install it from
VS Code's Extensions view, under "Install from VSIX...".

It registers the runner with VS Code as the MCP server "TAP Runtime", so there is
nothing to add to `mcp.json`, and it lets the runner call the tools VS Code
exposes through its public tool API. In VS Code 1.140, the Local chat harness
exposed the tested real MCP tools, while the newer Copilot SDK harness omitted
MCP tools from `lm.tools`. Connecting a server alone does not establish its
visibility through this extension. Calls use VS Code's connections. The runner
names an MCP tool by its server and the server's own tool name, as on other
clients (VS Code lists it as `mcp_<server>_<tool>`), so pins match it.
VS Code gives tool input schemas, so tools bind by name and schema, as on
Codex. Claude Code gives no schemas, so there a tool binds by name only (see
T18 in the [threat model](threat-model.md)).

**Who confirms a change.** VS Code itself, in its own UI. When Copilot agent
mode calls `tap_run`, VS Code asks the person first (`tap_run` is not
read-only), unless they have set it to be approved automatically. Each change
the primitive then makes (a file write, a command, a request, a tool that is
not known to be read-only) is asked by the runner as an MCP elicitation,
which VS Code's MCP client shows: in the chat that called `tap_run`, or,
with no chat, as a notification whose Respond button opens the form. Only an
explicit yes lets the change happen. VS Code's own auto-approval settings do
not answer these forms. A call the extension makes for a primitive carries
no chat invocation token, because an extension only gets one from a chat
request it handles itself; for such a call VS Code shows a modal dialog
before a tool that is not read-only, after the runner's form (read in VS
Code's source; the test profile approves tool calls automatically, so the
run did not show that dialog). Measured on
current source with VS Code 1.141 and a profile of the test's own
(`go test ./host -run LiveVSCodeConfirm -live-vscode-confirm`): a file write
approved in VS Code's form was written and a declined one was refused before
anything was written; a Playwright browser primitive approved in the form
passed, and declined, made no browser call. VS Code gives no tool
annotations to extensions, so a gateway such as Telara, whose catalog tool
must be known to be read-only before the runner trusts it, is not bound
through VS Code.

The extension finds `tap` in `~/.local/bin`, on PATH, or at the
`tapRuntime.path` setting. macOS and Linux only for now.

## Copilot CLI

`tap install --client copilot-cli` runs `copilot mcp add tap -- <path>/tap serve`.
Copilot CLI asks the person itself before it calls a tool that is not
read-only ("Do you want to use this tool?"), so `tap_save` and `tap_run` are
asked there first. It also shows the runner's own questions: Copilot CLI
advertises MCP elicitation, and in its interactive terminal UI each one is a
form ("tap needs information"; enter accepts, ctrl+d declines). In `copilot
-p` it declines every form, so a save or a change is refused there. Measured
on current source with Copilot CLI 1.0.94, a terminal and a runner home of
the test's own, the forms answered at the keyboard by the test
(`go test ./host -run LiveCopilotCLI -live-copilot-cli`): a primitive was
saved with `tap_save`, found again with `tap_search` in a new session, and
run; its file write happened after a yes and was refused after a no. With
`--mcp-url` (Copilot CLI does not lend its own connections to a server it
starts), a Jira comment through the Telara gateway was posted after a yes and
refused after a no, and a Playwright browser primitive passed after a yes.

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
