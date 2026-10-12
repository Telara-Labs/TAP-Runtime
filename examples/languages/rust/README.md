# Rust task summary (WASI Preview 2 component)

This package performs the same task-summary operation as the other language examples. It reads the declared task fixture through TAP's JSON-line broker, filters by `status`, and returns the matching IDs and titles as JSON.

It is built as a WebAssembly component (Component Model, WASI 0.2) with `rustc --target wasm32-wasip2`. The runner tells a component from a WASI Preview 1 module by its preamble and gives it the same authority: arguments, the broker protocol on standard input and output, standard error, clocks and random bytes. It has no files, environment variables or network of its own.

The example uses only the Rust standard library. `src/tap.rs` is a small broker client and `src/json.rs` a small JSON reader, so the build downloads no crates. The same source also builds as a Preview 1 module with `--target wasm32-wasip1`, and both make the same broker requests.

Install the target with `rustup target add wasm32-wasip2`. The author build runs with a temporary `HOME`, so a rustup proxy cannot find its toolchain there: set `compiler.json` `rustc` to the toolchain's own compiler, as printed by `rustup which rustc`. Then run, from the TAP-Runtime repository root:

```sh
tap discover build --approve-build examples/languages/rust
```

It runs `python3 build.py` twice against frozen source, compares the components, and writes `main.wasm` together with `BUILD.json`, which records `"format": "wasip2-component"`. After building:

```sh
go run ./host examples/languages/rust '{"status":"open"}'
```

The package declares one read of `examples/languages/fixtures/tasks.json`. Removing that declaration makes the runner refuse the request.
