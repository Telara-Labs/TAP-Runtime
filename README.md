# tap-runtime

The TAP runner.

Runs a TAP primitive. A primitive is a folder with a `primitive.yaml` and one
entrypoint: a source file (`main.sh`, `main.py`, `main.js`) or a compiled `.wasm`.

Design: `telara-documentation/architecture/tap/34-the-baseline-primitive-is-a-program.md`
section 13. Ticket: TENG-3031.

**Status: spike.** It proves the shape works. It is not the shipping runner: no
MCP tool calls, no client bridges, no resume, macOS arm64 only.

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

## As an MCP server

    host serve

exposes one tool, `tap_run`. Add it to a client's MCP configuration and the
client can start a primitive, and the runner can ask the person there to
approve a change. A client that cannot show an approval prompt is never asked,
and every change under it is refused.

## Build and run

    export GOWORK=off
    go build -o bin/host ./host
    go test ./host
    ./bin/host pkg/deploy-check-py
    ./bin/host --approve pkg/approve-check

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

## Known limits

- The Python interpreter imports file and socket calls. It is contained because
  the runner grants it nothing, not because the calls are absent.
- What follows a declared subcommand is not bounded: `kubectl get` allows any
  resource and any later flag.
- From the command line, approval is the `--approve` flag, which approves
  everything. Run as an MCP server (`host serve`) it asks the person at the
  client, once for each distinct action.
- No resume and no pipelining.
