# Contributing

Issues and pull requests are welcome.

## Build and test

```
go build -o bin/tap ./host
go vet ./... && go test ./...
(cd contract && go vet ./... && go test ./...)
(cd discover && go vet ./... && go test ./...)
```

Go 1.26. Tests that need an installed agent (Claude Code, Codex, Goose, Kilo,
Gemini CLI), `sqlite3`, `zstd` or the network skip themselves when it is
missing, and say so. CI runs these suites on Linux (amd64, arm64) and Windows
(amd64); on Windows the runner's suite runs the packages that start no host
program, plus one primitive in the sandbox.

## Layout

| Path | What it is |
|---|---|
| `host/` | the `tap` program: runner, CLI, MCP server, installers |
| `bridge/` | how the runner reaches each agent's tools |
| `bind/`, `satisfy/` | matching a primitive's declared tools to real ones, and checking calls against their contracts |
| `journal/` | the run record that makes resume safe |
| `guest-sh/`, `third_party/sh/` | the sandboxed Bash interpreter |
| `contract/` | module: the manifest format and its validation |
| `discover/` | module: reading agent history and proposing primitives |
| `conformance/` | the kit that tests any runner from outside |
| `release/` | reproducible, signed release builds |
| `npm/` | the npm package |
| `examples/`, `pkg/` | primitives used by docs and tests |

## Conventions

- Tests run against real things: a real sandbox, a real agent when one is
  installed, real recorded sessions (redacted) under `testdata/`. Mocks of
  the runner's own parts are avoided.
- Never commit built binaries. Build into a scratch directory.
- A fixture recorded from a real session is redacted with
  `discover/cmd/discover-fixture` and must pass the redaction tests (no home
  paths, user names or credentials).
- Changing how an agent's history is read: add or update a recorded session
  under `discover/history/testdata/`, and keep
  `discover/integration/each_agent_test.go` passing.

## Releases

Maintainers release with one command (`go run ./release publish --version
X.Y.Z`); see [AGENTS.md](AGENTS.md).
