# TAP Local

Install TAP globally on macOS, Linux, or Windows:

```sh
npm install --global @telaralabs/tap
```

The package contains the signed TAP runner release for each supported platform. When npm permits the package's install script, a global install verifies the runner and registers TAP with installed Claude Code and Codex clients. npm 12 and later may require explicitly allowing the package script:

```sh
npm install --global --allow-scripts=@telaralabs/tap @telaralabs/tap
```

If install scripts are disabled, run `tap setup` after installation to verify the runner and connect it to every agent installed here (`tap install --client detected`). Restart the agent, then call the TAP MCP tools. No Telara account is needed.

The same `tap` command exposes the runner CLI, including primitive discovery:

```sh
tap discover --review
tap --help
```

For a project-local npm dependency, install the package and opt into user-level client registration explicitly:

```sh
npm install @telaralabs/tap
npm exec -- tap setup
```

Setup connects Claude Code, Codex, Copilot CLI, Cursor, Windsurf and Gemini CLI (experimental) when they are installed. VS Code uses the separate TAP extension. To connect one agent later: `tap install --client <agent>`.
