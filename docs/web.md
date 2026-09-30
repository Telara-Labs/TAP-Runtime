# Running primitives in claude.ai

**Preview.** A primitive can run inside claude.ai, in the browser, using the
connectors your claude.ai account already has. Nothing is installed and no
credentials are handled: claude.ai makes each connector call for the page, as
you.

## How it works

`tap web build` writes one web page, the worker. You publish it as a claude.ai
Artifact and keep it open in a tab while you chat.

1. Your chat adds a job to the worker's job list: a primitive's name and its
   arguments.
2. The worker runs the primitive in a sandbox in the page. Each tool the
   program calls is answered by your own connector.
3. The worker writes what the program printed onto the job. Your chat reads
   it. The data the primitive read never enters the chat.

Measured on claude.ai with a Gmail primitive: 37 KB read from Gmail, 170 bytes
returned to the chat, about five seconds end to end.

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

- The worker must be open in a tab. It keeps working in a background tab.
- claude.ai paid plans, where Artifacts can use connectors.
- One worker page serves the primitives it was built with. Build it again to
  add one.
- ChatGPT has a similar feature (Sites with plugins) on Business, Enterprise
  and Edu workspaces. It has not been tested. Gemini and Copilot on the web
  offer nothing comparable.
