# Installing the runner

## What the machine needs

- **Claude Code or Codex.** They are the two clients that let a program call
  the tools they have connected. The runner also runs from a terminal with no
  client, for primitives that use no tools.
- **The connections a primitive uses**, already connected in that client (for
  example Gmail), and any host program it declares (`git`, `kubectl`).
- **Network on first use**, to download interpreters. `tap-runtime fetch`
  downloads them all ahead of time.

No Telara account, registry or gateway is needed.

## One instruction

macOS and Linux:

```
curl -fsSL <release>/install.sh | sh
```

Windows (PowerShell):

```
irm <release>/install.ps1 | iex
```

`<release>` is the address of a release's files, such as
`https://github.com/OWNER/REPO/releases/download/v0.1.0`.

The script downloads the runner for this machine and checks it against a
sha256 written into the script when the release was built. If it doesn't
match, the download is deleted and nothing is installed. It then registers
the runner as an MCP server named `tap` with each of Claude Code and Codex
that is installed:

```
claude mcp add --scope user tap -- <path>/tap-runtime serve
codex mcp add tap -- <path>/tap-runtime serve
```

Options: `--client claude|codex|none` and `--dir DIR` (default
`~/.local/bin`). Windows takes `-Client` and `-Dir`, and installs to
`%LOCALAPPDATA%\Programs\tap-runtime`.

To register an already-downloaded runner by hand:

```
tap-runtime install --client claude          # --scope local|user|project
tap-runtime install --client codex
tap-runtime install --client claude --print  # show the command, change nothing
```

## Checking a release yourself

Each release holds `SHA256SUMS`, its signature `SHA256SUMS.sig`, and the
public key that made it, `SHA256SUMS.pub`. The key in the release shows which
key signed it. It proves nothing on its own: check against a copy of the key
from somewhere you already trust.

```
go run ./release verify --dir <downloaded release> --pub <trusted key file>
```

## Codex

- In `codex exec`, Codex cancels an MCP tool call that needs approval. It
  reports `user cancelled MCP tool call`. Allow the tool once:

  ```
  codex exec -c 'mcp_servers.tap.tools.tap_run.approval_mode="approve"' "..."
  ```

  An interactive Codex session asks instead.
- A runner that Codex starts as an MCP server can download interpreters,
  measured in Codex's `read-only` sandbox. A runner started from Codex's
  **shell** has no network, and so can't download one. Run primitives
  through the `tap_run` tool, or run `tap-runtime fetch` once in your own
  terminal first.

## Building from source

```
export GOWORK=off
go build -o tap-runtime ./host
GOOS=wasip1 GOARCH=wasm go build -o sh.wasm ./guest-sh
```

A runner built from source doesn't know where a release's bash interpreter
is, so it can't download one. Put `sh.wasm` in its interpreter store
(`tap-runtime fetch` prints where that is), or pass `--interpreters DIR`.

`go install <module>/host@<version>` works only while the module is served
from the path it declares, `gitlab.com/telara-labs/tap-runtime`. Served from
any other address, such as a GitHub repository, Go refuses it: the module
declares one path and was required as another. The installed program is
named `host`.

## Tested on

| Platform | State |
|---|---|
| macOS arm64 | the full suite, the install script, and primitives through Claude Code and Codex |
| Linux amd64, arm64 | the full suite, in CI |
| Windows amd64 | a subset, and one primitive, under Wine in CI. Never on Windows itself. `install.ps1` has never been run |
| macOS amd64 (Intel) | built. Never run |
