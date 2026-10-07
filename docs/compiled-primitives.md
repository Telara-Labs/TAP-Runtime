# Compiled primitives

A primitive can use any language whose toolchain emits a compatible WASI
Preview 1 module and implements TAP's JSON-line protocol. The Go and C++
examples demonstrate this path. This does not promise compatibility with
native executables, WASI Preview 2 components, every compiler or every library.

The author compiles once when preparing a revision. The runner executes the
packaged `.wasm` directly in the same wazero sandbox used by source interpreters.
No compiler, native subprocess, language runtime installation or package-manager
operation runs during execution. The guest has no mounted host filesystem,
inherited environment or sockets. Files, commands, HTTP requests and connector
tools go through the existing declaration checks, bindings, approvals, budgets,
result contracts and journal. Standard libraries that require direct OS access
must use the broker instead.

## Package and build

Declare `execution.entrypoint: main.wasm`. Include all source and dependencies,
`CHANGELOG.md` with the manifest's current semantic version, and provenance:

```yaml
provenance:
  source: .
  toolchain: Go 1.26.1, WASI Preview 1
  build: GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -o main.wasm .
```

Those Go settings select the existing compiler target. C++ uses WASI SDK's
`wasm32-wasip1-clang++`; its example accepts an installed compiler through
`compiler.json`. Review your own build recipe and explicitly run:

```sh
tap discover build --approve-build <package>
tap manifest check <package>
tap <package> '{"status":"open"}'
```

The build command runs the author's local recipe twice against the same
snapshot, refuses source or dependency changes by the compiler, compares the output and writes `BUILD.json`. Source, dependencies,
manifest, changelog, executable bits and recipe determine freshness. Rebuild
after edits. The `.home` directory is reserved for temporary compiler files and cannot be authored as a package input. TAP's generated `SKILL.md` and save marker are reserved metadata
and are removed from build inputs. Save checks the receipt without compiling.
An isolated publishing verifier is still required for untrusted publication;
a locally writable receipt is not a signed attestation.

On Windows, compiled build recipes run through `sh.exe` from `PATH` or the
Git for Windows installation. Install Git for Windows to use `tap discover build`
for compiled primitives; ordinary primitive execution does not need a host shell.

Authoring/save provenance and validation requirements also apply: establish
`AUTHORING.json` from a real brief rather than fabricating evidence. Put test
cases and receipts outside the source bundle. For a new revision, advance
`metadata.version`, update the manifest and changelog, rebuild and validate.

## Wire protocol

TAP passes invocation arguments after argv[0] (`primitive`). For a structured
interface, argv[1] is its JSON input. Stdout is reserved for broker messages;
use a final `return` message for user output. Flush each newline-delimited JSON
request before waiting for one newline-delimited reply on stdin. Supply unique
string IDs and match every reply to its request. A request looks like:

```json
{"id":"1","method":"read","path":"examples/languages/fixtures/tasks.json"}
```

The reply carries the same ID. A successful `read` puts text in `result`.
A refused request sets `refused`; handle `violation`, `landed` and `unknown`
as failures with their original meaning. A nonzero command or file-write exit, failed connector call, or HTTP error
is a result to inspect, rather than proof that a broker transport failed.
Never automatically retry an unknown or already-landed write.

| Method | Request fields | Reply fields used |
|---|---|---|
| `tools` | none | `tools` |
| `call` | `alias`, `arguments` | `result` (JSON text when structured) |
| `exec` | `command`, `args`, optional `stdin` | `stdout`, `stderr`, `exit` |
| `read` | `path` | `result` |
| `write` | `path`, `stdin` | refusal/violation/unknown; also check `exit` |
| `fetch` | `url`, `http_method`, `headers`, optional `stdin` body | `status`, `result` body |

The manifest declares the same aliases, commands, file access and exact fetch
origins as a source primitive. Logical capabilities bind to the client's
connector through TAP; the guest does not connect directly to the gateway.

End with one terminal message, with JSON encoded inside its stdout string:

```json
{"method":"return","stdout":"{\"count\":2}\n","stderr":"","exit":0}
```

A nonzero terminal `exit` reports failure. The package output schema describes the caller
contract; the runner currently does not enforce it against the terminal
output. Validate actual outputs with held-out cases before saving. Bound tool
result contracts are checked through the existing broker path. The runner enforces the same guest memory,
time, dispatch and protocol-line limits. See the Go/C++ SDKs and
[language examples](../examples/languages/README.md) for complete programs.
