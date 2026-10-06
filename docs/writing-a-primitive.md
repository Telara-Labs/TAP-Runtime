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

The runner downloads each interpreter the first time it is needed and checks
it against a pinned sha256. `tap fetch` downloads them all ahead of time.

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
the exact tool when matching is not enough.

`effect` is `read`, `write`, `destructive`, `financial` or `identity-admin`.
Anything but `read` is asked of the person before it happens. A tool whose
own annotation says it does more than the declared effect (for example
`readOnlyHint: false` on a tool declared `read`) is not bound.

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
the machine; they are asked once per origin in a run, and every full address is
in the record. A write, or any method but `GET` and `HEAD`, is asked of the
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
The built-CLI regression test exercises the correction; TENG-3036 records
the source and released verification separately.

Through Claude Code or Codex, the runner is the MCP tool `tap_run`, and each
kind of change is asked of the person in the client. A client that cannot show
that prompt has every change refused.

Each run is recorded, and a run that stopped can be continued with
`--resume <run id>`: requests already answered are answered from the record
and not made again.

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
