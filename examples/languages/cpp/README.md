# C++ task summary (WASI Preview1)

This package performs the same task-summary operation as the Python, JavaScript, TypeScript, Bash, and Go examples. It reads the declared task fixture through TAP's JSON-line broker, filters by `status`, and returns the matching IDs and titles as JSON.

It requires a WASI SDK compiler targeting `wasm32-wasip1`. The example vendors the TAP header and [nlohmann/json single-header release v3.12.0](https://github.com/nlohmann/json/releases/tag/v3.12.0) (MIT); the upstream release SHA-256 for the vendored `json.hpp` is `aaf127c04cb31c406e5b04a63f1ae89369fccde6d8fa7cdda1ed4f32dfc5de63`. To build, set the example-owned `compiler.json` `compiler` field to the installed `wasm32-wasip1-clang++` program name or its absolute path, then run:

```sh
tap discover build --approve-build examples/languages/cpp
```

Run the authoring command from the TAP-Runtime repository root. It invokes the declared build script twice against frozen source, compares the executables, and writes `main.wasm` together with `BUILD.json`. The build script passes fixed compiler arguments directly without a shell. After building:

```sh
go run ./host examples/languages/cpp '{"status":"open"}'
```

The package declares one read of `examples/languages/fixtures/tasks.json`. It uses `tap::Client::read`; the vendored C++ SDK also exposes ID-matched `tools`, `call`, `exec`, `read`, `write`, and `fetch` requests. Connector calls convert nonzero exits to failures; generic requests leave write exits to the caller, and unknown outcomes take precedence over the accompanying refused flag. The example does not open host files directly; the runner supplies the declared file result over the broker. The third-party license text is in `vendor/NLOHMANN_JSON_LICENSE.MIT`.
