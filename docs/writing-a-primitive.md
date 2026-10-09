# Writing a primitive

A primitive is a folder with two files: `primitive.yaml`, which says what the
program may use, and one program. The runner runs the program in a sandbox
with no file system, no network, no processes and an empty environment. Every
effect the program has goes through the runner, which checks it against
`primitive.yaml`, asks the person before any change, and records it.

No Telara account is needed to write or run one.

```
hello/
  primitive.yaml
  main.py          # or main.sh, main.js or main.ts
```

## The smallest primitive

```yaml
apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.example, name: hello, version: 0.1.0}
execution: {entrypoint: main.py}
```

```python
print("hello")
```

```
tap hello/
```

`pkg/hello-sh`, `pkg/hello-py` and `pkg/hello-ts` are this, in each language.
They use nothing, so they run on any machine.

## Languages

| Entrypoint | Runs in | What the program has |
|---|---|---|
| `.sh` | the runner's bash-compatible interpreter (`guest-sh`) | bash syntax, pipes and redirection. `echo`, `cat`, `head`, `wc`, `jq` and `tap` are built in; `jq` takes `-r` and no other flag. Any other command is a host program, and must be declared |
| `.py` | CPython 3.12 compiled to WebAssembly | the standard library, and `tap` |
| `.js` | QuickJS-ng 0.17 | ES2023, `print`, `console.log`, `std` (`qjs:std`), and `tap` |
| `.ts` | QuickJS-ng, after the runner removes the types | as `.js`. Types are removed, not checked |
| `.wasm` | A packaged WASI Preview 1 module | Go, C++ or another compatible compiler; the TAP JSON-line broker protocol |

The runner downloads each interpreter the first time it is needed and checks
it against a pinned sha256. `tap fetch` downloads them all ahead of time.
A compiled package already contains its `.wasm` program, so execution downloads
no language interpreter and runs no compiler. The author needs the compiler
when building the package. See [compiled packages](compiled-primitives.md).

## What a program can do

Every one of these is a request to the runner. None of them works unless
`primitive.yaml` declares it.

### Python

```python
tap.tools()                              # the aliases bound on this client, as a list
tap.call("search", {"query": "x"})       # call a tool; returns its result, parsed from JSON when it is JSON
tap.call_many([("search", {...}), ...])  # send several calls, then read the answers, in the order given
tap.exec("git", ["log", "-5"])           # run a declared host program: {"exit", "stdout", "stderr"}
tap.read("in/notes.txt")                 # read a declared file, as text
tap.write("out/report.txt", text)        # write a declared file
tap.fetch(url, method="GET", body="", headers={})  # {"status", "body"}
```

### JavaScript and TypeScript

```js
tap.tools()
tap.call("search", { query: "x" })
tap.callMany([["search", { ... }], ...])
tap.exec("git", ["log", "-5"])
tap.read("in/notes.txt")
tap.write("out/report.txt", text)
tap.fetch(url, "GET", "", {})
```

### Bash

```sh
tap tools                               # bound aliases, one per line
tap call search '{"query":"x"}' | jq -r '.threads | length'
tap fetch https://api.github.com/repos/mvdan/sh
git log -5                              # a declared host program, written as usual
cat in/notes.txt                        # a declared file, read as usual
echo done > out/report.txt              # a declared file, written as usual
```

Bash sends one request at a time. Python and JavaScript can send several with
`call_many` / `callMany`.

### When the runner says no

A refused request raises in Python and throws in JavaScript. The error carries
`code`, so a program can tell the cases apart without reading the message:

| `code` | Meaning |
|---|---|
| `refused` | not declared, or the person did not approve it |
| `violation` | a tool's answer did not match what the primitive expects. `landed` says whether the call had already taken effect |
| `failed` | the call ran and failed |

A host program that exits non-zero is not an error. `tap.exec` returns its exit
code. In bash a refused command prints `REFUSED by host` on stderr and exits
non-zero.

## primitive.yaml

To run, a manifest needs `apiVersion`, `kind`, `metadata` and
`execution.entrypoint`, plus a declaration of everything the program uses. A
field the format does not have is an error, so a misspelt limit never reads
as no limit.

### tools: calls to the client's own connections

```yaml
tools:
  - {alias: threads, capability: gmail.threads.search, effect: read, optional: true}
  - {alias: emails,  capability: gmail.emails.search,  effect: read, optional: true}
```

The runner does not hold a credential. It finds, among the tools the client
(Claude Code or Codex) already has connected, the one that best fits the
capability, by name first and then by its argument schema. `alias` is the name
the program uses. `optional: true` means the primitive still runs when nothing
fits; `tap.tools()` says what was bound. `pin: {server: ..., tool: ...}` names
the exact tool when matching is not enough. Server names are chosen by whoever
connected the server, so the same server can be `telara` on one machine and
`claude.ai Telara` on another. When no server with the pinned name is
connected, the pinned tool binds on the one server that offers it, and the run
record shows both names; when several offer it, the person chooses once with
`tap bind --client <agent> server:<pinned name> <server>`.

A pin is an override, never a requirement. Some clients do not tell the
runner which tools they have (Kilo reports only which servers are connected;
Gemini CLI reports nothing). There the runner resolves only the capabilities
the primitive declares, one at a time, without asking for the client's whole
tool list: a mapping kept on this machine; then servers in the client's own
configuration whose name, command or address fits a well-known capability
(a Playwright or Claude in Chrome server for `tap.browser.use`, a Telara
gateway whose catalog holds the operation); then one question to the client,
"which of your tools does this?", answered with one tool name or none. An
answer binds only if its server is configured, connected where the client
says so, and not denied by the person's rules. What the tool does comes from
its server's annotation, never from the answer, so an unannotated tool is
treated as a write. A checked answer is kept per client, so later runs ask
nothing. A client that cannot be asked gets the question in the refusal,
answered with `tap bind --client <agent> <capability> <server>/<tool>`.
Nothing found blocks the run and names the missing capability. The receipt's
binding says how each tool was resolved (`resolved_by`) and what was checked
(`verified`).

`effect` is `read`, `write`, `destructive`, `financial` or `identity-admin`.
Anything but `read` is asked of the person before it happens. A tool whose
own annotation says it does more than the declared effect (for example
`readOnlyHint: false` on a tool declared `read`) is not bound.

Unpinned declarations can also bind operations behind a gateway. The host's
Telara adapter queries the connected server's read-only `telara_tool_search`
and `telara_tool_describe`, using the advertised integration/action envelope,
effect and parameter schema as live binding data. The operation participates
in the same name/contract matching as a direct tool. The program sends normal
operation parameters; the host fixes integration/action in the binding and
wraps them as `{integration, action, params}` at dispatch. The receipt records
both the logical capability and the actual server, dispatcher, operation and
schema digest. Multiple equally fitting connections use the existing binding
chooser. Other gateway protocols require a host adapter; TAP does not infer
connector support from a generic dispatcher name. Connection previews remain
inventory-only and may show these operations unresolved until runtime.
Client tool allowlists are checked on the actual dispatcher and catalog tools;
an operation hidden behind an allowed dispatcher need not itself be a listed
MCP tool. Explicit denials and approval rules for that operation still apply.

Some clients expose Telara actions only through the generic
`telara_execute_action` tool. Its annotation covers every action, including
writes, so it cannot establish that a particular action is a read. For a
primitive pinned to a specific Telara action, TAP checks that action's effect
through the read-only `telara_tool_search` catalog before dispatch. A read is
allowed only when the catalog returns an exact action name marked `read`; a
write, missing action, unavailable catalog, or failed lookup is refused. A
write primitive still goes through the ordinary approval gate. When a client
exposes the action-specific tool directly, that tool's annotation remains the
effect evidence.

### commands: host programs

```yaml
commands:
  - {command: git,     globals: ["-C <any>"], args: [log, "*"], effect: read}
  - {command: kubectl, globals: ["--context staging", "-n <any>"], args: [get, "*"], effect: read}
  - {command: kubectl, globals: ["--context staging", "-n <any>"], args: [delete, "*"], effect: destructive}
```

`command` is a program name, never a path. `args` is required: `["*"]` allows
any arguments, `[]` allows none. `globals` are flags allowed before the
arguments; `<any>` stands for one value. `env: [AWS_PROFILE, AWS_*]` names the
variables passed through; nothing else is. A program must be on the machine,
so a primitive that declares `kubectl` is refused on a machine without it.

Some programs run code the manifest cannot describe (`bash`, `sh`, `python`,
`node`, `docker run`, `kubectl exec`, `ssh`, `sudo` and others). They are
treated as `destructive` whatever the manifest says.

### files and fetch

```yaml
files:
  - {path: in,  access: read}
  - {path: out, access: write}
fetch:
  - {origin: "https://api.github.com"}
  - {origin: "https://*.atlassian.net", methods: [GET, POST]}
```

Paths are relative to the directory the runner is started in, and nothing
outside them can be read or written. A fetch origin is a scheme and a host,
with no path. One leading wildcard label is allowed, never over a public suffix
such as `*.com`. The person is asked before a primitive first reaches an
origin, even for a `GET`, because an address and its headers can carry data off
the machine; once they approve a `GET` origin it is kept for that exact version,
so later runs do not ask, and every full address is in the record. An agent
that cannot show a prompt runs a primitive that only reads when its own
settings let its model do the same unasked: read the web, and run shell
commands if the primitive runs a `read` program such as `git log`. A write, or any method but `GET` and `HEAD`, is asked of the
person each time it is a new kind of change. From the command line, add
`--approve` to let a primitive that fetches run.

### Checking a manifest

```
tap manifest check pkg/hello-py              # may it run?
tap manifest check --publish pkg/hello-py    # may it be published?
tap manifest complete pkg/hello-py           # the full form, with TODO: where a person must write
```

The full format is `contract/manifest/manifest.v3.schema.json`. Running needs
only what is above. Publishing to a registry needs the full form.

## Running

```
tap pkg/hello-py                   # changes are refused
tap --approve pkg/deploy-check-py  # changes are allowed, all of them
tap --approve --limit 3 pkg/x      # at most 3 changes of each kind
```

A positive limit is total consent for each kind in that CLI invocation.
Invoking `--resume` with `--approve --limit N` grants up to N new changes
of each kind; replayed requests are not made again. Version
0.1.17 mistakenly renewed depleted CLI allowances and exceeded this limit.
The built-CLI regression test exercises the correction.

Through Claude Code or Codex, the runner is the MCP tool `tap_run`, and each
kind of change is asked of the person in the client. A client that cannot show
that prompt has every change refused.

Each run is recorded, and a run that stopped can be continued with
`--resume <run id>`: requests already answered are answered from the record
and not made again.

## Automating work that agents do

Much of the work handed to agents can be partly automated: a browser session,
a desktop application, shell commands and files, an MCP connector, an HTTP
API, or a mix of these with steps only a person can take. The same three rules
apply to every kind. Design what the program does and what it hands back,
write it so it runs in any client, and test it before it is saved. Two worked
cases follow: [browsers](#browsers) and
[desktop applications](#desktop-applications-and-computer-use).

### Design: what the program does and what it hands back

Before writing code, write down:

- **The repeatable part.** The program owns the steps that go the same way
  each time: calls, navigation, polling, pagination, matching IDs, retries and
  checks. The surrounding request does not need to be automated.
- **The hand-back.** Judgement, choices the evidence cannot settle, sign-ins,
  CAPTCHAs, and changes the person must approve go back to the agent or person.
  Return them as a named status with what is needed, such as
  `{"status": "needs_person", "reason": "sign in to example.com"}`, not as a
  partial answer.
- **Observable states and completion evidence.** Name each state the procedure
  passes through and what proves it advanced (for a browser, see the
  [state table](#describe-observable-states-and-completion)). Completion needs
  evidence the source exposes: a last page, a total that matches, an exit code,
  an explicit empty state.
- **Unproven is not zero.** A missing result, a blocked view, a refused call, a
  timeout or a partial page is `unresolved` or `incomplete`, never an empty
  list, a zero count or a pass. Return the target, status, the value or
  `null`, the completion evidence, and the reason when unresolved.

Effects follow the work, not the objective. Declare each tool, command and
origin with the effect its provider gives it. A read-only goal does not lower
the effect of a click, a navigation or a typed key. Anything but `read` is
asked of the person.

### Run in any client

Clients lend different tools for the same job, so a primitive written against
one client's tool runs nowhere else. For each step:

| Surface | How to stay client-agnostic |
|---|---|
| Shell and files | `commands` and `files` run the same under every client; a declared program must be on the machine, so return `blocked` naming it when `tap.exec` is refused |
| HTTP APIs | `fetch` runs the same under every client; nothing to branch on |
| MCP connectors | declare the capability without a pin, so the runner binds whichever connected server fits; pin only when matching is not enough |
| Browser, desktop, anything a client provides its own way | declare each client's equivalent tool as `optional`, pick one from `tap.tools()`, and send it code every backend runs |
| A step only a person can take | return `needs_person` with what to do, then resume with that as an input |

Keep the procedure's logic in the program, or in code the program sends that
every backend runs the same way. Never script one client's private objects
(such as a REPL's own globals) as the procedure: those exist only in that
client. When no backend is bound, or the bound one cannot reach what is needed
(not signed in, app not installed), return `blocked` and name what is missing.
Never fall back to a guess.

A pin names a server and a tool. When no server with that name is connected,
the runner binds the pinned tool on the one server that offers a tool of that
name. A generic tool name can therefore bind somewhere else: a pin of
`{server: computer-use, tool: computer}` was observed binding Claude in
Chrome's `computer` tool, which acts only inside Chrome. Give each backend its
own alias and read the run's binding lines before trusting which one ran.

### Test before saving

Write `cases.json` and an oracle from the contract, not from the program,
before running the package. Cover at least:

| Case | What it proves |
|---|---|
| Normal | the ordinary result, matched against an answer established by hand |
| Empty | a verified empty result, distinct from a failed read |
| Changed count | a different number of records or pages than last time |
| Missing prerequisite | a missing input, program, sign-in or permission returns `blocked`, not a result |
| Ambiguous input | two matches, or a display name that is not unique, returns unresolved |
| Slow or partial loading | a page, list or call that loads late or stops part way returns incomplete |
| Unsupported client, no backend | with no tool bound, the program returns `blocked` naming what is missing |

Run the cases through the real runner, not by calling the code directly, on
each backend available: `tap --client claude <dir>` and `tap --client codex
<dir>` run the same package against each client's tools. A backend you could
not run is reported as not run, not as passed. Then:

```
tap discover validate --cases cases.json --freeze
tap discover validate <package> --cases cases.json --out receipts.json
tap discover save <package> --receipts receipts.json
```

Keep three kinds of evidence apart and say what each establishes:

- **Fixture results.** Parsing and classification from synthetic or recorded
  inputs. They prove no live identity, navigation, sign-in or completeness.
  Fixture validation of a primitive that reads a live service stays
  `validation: not_run` for the live part.
- **Live results.** Real identity, readiness and completion on the observed
  service, with unresolved items kept.
- **Installed execution.** The saved primitive runs through the client's normal
  `tap_run` route. A direct tool call or a source-built runner covers only that
  path.

Report requested, verified, empty and unresolved counts, the package reference
and digest, the execution path, and any blocker. Do not fall back to
model-driven work and report it as the primitive's result.

### Prove the execution path before scaling

Start with one small probe through the installed `tap_run` route the saved
primitive will use: confirm the binding, the caller context and that it reads
the intended target. Tell binding, caller-context, authorization and transport
failures apart from readiness or extraction failures. A failed bridge is a
reason to stop and diagnose that path, not to rewrite selectors. After saving,
repeat the probe against the exact saved reference and digest before the full
run. The broad CLI `--approve` used in examples is not permission for actions
on a person's live account; follow the host's approval flow.

### Bound execution and retain progress

Put polling, iteration and retry decisions in the program, with timeouts and
maximum pages, scrolls, dispatches and retries suited to the target. Keep
actions that share one live tab or window sequential. Return compact JSON
rather than whole pages or screens. End a path promptly on a persistent bridge
or authorization failure.

Keep per-item outcomes in the run record or a declared output file. Use
`--resume` to continue the same stopped run; for a new run, pass the list of
unresolved IDs. When combining runs, keep each item's source run and package
version, state the freshness of reused results, and do not call a mixed result
one clean pass. Reads may be retried within bounds; before retrying a possible
write, inspect its destination.

### Compare cost and time on the same workload

Measure the manual and primitive paths on the same inputs, evidence source and
completion criteria. Distinguish measured runs from extrapolations. Report
program runtime, end-to-end time and host dispatches, and separate one-time
authoring from repeat runs and from the assistant's launch, polling and
reporting overhead. A program with no model calls has zero internal model
tokens; its surrounding assistant still uses tokens. For prices, keep
uncached input, cached input and output (including reasoning, counted once) by
model and tier, cite the price source and date, and label estimates as
estimates.

### Make it fast

How fast a primitive runs is decided by whoever writes it. The runner adds
little; the program's shape decides how many round trips it makes, and round
trips dominate. Design for speed from the first version.

**Count round trips.** Every `tap.call` travels program -> runner -> agent ->
tool -> page or service and back. Measured through the real runner, one call
took about 0.7 s on Claude in Chrome and about 1.4 s on Playwright. A program
that makes 200 calls spends minutes in transit before it does any work. Count
the calls your design makes per input item, and per run, before writing it.

**Index once per run, then match every input against the index.** Do not walk
the source once per item. In one measured browser primitive, proving that a
single name was absent walked a 382-thread inbox in 150 to 270 calls (1.5 to
5.5 minutes), and that walk was repeated for every absent name; names found by
search took about 10 calls. Read the list once, build an index in the
program, then answer every input from it. Search first where search is
reliable, and fall back to the shared index, not to a fresh walk per item.

**Batch.** Send independent calls together (`call_many`, see `pkg/fan-out`),
and ask for many records per call when the tool takes a page size or a list
of IDs.

**Loop inside the backend when it allows.** One call that runs a loop inside
the page or service (scroll and collect until the list stops growing, read
every row of a table) replaces many calls that each do one step. Backends
differ: a page evaluation on Claude in Chrome or Playwright can loop for about
20 seconds, while Codex caps an evaluation at about 3 seconds. Keep a
bounded loop per call, return what it collected and whether it finished, and
continue in the next call when it did not.

**No fixed waits beyond what loading needs.** Wait for an observable state
(the element, the count, the end-of-list marker) with a timeout, not for a
fixed number of seconds. A fixed `sleep` is paid on every call, on every run.

**Declare a budget and report it.** State in the program the most calls,
pages and seconds a run of a given size should take, stop with the work done
and the rest marked `unresolved` when it is reached, and include the calls
made and the time taken in the result. A budget reached is reported, never
hidden.

### Measure and improve

Make it work, make it fast, then keep making it faster as it is used. Every
number below is already available to you.

**Measure every validation case.** For each case, record the tool calls, the
duration and the number of items left `unresolved`. The runner ends every
run with a line such as `12 call(s) and command(s) run, 0 refused, in 4.1s`,
which validation receipts keep in each case's `host_log`, and a `tap_run`
result ends with a `[stats: ...]` line giving tool calls, failed calls,
refusals, input items and duration. When a run made many calls for its
inputs, the result says so in one more line; treat that as a request to
improve the primitive.

**Compare versions on identical inputs.** Run the old and the new version on
the same inputs, against the same evidence and completion criteria, and
compare calls, duration and unresolved counts. A version that is faster on
different inputs proves nothing.

**Read the run record after real use.** Real runs meet inputs your cases did
not. Read the run's record (`tap_evidence` with the run id the result names)
to see which calls dominated and where the time went. When calls, duration or
unresolved items can drop without losing correctness, publish an improved
version: advance the semantic version, say in `CHANGELOG.md` what changed and
the before and after numbers, validate it, and save it.

**Never trade correctness for speed.** An item the program could not prove
stays `unresolved`; a faster version that reports it as found, empty or zero
is wrong, not faster. The cases that prove empty, ambiguous and partial
results must still pass on every new version.

### Browsers

The host owns authentication, browser and session access, binding and
authorization; the program owns navigation, polling, extraction and retries.
Use only observations the site permits. Do not copy credentials into a package
or substitute an export or private endpoint for the visible website the person
asked about.

The browser is a capability the runner provides. Declare one tool for it, with
no pin:

```yaml
tools:
  - {alias: browser, capability: tap.browser.use, effect: write}
```

The runner maps it onto whichever browser the client already lends: Claude in
Chrome, Codex's browser, or a Playwright MCP server the person configured. It
never starts a browser of its own. The program calls one operation at a time:

```python
tap.call('browser', {'op': 'navigate', 'url': url, 'ready': SIGNED_IN})
rows = tap.call('browser', {'op': 'read', 'function': '(a) => [...document.querySelectorAll(a.sel)].length', 'args': {'sel': 'li'}})
tap.call('browser', {'op': 'act', 'action': 'click', 'selector': 'li', 'index': 3})
tap.call('browser', {'op': 'act', 'action': 'insert_text', 'selector': 'textarea', 'text': 'Hello'})
tap.call('browser', {'op': 'wait', 'ms': 700})
tap.call('browser', {'op': 'close'})
```

- **navigate** opens an http or https URL. The first navigation opens a tab
  of the runner's own (it never drives a tab the person or another task is
  using) and chooses the browser. With `ready`, a page function that returns
  `true`, `false`, or `null` while the page has not decided, the runner polls
  it for up to `ready_timeout_ms` (default 20 s) and keeps the first browser
  that answers `true`, such as one signed in to the site. When none does, it
  keeps the first and answers `"ready": false` with what it tried, so the
  program can read the page and say why. `backend` names a browser
  (`claude-in-chrome`, `codex`, `playwright`, or a server name); otherwise the
  order is Claude in Chrome, Codex's browser, then Playwright.
- **read** runs a page function with `args` and returns its JSON value. All
  reading logic (finding records, identity, counts) lives in page functions,
  so every browser runs the same code. A page function only reads and returns
  at once: Codex runs it read-only (no element methods, no browser globals
  such as `encodeURIComponent`) and stops it after about three seconds.
- **act** clicks, scrolls into view, or enters text into the element `index`
  (default 0) of a CSS selector. A click lands where a pointer would, on the
  element at the target's centre. `insert_text` replaces the element's content
  with `text` (an editable region or a form field) the way typing or pasting
  would, so the page's own handlers see it; read the element back before you
  submit. On Codex the runner uses its Playwright locator (`fill` for text).
- **wait** pauses the program for up to 30 s between reads. The sandbox has
  no `time.sleep`.
- **close** closes the runner's tab. The runner also closes it when the run
  ends.

Navigate and act are writes and need approval; read, wait and close are
reads, unless the person's client asks before using that browser's tools.
Declaring the tool `effect: read` is refused. Page JavaScript runs only in the
runner's own tab, opened by an approved navigation.

The runner handles each browser's limits once: it encodes answers so they
survive each tool's quoting, reads answers longer than Claude in Chrome's
output limit (about 1000 characters) in parts, opens a visible window in
Claude in Chrome when it can (Chrome throttles timers and skips
scroll-triggered loading in background tabs), and starts Claude Code with
`--chrome` for a primitive that declares the browser.

What each client lends (checked on macOS with Claude Code 2.1 and Codex 0.161):

- **Claude Code** lends Claude in Chrome, which drives the person's own Chrome
  profile and its sign-ins, and any Playwright MCP server it has configured.
- **Codex** lends its browser (`cua_repl`) only from inside a Codex turn: run
  the primitive from Codex.
- **Playwright MCP** usually runs its own profile, which is not signed in
  (one per working directory unless started with `--isolated`). To use the
  person's signed-in Chrome instead, the person installs the Playwright
  Extension in Chrome and configures the server with `--extension`, for
  example `claude mcp add playwright-extension -- npx @playwright/mcp@latest --extension`
  or `codex mcp add playwright-extension -- npx @playwright/mcp@latest --extension`.
  On first use Chrome opens a page asking which tab to share; the runner
  navigates the tab chosen there, so choose one that is not in use.

Websites differ in ways that make a count look complete when it is not:

- **Virtualized lists** draw only the entries near the visible area; the rest
  are empty placeholders. Scroll each placeholder into view and read again
  before loading more, and count drawn entries, not list items.
- **Reversed scrollers** (newest at the bottom, as in chat history) report
  `scrollTop` 0 at the newest end and negative values toward the oldest.
  Judge "at the start of history" from the scroll direction, not from
  `scrollTop` alone.
- Keep progress (records seen, pages loaded) in the program, not in a page
  variable, and keep each answer compact: return short hashes of long record
  IDs and list each repeated value once.

#### Describe observable states and completion

| State | Evidence needed to advance |
|---|---|
| Target selected | Observed canonical URL or stable target ID within the intended origin |
| Identity verified | Selected recipient/account/entity matches the requested ID; a display name alone may be ambiguous |
| Ready | Expected heading/list/card is loaded and relevant loading indicators are absent |
| Extracted | Records and their stable IDs read from the permitted UI, with per-record classification |
| Continue | Observed next-page control/cursor or scrolling that exposes additional records |
| Complete | A terminal pagination/history boundary, supported by stable observations and any available total |
| Unresolved | Identity, readiness or completeness could not be established within the bounds |

Prefer accessible roles, labels and stable attributes over positional
selectors. Revalidate after navigation and when the page replaces a list or
dialog. Virtualized lists require accumulating unique IDs while scrolling,
because old rows leave the DOM. Count individual record IDs, not grouped
sender or date headings, and deduplicate across pages and overlapping scroll
windows. An unchanged viewport can mean loading stalled. A mismatch with an
advertised total remains incomplete, with the discrepancy reported. A verified
empty result needs the requested identity, a ready view and an explicit empty
state or a stable absence of records after loading; an absent search result,
blocked view or missing selector is unresolved, not zero.

The [browser examples](../examples/browser-support/README.md) exercise a
disposable local Playwright fixture. They illustrate declared effects and
bounded tool calls; they do not establish support for a signed-in native
browser or an arbitrary live website.

### Desktop applications and computer use

What each client lends the runner today (checked on macOS with Claude Code 2.1
and Codex 0.161; other clients not checked):

- **Codex** lends `cua_repl` `js`. Its runtime controls native applications
  as well as browsers: `cua.getApp(<name or bundle ID>)`, accessibility state
  (`getAXState`), screenshots, `click`, `setValue`, `typeText`, `pressKey` and
  `scroll`. The tool is annotated read-only although the code it runs clicks
  and types, so declare it `effect: write` (or stronger for what the procedure
  does). The first call in a fresh REPL can return the runtime's usage
  documentation instead of the expression's value; check the shape of every
  answer.
- **Claude Code** lends no desktop tool to the runner. Claude in Chrome's
  `computer` tool acts only inside Chrome tabs. A Claude Code session may have
  computer-use features of its own; they are not lent to a primitive.

So a desktop primitive has one backend today. Write it the same way anyway:

```yaml
tools:
  - {alias: cua_js, capability: desktop.repl.js, effect: write, optional: true, pin: {server: cua_repl, tool: js}}
```

The program decides the steps and checks; each call sends a short script that
performs one action and returns the fresh accessibility state, which the
program parses. Element indices are valid only for the observation that
produced them, so re-read the state after every action. Name the application
and window as inputs, verify the window title before acting, and treat a
missing app, a closed window or a permission prompt as `blocked` or
`needs_person`. On a client without `cua_js` the program returns
`{"status": "blocked", "reason": "no desktop automation tool is connected"}`.
Do not drive the person's real applications to test it; use a disposable app
or window you opened for the purpose.

## Examples

| Package | Shows |
|---|---|
| `pkg/hello-sh`, `hello-py`, `hello-ts` | the smallest primitive, in each language. Needs nothing |
| `pkg/recent-mail` | a tool bound from whichever Gmail connector the client has |
| `pkg/recent-mail-sh` | the same, in bash |
| `pkg/fan-out` | several calls sent together with `call_many` |
| `pkg/draft-gate` | a `write` tool, asked of the person first |
| `pkg/repo-report-sh` | `fetch` and `files`, and what is refused outside them |
| `pkg/deploy-check-py`, `deploy-check-js` | host programs, including a refused `destructive` one |

## Keep a package revision consistent

Authoring and Discover saves require a full semantic `metadata.version` and
`CHANGELOG.md` with a nonempty matching version heading. Update the manifest,
version and changelog when changing source, dependencies, interfaces,
permissions or package documentation. Agent-authored packages also keep their
`AUTHORING.json` name/publisher aligned. Different bytes under an installed
version or a lower version cannot be saved. Prior saved versions are retained.

For `.wasm`, declare the packaged source, exact toolchain and build recipe in
`provenance`. Explicitly review and run
`tap discover build --approve-build <package>` with the author's toolchain
before validation and saving. It checks two builds against the same original
inputs, writes the program and records `BUILD.json`. After changing inputs,
rebuild; stale output is refused. Keep cases and receipts outside the package.
The local build has the author's OS access. A freshness receipt is separate
from an isolated publisher's provenance check. Save and execution never build.
