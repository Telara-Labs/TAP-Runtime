# TAP Local

Install TAP globally on macOS, Linux, or Windows:

```sh
npm install --global @telaralabs/tap
tap setup        # connect TAP to the agents installed here
tap discover     # find the work you keep asking your agents to do
```

The package contains the signed TAP runner release for each supported platform, and checks the runner's sha256 before every run. `tap setup` connects TAP, as the MCP server `tap`, to every supported agent installed here (`tap install --client detected`) and says what it connected; restart the agent afterwards. npm's install step runs the same setup, but npm hides its output and may skip install scripts. No Telara account is needed.

`tap --help` lists every command.

For a project-local npm dependency, install the package and opt into user-level client registration explicitly:

```sh
npm install @telaralabs/tap
npm exec -- tap setup
```

Setup connects every supported agent it finds installed and names each one; `tap install --client all --print` shows what it would change without changing anything. VS Code uses the separate TAP extension. To connect one agent later: `tap install --client <agent>`.
