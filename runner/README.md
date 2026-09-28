# runner

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

## The vendored patch

`vendor/mvdan.cc/sh/v3/interp` differs from upstream 3.14.1 so that pipelines
work on wasip1, where `os.Pipe` is unimplemented:

- `stdin_js.go` is renamed `stdin_iopipe.go` (the `_js` suffix is itself a build constraint)
- its constraint is `js || wasip1` instead of `js`
- `stdin_os.go` is `!js && !wasip1` instead of `!js`

**Running `go mod vendor` discards this.** Reapply it, or land it upstream.

## Known defects

- Command matching is by argument prefix, so `git -C <dir> log` is refused
  against a declared `git log`.
- Host programs inherit the runner's environment and working directory.
- The Python interpreter imports file and socket calls. It is contained because
  the runner grants it nothing, not because the calls are absent.
- Standard input is not passed into host programs.
