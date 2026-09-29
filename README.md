# tap-runtime

The TAP runner.

Runs a TAP primitive. A primitive is a folder with a `primitive.yaml` and one
entrypoint: a source file (`main.sh`, `main.py`, `main.js`) or a compiled `.wasm`.

Design: `telara-documentation/architecture/tap/34-the-baseline-primitive-is-a-program.md`
section 13. Ticket: TENG-3031.

**Status: built and tested on macOS arm64. Not released.** Cross-builds for
Linux and Windows compile and have not been run.

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
uses. To publish it must satisfy `manifest/manifest.v3.schema.json` in full.
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

## As an MCP server

    host serve

exposes one tool, `tap_run`. Add it to a client's MCP configuration and the
client can start a primitive, and the runner can ask the person there to
approve a change. A client that cannot show an approval prompt is never asked,
and every change under it is refused.

## Build, test, release

    export GOWORK=off
    go build -o bin/tap-runtime ./host
    go test ./...
    ./bin/tap-runtime pkg/deploy-check-py
    ./bin/tap-runtime install --client claude --print

    go run ./release build --version 0.1.0 --out dist --key release.key
    go run ./release verify --dir dist --pub release.pub

A release holds the runner for five platforms, the bash-compatible
interpreter, their checksums and the licence notices of everything compiled
in. It is reproducible: the same source and toolchain give the same bytes.

## Interpreters are downloaded, not bundled

On first use the runner fetches the interpreter for the entrypoint's language
into the user cache directory and checks it against a pinned sha256. A file
that does not match is refused. The list is `host/interpreters.go`.

| Language | Source |
|---|---|
| Python | CPython 3.12.0, vmware-labs/webassembly-language-runtimes |
| JavaScript | QuickJS-ng 0.17.0 |
| Bash | This repository's `guest-sh`. Nobody publishes one, so until this repository cuts releases it is built locally: `GOOS=wasip1 GOARCH=wasm go build -o <store>/sh.wasm ./guest-sh` |

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

    go run ./conformance/cmd/tap-conformance -- ./bin/tap-runtime serve

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
