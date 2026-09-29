# tap-runtime

Runs TAP primitives: small programs, in bash, Python, JavaScript or TypeScript,
that do one job with the tools your AI client already has connected, and
nothing else.

A primitive is a folder with a `primitive.yaml` and one program. The runner
runs the program in a sandbox with no file system, no network and no
processes. Every tool call, command, file and web request goes through the
runner. It is checked against `primitive.yaml`, asked of you before anything
changes, and recorded.

No Telara account is needed.

## Install

macOS and Linux:

```
curl -fsSL https://github.com/Telara-Labs/TAP-Runtime/releases/download/v0.1.0/install.sh | sh
```

Windows (PowerShell):

```
irm https://github.com/Telara-Labs/TAP-Runtime/releases/download/v0.1.0/install.ps1 | iex
```

This installs `tap-runtime` and registers it with Claude Code and Codex, if
they are installed. See [docs/install.md](docs/install.md).

## Try it

```
tap-runtime examples/hello-py
```

Or ask Claude Code or Codex to run a primitive: the runner is its `tap_run`
tool.

## Write one

[docs/writing-a-primitive.md](docs/writing-a-primitive.md), with the
packages in [examples/](examples).

## Platforms

| Platform | State |
|---|---|
| macOS arm64 | tested |
| Linux amd64, arm64 | tested |
| Windows amd64 | built. Tested under Wine only; `install.ps1` has never been run |
| macOS amd64 (Intel) | built. Never run |

## Licence

MIT. Each release includes `THIRD_PARTY_NOTICES.txt`, the licences of
everything compiled into it.
