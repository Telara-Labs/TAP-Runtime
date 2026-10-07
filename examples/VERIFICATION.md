# Gallery verification

Evidence for TENG-3259. These are recorded checks of the example packages,
not a promise that every connected account or client will behave identically.

All twelve new manifests passed the source-built runner's `manifest check`.
The owning repository's `go test ./...` passed, including the host package.
The example unit suites passed 27 tests: six browser input checks, ten
desktop input/command/result checks, five API checks, and two each for the
meeting, draft and Jira examples.

| Group | Real execution observed | Limit of that evidence |
|---|---|---|
| Four browser packages | TAP dispatched to Microsoft Playwright MCP against the disposable localhost site; all four procedures passed | Requires the documented direct MCP backend. Local fixture, not a production account or the user's open browser |
| Browser approval | Without approval, refused before tool dispatch | Mutating Playwright calls resolve to destructive effects from its current annotations |
| Four macOS packages | Native `find`/`stat`, `sips`, `textutil`, and approved `open -R` ran on disposable files | macOS only; host programs have their own OS access |
| Desktop approval | Finder reveal refused without approval, then succeeded with approval | Successful process exit proves the OS accepted the reveal request; it is not a human review receipt |
| API release brief | An independent standalone CLI run returned TAP Runtime v0.2.7 release metadata and three successful check runs over two HTTP 200 requests. Earlier check-run HTTP 500 responses were reported as unavailable evidence | Check data is labeled a sample; the program does not assert that the ref is the release source or pronounce a release verdict |
| Meeting brief | Two reads through TAP's Codex/Telara path returned Calendar `items` and Gmail `messages` | The chosen query/window returned empty lists; it does not prove all event formats or pagination |
| Jira triage | One read through TAP's Codex/Telara path returned the gallery ticket | This connector pin and account were tested; alternative Jira providers remain separate |
| Draft reply | Write refused before dispatch without approval; message format checked against the connector implementation | No draft was created and no mail was sent. Successful draft-ID handling is unit-tested |

The Python tests use pure functions or a fake local broker to check program
logic. They are unit checks, not mocked Telara-service integration evidence.
Actual browser, host-program, public API and connected read runs are listed
separately above.

The standalone public-API receipt is `20261007T151811Z-6024b53c`: two GET
requests, zero refusals, exit 0. It used no client bridge or account. The
source-built runner was based on commit
`5dc7c0394ad76d125cdf632d3de5a895167552c8` plus these example files.

## Repeat the checks

Build the runner from the repository root:

```sh
go build -o /tmp/tap-gallery-check ./host
go test ./...
```

Run `manifest check` on any package you adapt. The unit suites need only
Python's standard library:

```sh
python3 -B -m unittest discover -s examples/browser-support -v
python3 -B -m unittest discover -s examples/desktop-support -v
python3 -B -m unittest discover -s examples/api-release-brief -v
python3 -B -m unittest discover -s examples/connector-meeting-brief -v
python3 -B -m unittest discover -s examples/connector-draft-reply -v
python3 -B -m unittest discover -s examples/connector-issue-triage -v
```

Follow each package's README for live execution. Review inputs and effects
before granting approval; account writes are separate from these unit checks.
