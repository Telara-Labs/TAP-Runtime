# tap-runtime

The TAP runner.

Runs a TAP primitive. A primitive is a folder with a `primitive.yaml` and one
entrypoint: a source file: `main.sh`, `main.py`, `main.js` or `main.ts`. A compiled `.wasm`
entrypoint is not supported yet: the request protocol it would speak is not
published.

Design: `telara-documentation/architecture/tap/34-the-baseline-primitive-is-a-program.md`
section 13. Ticket: TENG-3031.

**Status: v0.1.2 release; tested on macOS arm64 and Linux (amd64, arm64).** On
Windows (amd64) a subset runs in CI, on Windows itself: the packages that start
no program, and one primitive in the sandbox, which reads and writes what it
declared and is refused a write outside it. The rest of the suite drives Unix
programs and has not run there. macOS Intel has never run.

- Writing one: [docs/writing-a-primitive.md](docs/writing-a-primitive.md)
- Installing into Claude Code or Codex: [docs/install.md](docs/install.md)
- Running in claude.ai, in the browser: [docs/web.md](docs/web.md)

## Where it runs

| Client | State | How the runner borrows its tools |
|---|---|---|
| Claude Code | supported | Claude Code's own control channel |
| Codex (CLI and app) | supported | Codex's app-server |
| claude.ai (web) | preview | a page, published as an Artifact, whose code calls your connectors: [docs/web.md](docs/web.md) |
| VS Code (GitHub Copilot) | preview | an extension that calls the editor's tools. Proven in VS Code; not yet from Copilot chat |
| Gemini CLI | experimental | a hook that has Gemini make each call. Never run against Gemini CLI itself: Google no longer admits individual accounts to it |
| ChatGPT (web) | not supported | the matching feature, Sites with plugins, needs a Business, Enterprise or Edu workspace. Untested |
| Gemini, Copilot (web) | not supported | neither lets code reach its connected apps |
| Cursor, OpenCode | not supported | they list their tools and give no way to call them |


## What it does

- Runs the entrypoint inside a wasm sandbox with no filesystem, no network and
  an empty environment.
- Executes host programs the manifest declares (`kubectl`, `git`, ...) on the
  script's behalf, one gated and journaled call at a time.
- Refuses any command the manifest does not declare.
- Gates `write` and `destructive` commands unless the run is approved.

## The manifest: short to run, full to publish

    host manifest check pkg/recent-mail            # may it run?
    host manifest check --publish pkg/recent-mail  # may it be published?
    host manifest complete pkg/recent-mail         # the publishable form

To run, `primitive.yaml` needs a name, an entrypoint and what the primitive
uses. To publish it must satisfy `contract/manifest/manifest.v3.schema.json` in full.
`complete` derives what it can and marks with `TODO:` what a person must
write. A field the format does not have is an error, so a misspelt bound never
reads as no bound.

## Runs are recorded, and a stopped run can continue

    host pkg/recent-mail                 # prints its run id
    host --resume <run id> pkg/recent-mail

Every request a program makes is recorded before it is acted on and again
when it is answered. A run that stops is continued by starting the program
again and answering what it already asked from the record, so nothing is done
twice. A change that was in progress when the run stopped is not repeated and
is reported as unknown for a person to check. A read is simply asked again.

The program is given the clock readings and random bytes it took before, up
to the point it had reached, and the real clock after. One process holds a run
at a time: a second is refused and told which process holds it. Records are
removed after 30 days; `--retention-days 0` keeps them for ever.

## Telemetry

Off unless an endpoint is set. Configured by the standard OpenTelemetry
variables, which are the only environment variables the runner reads for
configuration: `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`
and the rest of that family. The protocol is OTLP over HTTP.

It sends events: what ran, what was approved, how each ended. What a call was
given and what it touched stay on the machine unless `--otel-payloads` is
passed. A collector that cannot be reached does not stop a run.

## TAP Local: your primitive collection as an MCP server

    tap serve

exposes five fixed tools: `tap_search`, `tap_load`, `tap_run`, `tap_status`,
and `tap_evidence`. It finds installed v3 primitives in a bounded local
collection, loads their declared inputs and effects, and runs an exact
`publisher/name@version` and package digest. Run status and evidence read the
local journal without resuming a run. The tool list does not grow with the
number of primitives. There is no publication tool or Telara account in this
local MCP.

`tap install --client claude` or `tap install --client codex` registers the
server with a local client. The runner borrows that client's connections and
checks each MCP call against its declared effect before dispatch; every
effectful call still needs approval. Tool-only primitives do not get a second,
whole-package prompt. Packages with local file, command, or web reach still
need first-run package trust as well as approval for changes. A client that
cannot show an approval prompt is never asked, and every change under it is
refused. A remote browser chat cannot reach this stdio server without a
supported desktop companion; the separate [web preview](docs/web.md) is not
the local MCP.

The five-tool surface has passed direct MCP and live Codex tests. Its Claude
Code live tool-call test remains unverified because that client's control
call did not return, including for a read-only search.

The npm distribution source is `npm/`. `npm install -g @telaralabs/tap`
installs the `tap` CLI.
When npm permits its install script, it also registers the local MCP with
installed Claude Code and Codex; `tap setup` performs registration explicitly
when scripts are disabled. The CLI includes `tap discover`; the MCP keeps the
five fixed tools above. See
[installation](docs/install.md).

## Find primitives in your own history

The runner's program is `tap`. `tap discover` reads the session history that
Claude Code, Codex and Cursor keep on this machine, groups the requests you
make again and again, and drafts each recurring one as a primitive:

    tap discover              # what it read, how it narrowed, the primitives
    tap discover --rejected   # also what each check removed, and why
    tap discover --review     # pick primitives to save for tap_run

It reads local files only and sends nothing anywhere (`discover/`).

## Build, test, release

    export GOWORK=off
    go build -o bin/tap ./host
    go test ./...
    ./bin/tap pkg/deploy-check-py
    ./bin/tap install --client claude --print

    go run ./release build --version 0.1.2 --out dist --key ~/.tap-release/release.key \
        --download-base https://github.com/Telara-Labs/TAP-Runtime/releases/download/v0.1.2
    go run ./release verify --dir dist --pub release/release.pub

A release holds the runner for five platforms, the bash-compatible
interpreter, `install.sh` and `install.ps1`, their checksums, the signature
and its public key, and the licence notices of everything compiled in. It is
reproducible: the same source and toolchain give the same bytes.

Pushing a `v*` tag to GitHub runs `.github/workflows/release.yml`: it tests the
modules, builds and signs the release, creates the GitHub release, and
publishes `@telaralabs/tap` with npm Trusted Publishing. Before enabling it,
configure npm Trusted Publishing for `Telara-Labs/TAP-Runtime` and
`.github/workflows/release.yml`. Future tag builds also need
`TAP_RELEASE_KEY` as a GitHub Actions repository secret (the hex-encoded
contents of the release signing key). Manual dispatch can package and publish
existing signed assets without that signing key.

`--download-base` is where the files will be served from. The runner is built
knowing the address and digest of its interpreter, and each install script
carries the digest of every runner. Without it the build is for checking only.

## Interpreters are downloaded, not bundled

On first use the runner fetches the interpreter for the entrypoint's language
into the user cache directory and checks it against a pinned sha256. A file
that does not match is refused. The list is `host/interpreters.go`.

| Language | Source |
|---|---|
| Python | CPython 3.12.0, vmware-labs/webassembly-language-runtimes |
| JavaScript | QuickJS-ng 0.17.0 |
| Bash | This repository's `guest-sh`, a file of each release. A runner built from source has no address for it: `GOOS=wasip1 GOARCH=wasm go build -o <store>/sh.wasm ./guest-sh` |

`tap fetch` downloads every interpreter ahead of time, for a machine
or a sandbox that will have no network when a primitive runs.

## The patched shell library

`third_party/sh/` is a copy of `mvdan.cc/sh/v3` 3.14.1 (BSD 3-clause, licence
included), with import paths rewritten to this module. It is kept in the tree
because upstream does not support pipelines on wasip1, where `os.Pipe` is
unimplemented. The changes, all in `interp/`:

- `stdin_js.go` is renamed `stdin_iopipe.go` (the `_js` suffix is itself a build constraint)
- its constraint is `js || wasip1` instead of `js`
- `stdin_os.go` is `!js && !wasip1` instead of `!js`

If upstream accepts that change, this directory is deleted and the library
becomes an ordinary dependency again.

## Conformance

    go run ./conformance/cmd/tap-conformance -- ./bin/tap serve

`conformance/corpus/` is data: binding, satisfaction and manifest cases with
their required outcomes, for a runner written by anybody in any language.
The kit tests a runner from outside, as an MCP server, with real packages and
a real directory. `conformance/testdata/badrunner` does everything wrong on
purpose, and a test asserts the kit fails it.

`corpus/independent.json` holds 40 cases written from the specification by a
session that did not write the runner and did not read its code. The rest of
the corpus was written by the runner's author.

## Known limits

- The Python interpreter imports file and socket calls. It is contained because
  the runner grants it nothing, not because the calls are absent.
- From the command line `--approve` agrees to everything, and `--limit N`
  caps each kind of change at N. Run as an MCP server (`host serve`) the
  runner asks the person at the client about each kind of change and how many
  to allow, and asks again when that number is reached.
- Bash sends one request at a time: the sandbox has one thread. Python and
  JavaScript can send several together with `tap.call_many` / `tap.callMany`.
- Codex accepts several calls at once and runs them one after another.

## Licence

MIT, in [LICENSE](LICENSE). `third_party/sh/` keeps its own BSD 3-clause
licence. A release carries both, with the licences of everything compiled in,
in `THIRD_PARTY_NOTICES.txt`.
