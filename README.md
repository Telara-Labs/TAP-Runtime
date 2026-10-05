# TAP Runtime

Run your agent's repeated work as a small program instead of a conversation.

A **primitive** is a folder with a `primitive.yaml` and one entrypoint
(`main.sh`, `main.py`, `main.js` or `main.ts`). The runner executes it in a
sandbox, borrows the tools your coding agent is already connected to (Jira,
GitLab, Gmail, ...), and asks before anything changes. `tap discover` finds
candidates for primitives in your own agent history, on your machine.

```
npm install -g @telaralabs/tap     # the tap CLI, and it connects to your agents
tap discover                       # what you keep asking your agents to do
```

No account, registry or server is needed. MIT licensed.

## What it does

- Runs the entrypoint inside a WebAssembly sandbox with no filesystem, no
  network and an empty environment. Everything the program does goes through
  the runner as a request.
- Executes only the host programs (`git`, `kubectl`, ...) and tools the
  manifest declares, one gated and recorded call at a time. Anything else is
  refused.
- Asks before every `write` or `destructive` effect. Reads run without asking.
- Records every request before acting on it, so a run that stops can resume
  without doing anything twice.

## Where it runs

The runner calls a primitive's tools through the agent you run it from, with
that agent's own connections. How each agent lends them, and whose approval
applies:

| Agent | State | How the runner reaches its tools | Approval |
|---|---|---|---|
| Claude Code | supported | Claude Code's control channel | Claude Code's, plus the runner's |
| Codex (CLI and app) | supported | Codex's app-server | Codex's, plus the runner's |
| Gemini CLI | experimental | a hook that has Gemini make each call | Gemini's |
| Goose | experimental | Goose's ACP tool call (marked unstable by Goose) | the runner's only |
| Kilo CLI | experimental | `kilo serve`'s MCP call route (marked experimental by Kilo); tools must be pinned | the runner's only |
| VS Code (GitHub Copilot) | preview | an extension that calls the editor's tools | VS Code's |
| claude.ai (web) | preview | a page published as an Artifact: [docs/web.md](docs/web.md) | claude.ai's |

Cursor, Windsurf, Copilot CLI, OpenCode, Qwen Code, Cline, Crush, Continue,
Zed and Aider give no way for another program to make a tool call, so
primitives that call tools cannot run inside them. `tap install` still
connects the TAP MCP server to them where they support MCP, and primitives
that use only host programs run anywhere. The research behind this table is
in [docs/bridge-research.md](docs/bridge-research.md).

## Install

```
npm install -g @telaralabs/tap
```

installs the `tap` CLI and connects it, as the MCP server `tap`, to every
supported agent installed on the machine (when npm runs install scripts;
otherwise run `tap setup`). `tap install --client <agent>`
connects one; `tap install --client all --print` shows what it would change.
Each release is signed, and the install scripts check every download against
a pinned sha256: [docs/install.md](docs/install.md).

Releases are built for macOS (arm64, amd64), Linux (amd64, arm64) and
Windows (amd64). Tests run on Linux and Windows in CI and on macOS arm64;
macOS on Intel is built but untested.

## Find primitives in your own history

```
tap discover               # read, narrow, and pick primitives to save
tap discover --client codex,claude-code --days 30
```

`tap discover` reads the session history that coding agents keep on this
machine: Claude Code, Codex, Cursor, Gemini CLI, Qwen Code, Goose, Kilo,
OpenCode, Cline, Crush, Continue, Copilot, Windsurf, Zed, Antigravity, Amp,
Aider. By default it reads every agent it detects. It finds procedures you
repeat (search, then act on what came back) and drafts each as a primitive.
It reads local files only and sends nothing anywhere. Drafts are proposals:
discover does not run them, so validate a draft before you rely on it.

## Write one

```
tap pkg/recent-mail                     # run a primitive folder
tap manifest check pkg/recent-mail      # may it run?
tap manifest complete pkg/recent-mail   # the publishable form
```

Start with [docs/writing-a-primitive.md](docs/writing-a-primitive.md) and the
folders in `examples/`.

To run, `primitive.yaml` needs a name, an entrypoint and what the primitive
uses. To publish, it must satisfy
`contract/manifest/manifest.v3.schema.json` in full. `complete` derives what
it can and marks with `TODO:` what a person must write. A field the format
does not have is an error, so a misspelt bound never reads as no bound.

## Runs are recorded, and a stopped run can continue

```
tap pkg/recent-mail                     # prints its run id
tap --resume <run id> pkg/recent-mail
```

A run that stops is continued by starting the program again and answering
what it already asked from the record, so nothing is done twice. A change
that was in progress when the run stopped is not repeated, and is reported
as unknown for a person to check. The program gets back the clock readings
and random bytes it took before. One process holds a run at a time. Records
are removed after 30 days; `--retention-days 0` keeps them.

## TAP Local: your primitives as an MCP server

`tap serve` exposes five fixed tools: `tap_search`, `tap_load`, `tap_run`,
`tap_status` and `tap_evidence`. It finds installed primitives in a local
collection and runs an exact `publisher/name@version` and package digest.
The tool list does not grow with the number of primitives. A client that
cannot show an approval prompt is never asked, and every change under it is
refused.

## Telemetry

Off unless an endpoint is set, through the standard OpenTelemetry variables
(`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, ...), which are
the only environment variables the runner reads for configuration. It sends
events: what ran, what was approved, how each ended. What a call was given
and what it touched stay on the machine unless `--otel-payloads` is passed.

## Build from source

```
go install github.com/Telara-Labs/TAP-Runtime/host@latest   # installs as host; rename to tap
```

or from a clone:

```
go build -o bin/tap ./host
go test ./... && (cd contract && go test ./...) && (cd discover && go test ./...)
```

The repository holds three Go modules: the runner (root), `contract` (the
manifest format and its validation) and `discover`. Tests that need an
installed agent, `sqlite3` or the network skip themselves when it is missing.

On first use the runner downloads the interpreter for the entrypoint's
language into the user cache directory and checks it against a pinned sha256
(`host/interpreters.go`): CPython 3.12.0 (vmware-labs
webassembly-language-runtimes), QuickJS-ng 0.17.0, and for Bash this
repository's `guest-sh`, which a source build does not know the address of:
`GOOS=wasip1 GOARCH=wasm go build -o <store>/sh.wasm ./guest-sh`. `tap fetch`
downloads them all ahead of time.

## Conformance

```
go run ./conformance/cmd/tap-conformance -- ./bin/tap serve
```

`conformance/corpus/` is data: binding, satisfaction and manifest cases with
their required outcomes, for a runner written by anybody in any language.
The kit tests a runner from outside, as an MCP server, with real packages.
`corpus/independent.json` holds 40 cases written from the specification by
an author who had not read the runner's code.

## Known limits

- The Python interpreter imports file and socket calls. It is contained
  because the runner grants it nothing, not because the calls are absent.
- From the command line `--approve` agrees to everything, and `--limit N`
  caps each kind of change at N. As an MCP server the runner asks the person
  at the client about each kind of change.
- Bash sends one request at a time. Python and JavaScript can send several
  together with `tap.call_many` / `tap.callMany`.
- A compiled `.wasm` entrypoint is not supported yet.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report a vulnerability as described
in [SECURITY.md](SECURITY.md), not in a public issue.

## Licence

MIT, in [LICENSE](LICENSE). `third_party/sh/`, a copy of `mvdan.cc/sh/v3`
3.14.1 patched to run pipelines on wasip1, keeps its BSD 3-clause licence. A
release carries both, with the licences of everything compiled in, in
`THIRD_PARTY_NOTICES.txt`.
