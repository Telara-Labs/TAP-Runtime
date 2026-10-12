# Compiled primitives

A primitive can use any language whose toolchain emits a compatible WASI
Preview 1 module or WASI Preview 2 component and implements TAP's JSON-line
protocol. The Go and C++ examples are Preview 1 modules; the Rust example is a
Preview 2 component. This does not promise compatibility with native
executables, every compiler or every library.

The author compiles once when preparing a revision. The runner reads the
entrypoint's preamble to tell a core module from a component, and executes the
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
`compiler.json`. Rust builds a component with `rustc --target wasm32-wasip2`;
its example names the toolchain's `rustc` in `compiler.json`. Say which format
the build produces in `provenance.toolchain`. Review your own build recipe and
explicitly run:

```sh
tap discover build --approve-build <package>
tap manifest check <package>
tap <package> '{"status":"open"}'
```

The build command runs the author's local recipe twice against the same
snapshot, refuses source or dependency changes by the compiler, compares the output and writes `BUILD.json`. The receipt's `format` is read from the
built artifact: `wasip1-module` or `wasip2-component`. Save refuses a receipt
whose format does not match the packaged entrypoint. Source, dependencies,
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

## WASI Preview 2 components

A component runs with the same authority as a Preview 1 module: its
arguments, the broker's protocol on standard input and output, standard error,
clocks and random bytes. It gets no preopened directory, no environment
variables, no working directory and no terminal. Socket creation and name
lookup return `access-denied`, so a standard library's `TcpStream::connect` or
`UdpSocket::bind` fails inside the guest. Any other WASI function that would
need a host resource traps when called. Every file, command, fetch and tool
request goes through the broker exactly as it does for a module.

The runner refuses a component before it runs when it:

- imports an interface outside the WASI 0.2 packages `wasi:cli`, `wasi:io`,
  `wasi:clocks`, `wasi:filesystem`, `wasi:random` and `wasi:sockets`. This
  includes `wasi:http` and any interface of your own;
- does not export `wasi:cli/run` (it is not a command);
- uses async, threads, error contexts, UTF-16 strings, a start function, its
  own resource types or a nested component that contains code;
- defines more than one linear memory, because the memory ceiling applies to
  each memory.

These are the components that `rustc --target wasm32-wasip2` and other
toolchains produce for a program with a `main` function.

### How the runner executes a component

wazero, the runner's engine, runs core modules only. A component is a set of
core modules plus instructions that wire them together and to the interfaces
they import. The runner reads those instructions, instantiates each core
module in wazero in the order the component gives, and supplies every
imported WASI function itself. It does not register WASI Preview 1 for a
component. The guest therefore runs in the same engine, under the same memory,
time, dispatch and protocol-line limits, and is stopped the same way.

This approach was chosen over the alternatives:

- Embedding wasmtime (through cgo) would add a C toolchain to every release
  build, cross-compilation for each of the five release platforms, and tens of
  megabytes to the runner. The runner is built today with `CGO_ENABLED=0`.
- A third-party component runtime for Go would put code the project does not
  review inside the sandbox boundary, and its WASI implementation would grant
  what that project grants rather than what TAP grants.
- Lowering at load time keeps the runner pure Go and keeps the list of what a
  component can reach in one place (`internal/component/wasi.go`). The cost is
  that the runner supports the part of the Component Model that command
  components use, and refuses the rest with a message that names it.

### Broker protocol for components

A component speaks the same JSON-line protocol as a module, over
`wasi:cli/stdin` and `wasi:cli/stdout`. There is no TAP WIT interface. One
protocol keeps one broker path, one set of declaration checks and one journal
for both formats, so a primitive built both ways makes the same requests and
returns the same output. The SDK clients need no second transport, and a
component can be built from the same source as a module. A typed WIT
interface could be added later if it mapped to the same methods.
