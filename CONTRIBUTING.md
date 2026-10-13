# Contributing

Issues and pull requests are welcome.

## Build and test

```
GOWORK=off go build -o /tmp/tap ./host
GOWORK=off go vet ./... && GOWORK=off go test ./... -count=1 -timeout 20m
(cd contract && GOWORK=off go test ./... -count=1)
(cd discover && GOWORK=off go test ./... -count=1)
node --test vscode/extension.test.js
node --test npm/test/*.test.cjs
python3 -B -m unittest discover -s examples/browser-support -v
python3 -B -m unittest discover -s examples/desktop-support -v
python3 -B -m unittest discover -s examples/api-release-brief -v
python3 -B -m unittest discover -s examples/connector-meeting-brief -v
python3 -B -m unittest discover -s examples/connector-draft-reply -v
python3 -B -m unittest discover -s examples/connector-issue-triage -v
python3 -B -m unittest discover -s examples/languages -p 'test_*.py' -v
```

Go 1.26. Tests that need an installed agent (Claude Code, Codex, Goose, Kilo,
Gemini CLI), `sqlite3`, `zstd` or the network skip themselves when it is
missing, and say so. CI runs the Go suites on Linux (amd64, arm64) and Windows
(amd64); on Windows the runner's suite runs the packages that start no host
program, plus one primitive in the sandbox. The language discovery command
reports an explicit skip unless supplied an explicit runner; it does not turn
a skipped acceptance test into a pass.

Tests that start installed agent clients or use configured accounts are
excluded by default. To opt into those tests, run
`GOWORK=off go test ./host -count=1 -live-clients`. The cross-client Jira read
acceptance is a separate explicit run:

```
GOWORK=off go test ./host -run '^TestLiveClientAcceptanceClaudeCodeAndCodex$' -count=1 -v -live-client-acceptance
```

It uses the connected Jira account; its optional controlled write requires a
prepared write plan and an explicit approval directory.

## Run the language acceptance matrix

The seven-language acceptance script executes real TAP guests, including
permission-refusal and malformed-data cases. It requires the Rust
`wasm32-wasip2` target and WASI SDK 34 with `wasm32-wasip1-clang++` available.
The Go, C++, and Rust guest builds use
[`examples/languages/README.md`](examples/languages/README.md). Build the
compiled guests first (the generated `main.wasm` files are ignored):

```
rustup target add wasm32-wasip2
GOOS=wasip1 GOARCH=wasm go build -C examples/languages/go -trimpath -buildvcs=false -o main.wasm .
python3 examples/languages/cpp/build.py
python3 examples/languages/rust/build.py
```

The source-built runner has no packaged shell interpreter. Put the runner and
shell guest in a fresh cache, then run the acceptance script with that runner:

```
mkdir -p /tmp/tap-language-cache/interpreters
GOWORK=off go build -ldflags "-X main.testCacheRoot=/tmp/tap-language-cache" -o /tmp/tap-language ./host
GOOS=wasip1 GOARCH=wasm go build -o /tmp/tap-language-cache/interpreters/sh.wasm ./guest-sh
python3 -B examples/languages/test_languages.py --runner /tmp/tap-language
```

The repository has three Go modules. The root
`github.com/Telara-Labs/TAP-Runtime` module builds the `tap` program and pins
the `contract` and `discover` modules at pseudo-versions. Therefore root
commands test the pinned module versions, not uncommitted edits in the sibling
directories. Run each changed module's suite from its own directory. To make
a module change available to the root module, publish the module revision
first, then update the root pin with `go get` and commit the resulting
`go.mod` and `go.sum` changes. Do not use a local replacement as release
evidence.

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
| `examples/` | runnable primitives used by docs and tests |
| `testdata/primitives/` | regression-fixture primitives used by tests; not the public example gallery |
| `discover/history/testdata/` | redacted or synthetic agent-history fixtures |

## Conventions

- Keep private issue references, internal decision notes and AI-session links
  out of public prose and incoming commit messages. CI checks public Markdown,
  schema descriptions, Go comments, and the incoming commit range for these
  references and AI coauthor attributions.
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

Maintainers release with one command, run from the repository root:

```
GOWORK=off go run ./release publish --version X.Y.Z --plan   # changes nothing
GOWORK=off go run ./release publish --version X.Y.Z
```

It commits the version bump, tags it, builds and signs the release from a
clean export of that commit, pushes, creates the GitHub release and waits
until npm serves the new version. It prints one JSON report, and rerunning it
resumes from the step that failed.

### Before announcing a release

Run the **Launch acceptance** workflow (Actions, Run workflow) with the new
version. On clean macOS (arm64 and Intel), Linux (amd64 and arm64) and
Windows machines it installs the published release from npm, the release
install script and `go install`; runs the README's commands, the first-run
dead ends, the example primitives and the write approval gate; verifies the
release signature against `release/release.pub`; and checks that
`tap discover` makes no network connections. The same test runs locally:

```
go test ./acceptance/launch -count=1 -v -launch-version X.Y.Z
```

### If a published release is broken

1. Point npm back at the last good version, so new installs get it:
   `npm dist-tag add @telaralabs/tap@<good> latest`.
2. Deprecate the bad one, with the reason:
   `npm deprecate @telaralabs/tap@<bad> "<what is broken>; install <good>"`.
3. On GitHub, mark the bad release as a pre-release, so `releases/latest`
   points at the last good one again, and add the same note to it.
4. Fix forward with a new version through the release command. A published
   npm version and a pushed tag are never reused.
