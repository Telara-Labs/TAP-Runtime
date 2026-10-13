# Client bridges

TAP's connection to an agent is client-specific. The runner can execute a
primitive's connected tools only when the agent exposes a usable bridge or
relay; registering the TAP MCP server does not establish tool execution or
approval support. A client that cannot show an approval question does not
receive implicit approval for a change.

This page describes the boundary. For dated client tests, source and package
versions, historical platform launch checks, and cases that remain unverified,
see the [TAP 0.2.28 compatibility evidence snapshot](compatibility-0.2.28.md).
That snapshot preserves older research as history and does not claim that its
results were repeated on the current package.

Bridge implementations live beside the runner in `bridge/`; CLI relays and
client-specific setup code live under `host/`. The implementation is the
current source of truth for what code paths exist. Runtime behavior still
depends on the client version, its configuration, and whether it can present
the runner's approval requests.
