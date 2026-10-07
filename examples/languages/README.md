# One tool, six languages

These independent packages implement the same operation with the same input
and output: read a declared task file, parse JSON, select a status, extract the
matching IDs and titles, and return a count and rows. Run from the repository
root so the fixture path resolves. No account, network, host command or effect
approval is needed.

```sh
tap examples/languages/bash '{"status":"open"}'
tap examples/languages/python '{"status":"open"}'
tap examples/languages/javascript '{"status":"open"}'
tap examples/languages/typescript '{"status":"open"}'
```

Each returns this JSON, with formatting differences:

```json
{"status":"open","count":2,"items":[{"id":"TAP-1","title":"Record terminal evidence"},{"id":"TAP-3","title":"Review extraction output"}]}
```

Change the input to `{"status":"done"}` to reuse the program: the result has
one row, `TAP-2`. The operation has one broker request per run: Bash's `cat`
requests the declared file read; the other programs call `tap.read`. JSON
parsing, filtering, extraction and counting happen inside the guest program.
No model performs these steps. The Bash guest uses built-in `jq`; it does not
launch your machine's Bash or jq.

| Language | Execution path | Author's build step |
|---|---|---|
| [Bash](bash/) | Bash-compatible WASM interpreter | None; supported built-ins and declared commands |
| [Python](python/) | CPython 3.12 WASM | None |
| [JavaScript](javascript/) | QuickJS-ng WASM | None; this is not Node.js |
| [TypeScript](typescript/) | Types removed, then QuickJS-ng WASM | None; the runner does not type-check |
| [Go](go/) | Packaged WASI Preview 1 module | Go's `wasip1/wasm` target; source and local TAP SDK included |
| [C++](cpp/) | Packaged WASI Preview 1 module | WASI SDK compiler; source, TAP header and pinned JSON header included |

The source-interpreter examples work with published TAP 0.2.8. Compiled
entrypoints require TAP 0.2.10 or this updated source runner; releases through
0.2.9 refuse them. Any other language must emit a compatible WASI Preview 1 module and
speak the same JSON-line broker protocol. This is a compatibility requirement,
not a promise that every language/library has been tested.

To prepare the compiled examples, build the current runner from source, then:

```sh
go build -o /tmp/tap-language-demo ./host
/tmp/tap-language-demo discover build --approve-build examples/languages/go
# Set cpp/compiler.json to your installed wasm32-wasip1-clang++ compiler first.
/tmp/tap-language-demo discover build --approve-build examples/languages/cpp
/tmp/tap-language-demo examples/languages/go '{"status":"open"}'
/tmp/tap-language-demo examples/languages/cpp '{"status":"done"}'
```

The build step is an explicit author operation and has the author's OS access.
It checks two builds, writes the packaged module and records `BUILD.json`.
Execution runs those compiled bytes without a compiler or host language runtime.
Every example includes a changelog; on revision update version/manifest and
changelog, and rebuild compiled output before validation or saving. See
[compiled packages](../../docs/compiled-primitives.md).

All six packages declare the same one-file read in `primitive.yaml`.
Removing it makes the real runner refuse the request. Bad input fails before
reading, and malformed task data fails rather than returning an empty success.
The paths are relative to the runner's working directory, not to the package.

Run the actual interpreter tests with an explicit runner:

```sh
python3 -B examples/languages/test_languages.py --runner /absolute/path/to/tap
```

They run 36 guest invocations: twelve successful changed-input runs, twelve bad
inputs, six malformed fixture reads, and six undeclared-read refusals.
Build both compiled examples before running the matrix.
See [verification](../VERIFICATION.md) for the recorded execution boundary.
