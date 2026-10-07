# Browser form review

Fills a project and summary into a disposable local form, submits it, and
checks the rendered confirmation. The fixture only updates an on-page output;
it has no backend or persistence. Start the fixture and Playwright MCP using
[`../browser-support/README.md`](../browser-support/README.md).

```sh
tap --mcp-url http://127.0.0.1:8931/mcp --mcp-server-name Playwright --approve --limit 4 examples/browser-form-review '{"base_url":"http://127.0.0.1:4173","project":"tap","summary":"review the local demo form"}'
```

Input validation restricts the target to a local origin and bounds both text
fields. Navigation, typing, and submit are write-gated. This does not submit to
a real service.
