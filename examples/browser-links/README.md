# Browser links

Opens a fixture page, follows its same-origin link in the real browser, and
checks the destination snapshot. Start the fixture and Playwright MCP as shown
in [`../browser-support/README.md`](../browser-support/README.md).

```sh
tap --mcp-url http://127.0.0.1:8931/mcp --mcp-server-name Playwright --approve --limit 2 examples/browser-links '{"base_url":"http://127.0.0.1:4173"}'
```

Only an explicit localhost HTTP origin is accepted. Navigation and clicking
are write-gated; snapshots are read-only. The fixture has no external links.
