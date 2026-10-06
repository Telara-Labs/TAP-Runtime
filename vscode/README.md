# TAP Runtime for VS Code

Lets GitHub Copilot in VS Code run TAP primitives with the tools the editor
exposes through its public tool API. VS Code 1.140's Local chat harness exposed
real connected MCP tools in the tested fixture; the newer Copilot SDK harness
omitted MCP tools from `lm.tools`. Connected-server visibility therefore depends
on the harness and API, rather than merely being configured in the editor.

A primitive is a small program that does one job with those tools. Copilot
calls it as one tool, `tap_run`; the program makes its calls through VS Code,
and the program's output goes back to the model. Intermediate data stays out
of the conversation only when the program does not print it.

- Needs the `tap` program (the TAP runner): see the install guide in the TAP-Runtime
  repository. The extension looks in `~/.local/bin`, then on PATH, or at the
  `tapRuntime.path` setting.
- The extension registers the runner as the MCP server "TAP Runtime"; there is
  nothing to add to `mcp.json`.
- Tool calls use VS Code's connections. The TAP runner obtains required
  approval itself; extension-dispatched calls cannot rely on VS Code's native
  confirmation.

macOS and Linux preview. On Windows VS Code's extensions speak named pipes, which the
runner does not reach yet.

Download `tap-vscode-X.Y.Z.vsix` from a release that includes it on the
[GitHub releases page](https://github.com/Telara-Labs/TAP-Runtime/releases).
Verify its entry in the release's signed `SHA256SUMS` with the runner's release
verification procedure, then install the downloaded file:

```sh
code --install-extension ./tap-vscode-X.Y.Z.vsix
```

Replace `X.Y.Z` with that release's version. Install the matching TAP runner
separately; the VSIX does not bundle it. This delivery path uses a release asset
and does not require a Marketplace listing.

Commands: **TAP: Run a primitive...** runs one outside chat, and **TAP: Show
the tools a primitive can use** lists what VS Code can call.
