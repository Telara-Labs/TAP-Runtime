# Installing the runner

## What the machine needs

- **An agent that can lend its connections**, for a primitive that calls
  tools: Claude Code, Codex, VS Code (with the extension below), Gemini CLI
  (experimental) or Goose (experimental, see below). `tap install` also connects the TAP MCP server to Cursor,
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

For clients using Streamable HTTP, the source build also supports:

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

The package is published from a signed five-platform release. The matching
release assets are at
`https://github.com/Telara-Labs/TAP-Runtime/releases/tag/v0.1.2`.

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
`https://github.com/OWNER/REPO/releases/download/v0.1.0`.

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
user config directory. The primitive's manifest never names a server, so the
same primitive runs where Gmail is a plugin on one client and an MCP server on
another.

## Gemini CLI

**Experimental.** Built and tested against a stand-in for Gemini CLI, never
against Gemini CLI itself: Google no longer admits individual accounts to it
("please migrate to the Antigravity suite"), so the test could not run.

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

**Preview.** The extension ships with each release as
`tap-vscode-<version>.vsix`. Install it from VS Code's Extensions view, under
"Install from VSIX...".

It registers the runner with VS Code as the MCP server "TAP Runtime", so there is
nothing to add to `mcp.json`, and it lets the runner call the tools VS Code
already has: every MCP server connected for Copilot, and the editor's own
tools. VS Code makes each call with its own connection and shows its own
confirmation where a tool asks for one. Tools bind by name and schema, as on
Claude Code and Codex.

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
  reports `user cancelled MCP tool call`. Allow the tool once:

  ```
  codex exec -c 'mcp_servers.tap.tools.tap_run.approval_mode="approve"' "..."
  ```

  An interactive Codex session asks instead.
- A runner that Codex starts as an MCP server can download interpreters,
  measured in Codex's `read-only` sandbox. A runner started from Codex's
  **shell** has no network, and so can't download one. Run primitives
  through the `tap_run` tool, or run `tap fetch` once in your own
  terminal first.

## Building from source

```
export GOWORK=off
go build -o tap ./host
GOOS=wasip1 GOARCH=wasm go build -o sh.wasm ./guest-sh
```

A runner built from source doesn't know where a release's bash interpreter
is, so it can't download one. Put `sh.wasm` in its interpreter store
(`tap fetch` prints where that is), or pass `--interpreters DIR`.

`go install <module>/host@<version>` works only while the module is served
from the path it declares, `gitlab.com/telara-labs/tap-runtime`. Served from
any other address, such as a GitHub repository, Go refuses it: the module
declares one path and was required as another. The installed program is
named `host`.

## Tested on

| Platform | State |
|---|---|
| macOS arm64 | the full suite, the install script, and primitives through Claude Code and Codex |
| Linux amd64, arm64 | the full suite, in CI |
| Windows amd64 | a subset, and one primitive, under Wine in CI. Never on Windows itself. `install.ps1` has never been run |
| macOS amd64 (Intel) | built. Never run |
