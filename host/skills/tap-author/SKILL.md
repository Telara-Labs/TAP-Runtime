---
name: tap-author
description: "Turn a multi-step procedure the person keeps asking for into a saved TAP primitive, so the next request runs it as code with new inputs. Use after finishing such a task when tap_search found nothing and the same kind of request has come before or the person says it recurs. Not for one-off requests."
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

Make a folder outside the person's project, such as
`~/tap-drafts/<name>/`, with `primitive.yaml` and one program (`main.py`,
`main.js`, `main.ts` or `main.sh`). The guide, with every field, is
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
- Declare only what the program uses. Reads are `effect: read`; anything that
  changes something is `write` or stronger and is asked of the person.
- In Python, `tap.call(alias, args)` calls a declared tool and
  `tap.fetch(url)` returns `{"status", "body"}`.
- Print one JSON object: the verdict, what passed, what is missing, and links
  to the evidence. When a read comes back partial (a page limit, a count that
  does not match), report it as incomplete. Never count incomplete evidence
  as a pass.

## Checking and saving it

```
tap manifest check <dir>                        # may it run?
tap --approve <dir> '{"project": "...", ...}'   # run it once from the shell
tap discover save <dir>                         # after the person agrees
```

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
