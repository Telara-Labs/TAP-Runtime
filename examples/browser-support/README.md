# Local Playwright fixture

These browser examples use the Microsoft Playwright MCP server against a
disposable static site. The fixture is deliberately local: primitives reject
URLs outside `localhost` and `127.0.0.1`, and the form stores nothing.

From the TAP-Runtime repository root, serve the fixture in one terminal:

```sh
python3 -m http.server 4173 --bind 127.0.0.1 --directory examples/browser-support/site
```

In another terminal, start Playwright MCP with its Streamable HTTP endpoint:

```sh
npx --yes @playwright/mcp@latest --port 8931 --headless --isolated
```

Run the primitives from the repository root. `--approve --limit N` is only
for this isolated fixture; an interactive MCP client can show TAP's approval
prompt instead.

```sh
tap --mcp-url http://localhost:8931/mcp --mcp-server-name Playwright --approve --limit 1 examples/browser-smoke '{"base_url":"http://127.0.0.1:4173"}'
tap --mcp-url http://localhost:8931/mcp --mcp-server-name Playwright --approve --limit 2 examples/browser-links '{"base_url":"http://127.0.0.1:4173"}'
tap --mcp-url http://localhost:8931/mcp --mcp-server-name Playwright --approve --limit 4 examples/browser-form-review '{"base_url":"http://127.0.0.1:4173","project":"tap","summary":"review the local demo form"}'
tap --mcp-url http://localhost:8931/mcp --mcp-server-name Playwright --approve --limit 5 examples/browser-keyboard-check '{"base_url":"http://127.0.0.1:4173"}'
```

The direct bridge requires Playwright MCP's HTTP transport; a stdio-only
connection is insufficient. Each primitive pins `Playwright` and exact current
`browser_*` tool names. Navigation, clicks, typing, and key presses are
declared `write`; snapshots are `read`. The fixture has no external links,
credentials, server-side form handler, or persistent state. The keyboard page
reports key events through an accessible output. These examples use a
disposable Playwright context; they do not control the user's open browser.

In the current Playwright MCP schema, navigation, clicks, typing, and key
presses have `readOnlyHint: false`, so TAP resolves them to effective
`destructive` actions and asks for approval even though the manifests request
`write`. Snapshots remain read-only. The commands above use `--approve` with a
per-primitive `--limit` for this explicitly local fixture only; without
approval TAP refuses before dispatch.

`site/counter.html` has a heading, a button and a count kept only in the page.
Client-agnostic browser primitives use it to check that page JavaScript can
read a page and press a control on each browser backend a client lends
(Claude in Chrome, Playwright MCP or Codex's browser). Claude in Chrome asks
for permission per origin, port included; open the fixture as
`http://localhost:4173`, so one permission covers every run.

Run input-validation tests without a browser server:

```sh
python3 -m unittest examples/browser-support/test_browser_examples.py
```

Local review recordings of the complete smoke → links → form → keyboard
flow are kept under `evidence/`. They were recorded against this local
fixture with no action callouts. Generated WebM files are ignored and are
not included in this repository's example packages.
