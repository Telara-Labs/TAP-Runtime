---
name: tap-author
description: "Use at the start of every request to look something up, check something or do something, even one that names a specific commit, ticket or file: call tap_search first, with a few words describing the kind of task, to find a saved TAP primitive that does it in one step. Afterwards, offer to save the procedure as a primitive only when tap_search says the person asked for this kind of task before."
tap-owned: tap-author
---

# Save a recurring procedure as a TAP primitive

A TAP primitive is a small program, run by the TAP runner in a sandbox, that
does one procedure with new inputs each time: the calls, the pagination, the
matching of IDs, the checks. Saving one means the next request of that kind is
answered by running it (`tap_run`) instead of redoing every step by hand.

## When to offer one

Do the person's task first, normally. Then offer a primitive only when all of
these hold:

1. `tap_search` found no saved primitive for it.
2. It recurs. Either the person said so ("every release", "again", "for each
   customer"), or your own history shows the same kind of request before. For
   Claude Code, look in `~/.claude/projects/*/*.jsonl`; for Codex,
   `~/.codex/sessions`. Search for the distinctive words of the request, not
   its exact values.
3. It has several dependent steps that would be repeated the same way with
   different inputs. A single lookup is not worth a primitive.

If they hold, say so in one line at the end of your answer and ask before
writing anything, for example: "You've asked for this release check before.
Want me to save it as a TAP primitive so next time it runs as one step?" If
the person declines, do not offer again for the same procedure in this session.

## Writing it

Draft inside the workspace you are working in, in `.tap/drafts/<name>/`
(a relative path), never in your home folder or anywhere outside the
workspace: some agents may only touch their workspace, and a prompt for
another folder may have nobody to answer it. Run `tap discover brief` (below)
first; it creates `.tap/drafts/` with a `.gitignore` that keeps everything in
it out of git. Keep `cases.json` and receipts in `.tap/drafts/`, beside the
package, not in it. The package holds `primitive.yaml` and one program (`main.py`,
`main.js`, `main.ts` or `main.sh`). Go, C++ and other compiled languages
use a packaged WASI Preview 1 `.wasm` entrypoint with source and an explicit
build recipe. See the guide for the JSON-line SDK protocol. The guide, with every field, is
https://github.com/Telara-Labs/TAP-Runtime/blob/main/docs/writing-a-primitive.md

```yaml
apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: local.me, name: release-check, version: 0.1.0, description: "One line saying what it checks and returns."}
execution: {entrypoint: main.py}
tools:            # tools of the agent's own MCP servers that you just used
  - {alias: commit, capability: gitlab.commit.get, effect: read, pin: {server: <server name>, tool: <tool name>}}
fetch:            # or plain HTTPS reads of public APIs
  - {origin: "https://gitlab.com"}
interface:
  inputSchema:
    type: object
    required: [project, candidate]
    properties:
      project: {type: string}
      candidate: {type: string}
```

- Inputs arrive as one JSON object in `sys.argv[1]` (Python). Make the values
  that change between requests inputs; never fix them as constants.
- Take inputs in the words the person uses when asking: a person's name, a
  ticket key, a repository or customer name. The program finds the rest
  itself (the conversation, record ID, URL or thread) the way it would be
  found by hand: a search, then a fuller listing, then any stated fallback.
  Never require a value the caller could only get by doing the procedure's
  own lookups or browsing first; accept it as an optional shortcut. When the
  lookup finds nothing or more than one match, return `not_found` or
  `ambiguous` with what was tried, never a guess. A send-a-message primitive,
  for example, takes a name and the text; the conversation link is optional.
- `tools:` are tools of an MCP server the agent is connected to. Your own
  built-in tools (web fetch, shell, file reads) are not: declare a web read
  under `fetch:` and a program such as `git` under `commands:`. Agents that
  cannot lend their connections refuse a package with required `tools:`.
- Declare only what the program uses. Reads are `effect: read`; anything that
  changes something is `write` or stronger and is asked of the person.
- Name the operation, not the route you happened to reach it by: declare
  `gitlab.list_projects`, not a gateway's dispatcher such as
  `telara_execute_action` or `gitlab.execute_action`. The runner finds the
  route on each client; an operation it can match there, a dispatcher it
  cannot.
- In Python, `tap.call(alias, args)` calls a declared tool and
  `tap.fetch(url)` returns `{"status", "body"}`.
- Print one JSON object: the verdict, what passed, what is missing, and links
  to the evidence. When a read comes back partial (a page limit, a count that
  does not match), report it as incomplete. Never count incomplete evidence
  as a pass.

## Automating work in a browser, an app or anything a client provides

Read the guide's
[automation section](https://github.com/Telara-Labs/TAP-Runtime/blob/main/docs/writing-a-primitive.md#automating-work-that-agents-do)
first (or `docs/writing-a-primitive.md` in a TAP checkout). It applies to
browsers, desktop apps, shell and files, MCP connectors, HTTP APIs, and mixed
steps that need a person.

- Design it. Decide what the program does and what it hands back (sign-ins,
  judgement, approvals) as `needs_person`. Name each observable state and the
  evidence that completes it. A missing, refused, blocked or partial read is
  `unresolved` or `incomplete`, never zero or a pass.
- Write it for any client. Declare each client's equivalent tool as
  `optional`, branch on `tap.tools()`, and keep the logic in code every backend
  runs (for a browser: navigate, read with page JavaScript, act on a
  selector; Codex reads page JavaScript read-only, so act through its
  locator there). Never script one
  client's private objects. With no usable backend, return `blocked` naming
  what is missing. Declare effects as the provider classifies the action; a
  click or navigation is not a read.
- Test before saving. Write `cases.json` covering normal, empty, changed count,
  missing prerequisite, ambiguous input, slow or partial loading, and a
  client with no backend. Run it through the real runner (`tap --client <agent>`) on each
  backend you have, keep fixture results apart from live results, then
  `tap discover validate`. Prove one small read through the installed
  `tap_run` path before scaling up.

## Make it fast, then keep improving it

Its speed is decided by how you write it; see the guide's
[Make it fast](https://github.com/Telara-Labs/TAP-Runtime/blob/main/docs/writing-a-primitive.md#make-it-fast)
and
[Measure and improve](https://github.com/Telara-Labs/TAP-Runtime/blob/main/docs/writing-a-primitive.md#measure-and-improve).

- Count round trips: each tool call goes program, runner, agent, tool and
  back, about 0.7 to 1.4 s each. Index once per run and match every input
  against the index, never walk the source once per item. Batch. Loop inside
  the backend when it allows, bounded per call (backends differ; one caps an
  evaluation at about 3 s). No fixed waits beyond what loading needs. Declare a
  budget and report calls and time in the result.
- Measure every validation case: tool calls, duration, unresolved items.
  Compare versions on identical inputs.
- After real use, read the `[stats: ...]` line of the `tap_run` result and the
  run record (`tap_evidence`). When calls, duration or unresolved items can
  drop, publish an improved version (new semantic version, changelog entry).
  Never trade correctness for speed: unresolved stays unresolved.

## Revising and building

Every authored package needs `CHANGELOG.md` with a nonempty `## <version>`
entry matching `metadata.version` in `primitive.yaml`. After changing code,
dependencies, the interface, permissions or other package files, advance the
semantic version, update the manifest and explain the change in the changelog.
Saving different bytes under an existing version or regressing a version is
refused. Identical saves are allowed; prior saved versions remain available.

For a compiled `.wasm` package, include the source and declare
`provenance.source`, `provenance.toolchain` and `provenance.build`. Review the
build command, then run `tap discover build --approve-build <dir>` explicitly
with the author's local toolchain. It builds twice from stable inputs and
writes the executable and `BUILD.json`. Rebuild after changing source,
dependencies, the manifest or changelog. A stale receipt blocks saving.
Save and execution do not run build commands. Local build recipes have the
author's OS access; publish untrusted packages through isolated verification.
Keep test cases and validation receipts outside the package to avoid changing
its inputs after validation.

## Checking and saving it

A saved primitive records where it came from. Start from the earlier request
that `tap_search`'s note names:

```
tap discover brief --task <client/session/request> --out .tap/drafts/<name>-brief
```

Run every command below from the workspace, with the package as
`.tap/drafts/<name>`. Saving copies the package into the TAP collection;
TAP writes the collection, so you never write outside the workspace.

If `tap` is not on your shell's PATH, use the program path the note gives in
its place, here and in every command below.

`BRIEF.md` in that folder shows the earlier request and the calls that
answered it, and says what to establish. Write the package from it, then put
`AUTHORING.json` in the package as `BRIEF.md` step 3 describes, with
`"selection": "selected_task"`, the `sources` ref and `brief_digest` the
brief printed, and every contract field with what established it (the
earlier calls, this session's calls, or the person's words). Do not invent a
value nothing established; ask the person.

```
tap manifest check <dir>                        # may it run?
tap --approve <dir> '{"project": "...", ...}'   # run it once from the shell
tap discover save <dir>                         # or the tap_save tool, after the person agrees
```

To save, prefer the `tap_save` tool with the package's absolute path. The
host handles its confirmation under the person's permission settings, and
it works where your shell cannot write outside the workspace. An accepted
confirmation needs no additional checkbox or special wording from the person.
If the client declines, cancels, or cannot complete the confirmation, report
that exact reason; do not describe a client cancellation as the person refusing.
Then try the saved primitive with `tap_run`. Saving does not approve its effects.

When the procedure can run on local fixtures (files, a git repository),
validate it first as `BRIEF.md` steps 4 and 5 describe and save with
`--receipts`. Fixtures can check a live-service primitive's parser, but cannot
establish its live identity, navigation or completeness contract. Live-service
validation remains `validation: not_run` through this fixture workflow. Keep
parser receipts and live-run evidence separate and say what each establishes.

Run from Claude Code's or Codex's shell, the draft borrows that agent's own
tool connections; `--approve` lets it reach its declared origins and make its
declared changes, so only use it on a draft that reads. Compare its output
with the answer you already gave, and fix the program until they agree. Then
save it, run it once with `tap_run` (find its ref and digest with
`tap_search`), and tell the person its reference (`local.me/<name>@<version>`)
and what it takes.

Saving puts the package in the shared TAP collection and writes a pointer
skill for each connected agent, so a later request of the same kind, in a new
session, finds it through `tap_search` without anyone naming it.
