# Browser keyboard check

Places focus on the local page heading, then uses Tab and Space to toggle a
checkbox and verifies its accessible output. Start the site and
Playwright MCP using [`../browser-support/README.md`](../browser-support/README.md).

```sh
tap --mcp-url http://localhost:8931/mcp --mcp-server-name Playwright --approve --limit 5 examples/browser-keyboard-check '{"base_url":"http://127.0.0.1:4173"}'
```

The example accepts only an explicit localhost HTTP origin. Navigation,
focus-setting click, and keypresses are write-gated; snapshots are read-only.
It interacts with the fixture, not the user's browser.
