# Browser smoke

Opens the local fixture in a real Playwright browser and checks its
accessibility snapshot for the expected heading. Start the site and Playwright
server using [`../browser-support/README.md`](../browser-support/README.md).

```sh
tap --mcp-url http://127.0.0.1:8931/mcp --mcp-server-name Playwright --approve --limit 1 examples/browser-smoke '{"base_url":"http://127.0.0.1:4173"}'
```

Input validation rejects non-local hosts, paths, credentials, queries, missing
ports, extra fields, and malformed JSON. Navigation requires TAP's write
approval; the page snapshot is read-only.
