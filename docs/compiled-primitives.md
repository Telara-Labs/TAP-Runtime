# Compiled primitives

A primitive can use any language whose toolchain emits a compatible WASI
Preview 1 module, WASI Preview 2 component or WASI Preview 3 component and
implements TAP's JSON-line protocol. The Go and C++ examples are Preview 1
modules; the Rust example is a Preview 2 component, and the same source builds
as a Preview 3 component. This does not promise compatibility with native
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
- uses threads, UTF-16 strings, a start function, its own resource types or a
  nested component that contains code;
- defines more than one linear memory, because the memory ceiling applies to
  each memory.

These are the components that `rustc --target wasm32-wasip2` and other
toolchains produce for a program with a `main` function. A component that
imports WASI 0.3 interfaces, or uses async functions, streams, futures or
error contexts, is a Preview 3 component and runs as described in
[WASI Preview 3 components](#wasi-preview-3-components).

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

## WASI Preview 3 components

A component that needs WASI Preview 3 (WASI 0.3: async functions, streams and
futures, and the 0.3 interfaces built on them, as `rustc --target
wasm32-wasip3` produces) runs in a separate process: the official
[wasmtime](https://github.com/bytecodealliance/wasmtime) command-line program,
version 49.0.2. The runner's own loader does not implement the async parts of
the Component Model. Preview 1 modules and Preview 2 components are not
affected: they keep running inside the runner, and someone who never runs a
Preview 3 component never downloads wasmtime.

The runner tells a Preview 3 component apart when it reads the entrypoint, and
checks its imports as it checks a Preview 2 component's: only the WASI packages
`wasi:cli`, `wasi:clocks`, `wasi:filesystem`, `wasi:random` and `wasi:sockets`
(and `wasi:io` at 0.2), never `wasi:http`, never an interface of your own. A
component that does not export `wasi:cli/run` is refused.

**What the process is given.** The same as an in-process guest: its arguments
after `primitive`, the broker's protocol on standard input and output, standard
error, clocks and random bytes. The command line is fixed in the runner:

```
wasmtime run -C cache=n -S p3=y
  -S inherit-env=n -S inherit-network=n -S tcp=n -S udp=n
  -S allow-ip-name-lookup=n -S http=n -S tls=n -S config=n -S keyvalue=n -S nn=n
  -W max-memory-size=536870912 -W max-memories=1
  --allow-precompiled --argv0 primitive <compiled component> <args...>
```

No `--dir` and no `--env` is ever passed, so the guest has no directory and
no environment variable; sockets and name lookup are denied, and wasmtime is
started with an empty environment of its own and reads no configuration
file. Its memory ceiling is the runner's guest limit (512 MiB) with one
linear memory. The process is killed (on Windows too) when the run's time is
up, when the run is interrupted, and once it has returned its result. Every
file, command, fetch and tool request goes through the broker exactly as it
does for a module.

**Obtaining wasmtime.** The first time a Preview 3 component runs, the runner
looks for wasmtime in its cache and then on `PATH`. A copy is used only when
it is the official build of 49.0.2: the program's SHA-256 must equal the
official build's for this platform. The runner checks only the digest and
never runs a copy to ask its version, so a copy that is not the official
build is never executed. A
build of the same version from source or from a package manager is a
different program, and is not used; the runner says why in one log line.
Otherwise it downloads the official release archive for this platform once
(about 10 to 13 MB; about 60 to 70 MB once extracted), checks the archive and
the program inside against SHA-256 digests shipped in the runner, and keeps
the program in its interpreter cache. Builds are pinned for macOS (arm64,
x86-64), Linux (arm64, x86-64) and Windows (x86-64); on any other platform a
Preview 3 component is refused with a message that says so.

**Compiling once.** wasmtime compiles the component the first time it runs,
into the runner's compiled cache next to the component's SHA-256. Later runs
load the compiled file and start in milliseconds. The cache keeps one
wasmtime version: a newer runner that pins another version removes the old
program and its compiled files.

**Resumed runs and the clock.** A Preview 3 component reads the real clock
and real random bytes, and these readings are not recorded. A resumed run of
a Preview 3 component replays the requests the run already answered, as for
any primitive, but it does not give the program back the clock readings and
random bytes it took before. A program whose requests depend on them may
send a different request the second time; one that differs from the request
the record holds under the same id stops the run, as it does for any program
that does not follow its record.
Preview 1 and Preview 2 programs are given their earlier readings.
