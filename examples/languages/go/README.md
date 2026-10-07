# Go task summary (WASI Preview1)

This package runs the same task-summary operation as the Python, JavaScript, TypeScript, and Bash examples. It reads the declared task fixture through TAP's JSON-line broker, filters by `status`, and returns the matching IDs and titles as JSON.

The Go broker client is included in the local `tap/` package so this example builds without network access or an unpublished module version. It uses only Go's standard library and matches the canonical [`sdk/go/tap`](../../../sdk/go/tap/) client. Build with Go 1.26.1 or compatible Go support for `wasip1/wasm`:

```sh
tap discover build --approve-build examples/languages/go
```

Run the authoring command from the TAP-Runtime repository root. It invokes the declared compiler recipe twice against frozen source, compares the executables, and writes `main.wasm` together with `BUILD.json`. After building:

```sh
go run ./host examples/languages/go '{"status":"open"}'
```

The package declares one read of `examples/languages/fixtures/tasks.json`. It uses `tap.Client.Read`; the Go SDK also exposes ID-matched `tools`, `call`, `exec`, `read`, `write`, and `fetch` requests through `Request` and `Call`. `Call` reports nonzero exits as failures, while generic `Request` leaves write exits to the caller. Unknown outcomes retain their original message and take precedence over the accompanying refused flag. The example does not open host files directly; the runner supplies the declared file result over the broker. See [`sdk/go/tap`](../../../sdk/go/tap/) for protocol API and typed error handling.
