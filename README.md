# TAP Runtime

```text
████████╗ █████╗ ██████╗ 
╚══██╔══╝██╔══██╗██╔══██╗
   ██║   ███████║██████╔╝
   ██║   ██╔══██║██╔═══╝ 
   ██║   ██║  ██║██║     
   ╚═╝   ╚═╝  ╚═╝╚═╝     
```

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

The [automation gallery](examples/README.md) includes twelve procedures you
can inspect and adapt: browser smoke, link, form and keyboard checks; local
macOS file, image and document tools; public API release evidence; and
connected calendar, email-draft and issue-triage tools. Each package explains
the host it needs and how to run it.

## What it does

- Runs the entrypoint inside a WebAssembly sandbox with no filesystem, no
  network and an empty environment. Everything the program does goes through
  the runner as a request.
- Executes only the host programs (`git`, `kubectl`, ...) and tools the
  manifest declares. Each request is checked, gated and recorded; undeclared
  requests are refused. Independent requests can run concurrently.
- Gates `write` and `destructive` effects. Fetches require origin approval,
  through a prompt or an explicit owner grant for the exact package digest; an
  approved read origin is kept for that version, so later runs do not ask.
  Local file reads need no effect prompt; tool reads still follow the client's
  ask rules and the server's annotations.
- Runs other primitives a primitive lists under `uses`, pinned by digest. Each
  child runs under its own manifest, approvals and record; the parent receives
  only its output, and one deadline and request limit cover the tree.
- Replays recorded responses when a run resumes. An interrupted change with
  an unknown outcome is left for a person to check.

## Where it runs

The runner calls a primitive's tools through the agent you run it from, with
that agent's own connections. How each agent lends them, and whose approval
applies:

| Agent path | State | How the runner reaches tools |
|---|---|---|
| Claude Code | supported | Claude Code control channel |
| Codex CLI | supported | Codex app-server; native UI tools may require caller turn metadata |
| Codex app | preview | Connected-tool reads through the app have not been verified; browser context is available only during a Codex turn |
| Gemini CLI | experimental | Gemini hook relay |
| Goose | experimental | Goose ACP tool-call extension |
| Kilo CLI | experimental | Kilo's experimental MCP call route |
| VS Code (GitHub Copilot) | preview | TAP extension using VS Code's tool interface |
| claude.ai (web) | preview | Artifact page described in [docs/web.md](docs/web.md) |

Installing the TAP MCP server does not establish that a client can execute a
primitive's connected tools or present its approval questions. Support and
approval behavior vary by client. See the [versioned compatibility evidence](docs/compatibility-0.2.28.md)
for the dated test matrix, its provenance, and the cases that were not reached
or were not repeated on the current release.

Other agents can connect to the local MCP server where they support MCP.
Tool execution depends on a supported bridge, an experimental relay, or an
explicit direct MCP backend; registration alone does not prove execution or
approval support. Headless clients may need explicit trust, fetch grants and
tool bindings. See [docs/bridge-research.md](docs/bridge-research.md) and
[docs/headless-and-sharing.md](docs/headless-and-sharing.md).
To run TAP inside a Docker sandbox, `tap kit export` writes a Sandbox Kit; see
[docs/sandbox-kits.md](docs/sandbox-kits.md).

## Install

```
npm install -g @telaralabs/tap
```

installs the `tap` CLI and the one runner that matches your machine (about
13 MB; the other platforms' runners are separate packages npm skips). `tap
setup` then connects it, as the MCP server
`tap`, to every supported agent installed on the machine and says what it
connected. It also refreshes discoverable skill pointers for saved primitives
in the shared TAP collection, so normal task requests can find them without
naming TAP. Save packages with `tap discover save <package-dir>`; setup does not
import arbitrary project folders. Setup also gives each agent a `tap-author`
skill: when a multi-step procedure keeps coming back and no saved primitive
fits it, the agent offers to write one, checks it with the runner and saves it
once the person agrees. (npm's install step runs the same setup, but npm hides its output
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
the install scripts and `go install` on all five platforms. Check the workflow
for the specific release before claiming those installs passed. Historical
launch results and their platform-specific limits are recorded in the
[compatibility evidence snapshot](docs/compatibility-0.2.28.md).

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
tap examples/recent-mail                     # run a primitive folder
tap manifest check examples/recent-mail      # may it run?
tap manifest complete examples/recent-mail   # the publishable form
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
tap examples/recent-mail                     # prints its run id
tap --resume <run id> examples/recent-mail
```

A run that stops is continued by starting the program again and answering
matching requests from the record. A change that was in progress when the
run stopped is not repeated, and is reported as unknown for a person to
check. This does not establish exactly-once behavior in remote systems. The program gets back the clock readings
and random bytes it took before, except a WASI Preview 3 component, whose
readings are not recorded. One process holds a run at a time. Records
are removed after 30 days; `--retention-days 0` keeps them.

## Review version changes

Read the [runtime migration history](CHANGELOG.md) before upgrading the
runner. To review two versions of a primitive, use
`tap diff [--json] OLD-PACKAGE-DIR NEW-PACKAGE-DIR` (available since 0.2.1).
It shows contract changes, permission declarations, code hashes,
and changed bytes under the same version. A changed digest requires review;
the report does not certify compatibility. See the [runnable version example](docs/versioning.md).

## TAP Local: your primitives as an MCP server

`tap serve` exposes seven fixed tools: `tap_search`, `tap_load`, `tap_run`,
`tap_status`, `tap_evidence`, `tap_save` and `tap_result`. It finds installed primitives in a
local collection and runs an exact `publisher/name@version` and package digest.
`tap_save` saves a package an agent wrote into the local collection after the
person agrees in a prompt; it exists because an agent's shell may be sandboxed
away from the collection (Codex), and it never publishes anywhere. A run still
going after 20 seconds keeps running and `tap_run` returns a handle;
`tap_result` waits on it, because clients stop waiting for one tool call (Codex
at about 30 seconds). A question the run asks after that is refused if nobody
answers it within two minutes.
The tool list does not grow with the number of primitives. A client that
cannot show an approval prompt receives no implicit approval. Package trust,
fetch grants and tool bindings are separate owner decisions.

Search, load, status and evidence return compact readable text by default using
standard MCP content. TAP sends one text representation, without duplicating the
payload in `structuredContent`. Input constraints, exact references/digests,
permission declarations and connection warnings remain visible. Full output
schemas and capability definitions are available with `tap_load`'s
`detail: true`; all four inspection tools accept this option to return their
complete JSON contract for programmatic callers. Scripts that parse these
tools' text as JSON must request it explicitly. Run/result program output and approval
behavior are unchanged. Clients still control the surrounding tool-call UI.

`tap_load` shows a connection summary for the exact requested ref and
digest. It resolves possible tool aliases against one current host inventory,
showing connection and tool names, declared and base effects, and write gates.
Missing, ambiguous and refused aliases stay visible. Dispatcher operations
require a runtime effect check. A client without live inventory reports
`inventory_unavailable`; it does not claim the package's pins are connected.
The preview executes no tools, prompts nobody and grants no trust. Execution
resolves again. See [connection previews](docs/connection-preview.md).
The full flat `connection_preview` object is included with `detail: true`.

`tap_evidence` ties a run to its executed package digest and saved manifest.
It returns declared tool, command, file and network permissions from that
snapshot, plus bounded journal events. Recorded tool attempts identify the
alias, resolved host connection and tool, effective effect, and nested
operation when resolved. Begin events are attempts; end and finish outcomes
distinguish refusals, failures and unknown results. Replays keep the original
record. Requests without journal IDs and `--no-journal` runs have no such trace.

Pass `include_manifest: true` to request the exact saved YAML. This is opt-in
because package defaults and examples may be sensitive. Runtime arguments,
credentials and results are excluded from evidence. Oversized YAML or
permission declarations are explicitly omitted; `truncated` marks omitted
events. Legacy runs report that snapshots are unavailable rather than reading
the current package. Evidence is a local execution record, not independent
attestation or a guarantee that a result is correct. See [run evidence](docs/evidence.md).

The first time a package runs, the person is asked whether it may, unless the
package only calls tools (each of those calls has its own gate). If the client
cannot present that question, a package that only reads runs when the agent's
own settings let its model read the web unasked; one that writes files, runs
host programs or sends anything but `GET`/`HEAD` is declined until you run
`tap trust PACKAGE-DIR` (`tap trust --list`, `tap trust --forget DIGEST-PREFIX`).
Trusting a package lets it start; every write it makes is still refused
unless someone approves it. Fetch approval is separate: the released runner
supports `tap trust --fetch-origin https://example.com PACKAGE-DIR` for an exact
declared origin and package digest. The grant is lost when package bytes
change. This option shipped in v0.1.13; plain package trust revokes existing
fetch grants for that digest.

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
GOWORK=off go build -o bin/tap ./host
GOWORK=off go test -count=1 ./... && (cd contract && GOWORK=off go test -count=1 ./...) && (cd discover && GOWORK=off go test -count=1 ./...)
```

The repository holds three Go modules: the root `github.com/Telara-Labs/TAP-Runtime`
module builds the `tap` program, `contract/` owns the manifest format and its
validation, and `discover/` reads agent histories and proposes primitives.
The root module depends on pinned pseudo-versions of `contract` and `discover`;
root commands use those pins, not edits in the sibling directories. Test a
module change from that module's directory. To update a root dependency,
publish the module revision first, then update the root pin with `go get` and
commit the resulting `go.mod` and `go.sum` changes. Do not use a local
replacement as release evidence. Tests that need an installed agent,
`sqlite3` or the network skip themselves when it is missing.

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
- Compiled WASI Preview 1 `.wasm` modules and WASI Preview 2 components are
  supported in the current source through the same broker and sandbox. A
  component gets no more than a module does. WASI Preview 3 components run in
  the official wasmtime 49.0.2, downloaded once (about 10 to 13 MB) the first
  time one runs, with the same broker and no more authority; see
  [WASI Preview 3 components](docs/compiled-primitives.md#wasi-preview-3-components). Go/C++/Rust authors compile before saving;
  execution requires the packaged module, not a host language runtime. See
  [compiled packages](docs/compiled-primitives.md). Releases through 0.2.9 predate this path; compiled support starts in 0.2.10.
- Each run compiles its interpreter (about 2 seconds); `--cache DIR` keeps
  compiled code between runs, but cached code is not re-checked against the
  interpreter's pinned digest, so it is off unless you ask for it.
- Through Codex, starting its app-server adds about 4 seconds to each run.
  A run still going after 20 seconds is handed back and finished through
  `tap_result`.

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

## License

TAP is MIT-licensed; see [LICENSE](LICENSE).

`third_party/sh/` is a vendored copy of `mvdan.cc/sh/v3` 3.14.1, patched so
shell pipelines run under wasip1. It keeps its original BSD 3-Clause license.

Release archives include `THIRD_PARTY_NOTICES.txt`, which lists the license of
every dependency compiled into the binary.

---

<a href="https://telara.dev">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/Telara-Labs/Telara-CLI/main/docs/logo-dark.png">
    <img alt="Telara" src="https://raw.githubusercontent.com/Telara-Labs/Telara-CLI/main/docs/logo-light.png" height="40">
  </picture>
</a>
