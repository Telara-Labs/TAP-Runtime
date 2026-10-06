# TAP Runtime

TAP (Trusted Agent Primitives) lets agents build their own internal tools as
reusable blocks of code. We expect agents to write most of these primitives
as they work. Humans can write them too.

People ask agents to get work done across source control, ticketing, CRM,
and other company systems. A different project or account often needs the
same procedure. Much like engineers build software for people, agents can
put those repeatable steps in code, give them a defined interface, and call
them again with new inputs. The code handles the procedure and returns a
result or failure; the agent decides what to do next.

Read [Why TAP](WHY-TAP.md) for the thinking behind the project.

## Quick start

```
npm install -g @telaralabs/tap     # the tap CLI
tap setup                          # connect it to the agents installed here
tap discover                       # what you keep asking your agents to do
```

No account, registry or server is needed. MIT licensed.

`tap discover` looks for reusable procedures in local agent histories and
proposes primitives for review. To write one yourself, start with the
[authoring guide](docs/writing-a-primitive.md) and [examples](examples/).
A package contains `primitive.yaml` and an entrypoint (`main.sh`, `main.py`,
`main.js` or `main.ts`). The runner executes it in a WebAssembly sandbox,
using the host's tool connections and approval paths. Connected-tool
execution is supported in Claude Code and Codex; see [Where it runs](#where-it-runs)
for the other clients.

## An example: checking a release

An agent could write a primitive that takes a repository, a candidate commit,
and a set of release requirements, then:

1. Finds the changes since the previous release.
2. Retrieves the CI results for the candidate commit.
3. Follows references to the work items and reviews associated with those changes.
4. Handles pagination, matches records by their IDs, and checks the requirements.
5. Returns the checks that passed, anything missing, and links to the evidence.

The code carries results from one call into the next. The next release has
different commits, work items, and CI results, supplied or retrieved during
the run. The same procedure handles them. The agent uses the report to
decide what needs attention.

This is an example of a primitive you could build. The folders in
[`examples/`](examples/) show the runnable package format.

## What it does

- Runs the entrypoint inside a WebAssembly sandbox with no filesystem, no
  network and an empty environment. Everything the program does goes through
  the runner as a request.
- Executes only the host programs (`git`, `kubectl`, ...) and tools the
  manifest declares. Each request is checked, gated and recorded; undeclared
  requests are refused. Independent requests can run concurrently.
- Gates `write` and `destructive` effects. Fetches require origin approval,
  through a prompt or an explicit owner grant for the exact package digest.
  Local file reads need no effect prompt; tool reads still follow the client's
  ask rules and the server's annotations.
- Replays recorded responses when a run resumes. An interrupted change with
  an unknown outcome is left for a person to check.

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
| VS Code (GitHub Copilot) | preview | an extension using tools exposed by VS Code; Local chat MCP reads exercised, Copilot SDK visibility limited | the runner's; native trust and fetch forms exercised; extension write/stall release acceptance remains separate |
| claude.ai (web) | preview | a page published as an Artifact: [docs/web.md](docs/web.md) | only connector tools annotated read-only are permitted |

Other agents can connect to the local MCP server where they support MCP.
Tool execution depends on a supported bridge, an experimental relay, or an
explicit direct MCP backend; registration alone does not prove execution or
approval support. Headless clients may need explicit trust, fetch grants and
tool bindings. See [docs/bridge-research.md](docs/bridge-research.md) and
[docs/headless-and-sharing.md](docs/headless-and-sharing.md).

## Install

```
npm install -g @telaralabs/tap
```

installs the `tap` CLI. `tap setup` then connects it, as the MCP server
`tap`, to every supported agent installed on the machine and says what it
connected. It also refreshes discoverable skill pointers for saved primitives
in the shared TAP collection, so normal task requests can find them without
naming TAP. Save packages with `tap discover save <package-dir>`; setup does not
import arbitrary project folders. (npm's install step runs the same setup, but npm hides its output
and may skip install scripts.) `tap install --client <agent>` connects one;
`tap install --client all --print` shows what it would change.

To turn TAP off, run `tap remove`: it takes the `tap` entry out of every
agent's configuration (and Gemini CLI's hook with it). Do this before
`npm uninstall -g @telaralabs/tap`, which runs no cleanup of its own, or the
agents keep an entry for a program that is gone. `tap setup` turns it back
on. Nothing runs in the background: an agent starts `tap serve` for its own
session and stops it when the session ends. To pause TAP for a while, switch
the `tap` server off in the agent's own MCP settings (`/mcp` in Claude Code).
Releases include a signed checksum manifest. Install scripts check downloads
against hashes embedded when the release was built; independently checking the
signature against a trusted public key is a separate step:
[docs/install.md](docs/install.md).

Releases are built for macOS (arm64, amd64), Linux (amd64, arm64) and
Windows (amd64). CI tests the source on Linux, macOS and Windows. Each release
has a separate Launch acceptance workflow (`acceptance/launch`) for npm,
the install scripts and `go install` on all five platforms. Check the matching
workflow result before claiming those release installs passed. Historical
v0.1.12 passed on all five platforms
([run](https://github.com/Telara-Labs/TAP-Runtime/actions/runs/37360314957)).
v0.1.14 passed clean native launch checks on both Linux architectures, Windows
and Intel macOS ([run](https://github.com/Telara-Labs/TAP-Runtime/actions/runs/37373338835)).
Its Apple Silicon launch checks passed locally with an isolated HOME and PATH,
using existing Go caches; the clean hosted job was cancelled before any steps.
Those results do not establish a complete five-platform v0.1.15 launch run.

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
matching requests from the record. A change that was in progress when the
run stopped is not repeated, and is reported as unknown for a person to
check. This does not establish exactly-once behavior in remote systems. The program gets back the clock readings
and random bytes it took before. One process holds a run at a time. Records
are removed after 30 days; `--retention-days 0` keeps them.

## TAP Local: your primitives as an MCP server

`tap serve` exposes five fixed tools: `tap_search`, `tap_load`, `tap_run`,
`tap_status` and `tap_evidence`. It finds installed primitives in a local
collection and runs an exact `publisher/name@version` and package digest.
The tool list does not grow with the number of primitives. A client that
cannot show an approval prompt receives no implicit approval. Package trust,
fetch grants and tool bindings are separate owner decisions.

The first time a package runs, the person is asked whether it may, unless the
package only calls tools (each of those calls has its own gate). If the client
cannot present that question, a package that uses files, host programs, the
web, or no tools at all is declined until you run `tap trust PACKAGE-DIR` (`tap trust --list`, `tap trust --forget DIGEST-PREFIX`).
Trusting a package lets it start; every write it makes is still refused
unless someone approves it. Fetch approval is separate: the released runner
supports `tap trust --fetch-origin https://example.com PACKAGE-DIR` for an exact
declared origin and package digest. The grant is lost when package bytes
change. This option shipped in v0.1.13; plain package trust revokes existing
fetch grants for that digest.
Historical read check with Claude Code 2.1.287 and the published v0.1.4
binary: a model searched, ran and read the status of a primitive through the
five tools.

## Telemetry

Off unless an endpoint is set, through the standard OpenTelemetry variables
(`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, ...). Fetches
also honor the standard `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` settings;
TAP introduces no separate proxy setting. It sends
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

## TAP and Telara

We built TAP at [Telara](https://telara.dev/), the enterprise AI operating
layer. Telara gives connected AI clients company context, applies policy to
the actions it mediates, and keeps a record of that work. We're extending
the platform with verification, versioning, admin approval, and distribution
for TAP primitives so teams can review and share the tools their agents create.

TAP Runtime is independently usable without Telara. If you're working on
primitives across a team, [explore Telara](https://telara.dev/).

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report a vulnerability as described
in [SECURITY.md](SECURITY.md), not in a public issue.

## Licence

MIT, in [LICENSE](LICENSE). `third_party/sh/`, a copy of `mvdan.cc/sh/v3`
3.14.1 patched to run pipelines on wasip1, keeps its BSD 3-clause licence. A
release carries both, with the licences of everything compiled in, in
`THIRD_PARTY_NOTICES.txt`.
