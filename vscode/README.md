# TAP Runtime for VS Code

Lets GitHub Copilot in VS Code run TAP primitives with the tools the editor
already has: every MCP server you connected, and the editor's own tools.

A primitive is a small program that does one job with those tools. Copilot
calls it as one tool, `tap_run`; the program makes its calls through VS Code,
and only its short answer goes back to the model.

- Needs the `tap-runtime` program: see the install guide in the TAP-Runtime
  repository. The extension looks in `~/.local/bin`, then on PATH, or at the
  `tapRuntime.path` setting.
- The extension registers the runner as the MCP server "TAP Runtime"; there is
  nothing to add to `mcp.json`.
- Each tool call is made by VS Code, with its own connection and its own
  confirmation.

macOS and Linux. On Windows VS Code's extensions speak named pipes, which the
runner does not reach yet.

Commands: **TAP: Run a primitive...** runs one outside chat, and **TAP: Show
the tools a primitive can use** lists what VS Code can call.
