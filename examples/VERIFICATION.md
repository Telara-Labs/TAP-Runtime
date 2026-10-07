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

## Gateway operation binding (TENG-3261)

The three connector packages introduced operation binding in version 0.2.0 (current revision 0.2.1 adds the authoring changelog) and declare unpinned
operations with parameter/result contracts. Guest arguments use the operation's
normal names, rather than Telara's `params_` transport names. The host discovers
the connected gateway's catalog metadata and wraps the selected operation.

Real source-built Codex 0.147.0 runs verified unpinned Jira issue search and
Calendar/Gmail reads. Jira returned TENG-3261; Calendar and Gmail returned valid
empty lists for the chosen window/query. These runs do not establish populated
event, pagination or arbitrary other-connector compatibility. The unpinned
Gmail draft operation bound and then refused without approval, with zero writes.
No draft was created and no email was sent.

Runtime client-protocol tests cover the same declaration through a direct tool
and a gateway, argument/result contracts, effect changes, catalog permission
and failures, fixed selectors, duplicate metadata and ambiguous connections.
A real fresh-client check found that Codex's callable-tool allowlist was being
applied to hidden operations. A regression test and the host permission seam
now distinguish transport allowlists from explicit operation denials. New
operation bindings refuse when their current effect cannot be verified.

The local root Go suite, contract and discover module suites, npm's 15 tests,
the 27 example Python tests, static checks and all twelve gallery manifest
checks passed during verification. Final source and release receipts are
recorded on TENG-3261. Other gateways require their own discovery adapter;
matching the name alone does not translate incompatible parameter semantics.

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

## Source-language showcase and onboarding corrections (TENG-3259)

The [language examples](languages/) were run with the actual public signed
macOS arm64 TAP 0.2.8 binary, rather than a global install or a substitute
interpreter. Bash, Python, JavaScript and TypeScript each read the declared
task fixture and returned the same selected IDs/titles/count for both `open`
and `done` inputs. Each successful run made one broker read and had zero
refusals. The real-runner test suite passed four tests comprising 24 guest
invocations, including malformed-input/data failures and refusal when the read
declaration is removed. All 27 example manifests passed `manifest check`.

Successful open-input receipts: Bash `20261007T195802Z-e383f52f`, Python
`20261007T195804Z-a8d8f4f1`, JavaScript `20261007T195807Z-7ed1b261`, and
TypeScript `20261007T195808Z-d77031da`. The language source is new local work;
these runs establish compatibility with the released runner, not inclusion of
the new examples in a published release.

The corrected desktop manifests (version 0.1.1) allow one absolute path in
their final argument, replacing the final bare `*` that also allowed multiple
files or no path. The Go regression test confirms additional paths, missing
paths and relative paths are denied for all four manifests. Actual inventory,
image and document reads passed on disposable macOS files: respectively
`20261007T195808Z-e71ec73f`, `20261007T195809Z-2035d249`, and
`20261007T195810Z-b02747a4`. Finder without approval was refused with zero
commands dispatched (`20261007T195811Z-2485a17c`). This new check did not open
Finder. The existing 27 Python example unit tests also passed.

The corrected approved public API recipe passed with two GET requests and
zero refusals (`20261007T195812Z-860b9439`); its output reported TAP v0.2.8 and
four successful sampled check runs. This remains evidence rather than a
release-readiness verdict. The gallery's old save recipe was actually tested
and failed because `AUTHORING.json` was absent. The guidance now requires an
adapted, provenance-backed authored package instead of claiming a gallery
folder can be saved as-is.

## Compiled-language authoring and execution (TENG-3059, TENG-3275)

The updated source runner executes packaged WASI Preview 1 modules directly.
Go and C++ now implement the same task extraction as the four source languages.
The six-language suite executes 36 real guests: twelve successful changed-input
runs, twelve invalid inputs rejected before reading, six malformed-data failures,
and six undeclared-read refusals. This verifies these toolchains and protocol
paths; it does not establish compatibility with every language or library.

Both compiled packages were built through explicit `discover build` operations,
which compare independent builds and write source/artifact receipts. Fresh
packages passed validation and saving; source edits invalidated the receipt and
were refused before replacing the installed bytes. Tests also cover rewritten
compiler inputs, old build receipts, consent snapshots, immutable versions,
retained exact-version execution, guest filesystem/environment isolation, tool
refusals, approved writes and CPU cancellation.

A real Go guest using an unpinned `jira.issue.get` declaration executed through
Codex 0.147.0 and Telara's gateway. Receipt `20261007T210703Z-cdc7b85a` records
one Jira read, zero refusals, exit 0, with the operation mapped to the gateway's
execute-action transport. No account write was attempted. This verifies the
configured gateway/account and operation, not arbitrary MCP implementations.

The complete host suite passed in 715.131 seconds; the complete release suite
passed in 230.564 seconds after fixing its clean archive export hang. The Go
and C++ SDK tests cover combined unknown/refused frames and failed connector
calls. The independent Opus 5.5 review findings were corrected before release.
Releases through TAP 0.2.9 predate this compiled path and refuse `.wasm`
entrypoints. Compiled examples require TAP 0.2.10 or this updated source runner.
