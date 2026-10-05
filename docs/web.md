# Running primitives in claude.ai

**Preview.** A primitive can run inside claude.ai, in the browser, using the
connectors your claude.ai account already has. The tested path publishes a
worker as an Artifact and makes connector calls through claude.ai's account
connections; no runner installation or manual credential entry was needed for
those checks.

## How it works

`tap web build` writes one web page, the worker. You publish it as a claude.ai
Artifact and keep it open in a tab while you chat.

1. Your chat adds a job to the worker's job list: a primitive's name and its
   arguments.
2. The worker runs the primitive in a sandbox in the page. Each tool the
   program calls is answered by your own connector.
3. The worker writes what the program printed onto the job. Your chat reads
   it. Data enters the chat if the primitive prints it; only intermediate
   results kept out of its output stay outside the conversation.

The connector guard requires `readOnlyHint: true` and refuses a tool with
`destructiveHint: true` or missing read-only metadata. The exact guard was
exercised by signed v0.1.15 compiled worker jobs with Gmail search_threads,
Calendar search_events, Drive search_files and Telara telara_tool_describe.
Rendered rows and terminal database records agreed. These four representative
reads do not establish every connector/tool or a desktop-companion connection.

Published v0.1.15 undercounted failed provider attempts and did not forward the
manifest's request budget to the browser worker. Corrected source `2f707f8`
counts actual dispatch attempts, including provider errors, and applies
`execution.limits.max_dispatches` (default 1000). A private developer Artifact
recorded one failed provider attempt as one call and zero refusals; a batch
with a two-request budget made two calls and refused the third. This correction
was verified in a developer Artifact; the signed v0.1.15 baseline predates
these fixes. Those developer checks do not establish signed-release acceptance.

## Build the page

```
tap web build --out tap-worker.html pkg/recent-mail-web
```

It prints the capabilities the page must be published with, for example:

```json
{
  "db": {},
  "mcp": {"servers": [{"server": "Gmail", "tools": ["search_threads"]}]},
  "user": {}
}
```

An Artifact may call only the connector tools it declares, so the page can
reach exactly what its primitives declare and nothing else.

## Publish it

In Claude Code, signed in with your claude.ai account:

> Publish tap-worker.html as an artifact with these capabilities: (paste the JSON)

The page is private to you. A page that uses connectors cannot be shared by a
public link, so each person publishes their own.

Open the page once and allow the connectors it asks for. After that it starts
by itself whenever it is opened.

## Use it from a chat

With the worker open in a tab, ask Claude in any claude.ai chat:

> In the artifact at <your worker's link>, create the document jobs/a1 with
> {"primitive": "recent-mail-web", "args": ["7"], "state": "queued"}. Then read
> jobs/a1 every 5 seconds until its state is "done" and tell me the result.

## What a primitive needs to run here

| | |
|---|---|
| Language | JavaScript or TypeScript |
| Tools | each one pinned to its connector: `pin: {server: Gmail, tool: search_threads}` |
| Effects | `read` only: the page has no approval step yet |
| Host programs, files, web requests | not available: a web page has no machine |

`tap web build` refuses a primitive that needs anything else, and says why.
`pkg/recent-mail-web` is a working example.

## Limits

- The worker must stay open in a tab. Background-tab execution has not been
  verified.
- An account where Artifacts can use connectors is required.
- The browser worker does not inherit the native runner's 512 MiB memory
  setting, `timeoutSeconds` or 16 MiB protocol-line cap. The request-budget
  correction above does not establish full limit parity.
- One worker page serves the primitives it was built with. Build it again to
  add one.
- Other web clients have not been verified.
