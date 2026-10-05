# Headless runs and output sharing

These workflows describe current source. The fetch grant and pointer repair
need a release containing the October 5 remediation; v0.1.4 and v0.1.10 do
not contain it.

## Approve a package before a headless run

Review its manifest and entrypoint, then run the CLI yourself:

```sh
tap trust PACKAGE-DIR
tap trust --list
```

The grant names the manifest and entrypoint digest. Relocating the package or
updating a SKILL.md does not change it. Editing either executable input
requires a new grant. `tap trust --forget DIGEST-PREFIX` removes the trust
record and any fetch grants. Trust alone does not approve file writes, host
commands, effectful tools or network requests.

For a fetch, explicitly name each origin you approve:

```sh
tap trust --fetch-origin https://example.com PACKAGE-DIR
```

Only that exact origin, this digest, and methods declared by the manifest
are permitted. A wildcard declaration can be granted one exact host; the
CLI grant itself cannot contain a wildcard, credentials, path or query.
Requests can send URL parameters, headers and bodies to the approved origin.
The grant is listed by `--list`; a plain `tap trust PACKAGE-DIR` replaces it
with package trust only. Runtime bounds and network address checks still
apply, and requests and approvals are recorded.
Cross-origin redirects are refused even when both origins are declared;
fetch the second origin separately so its approval can be checked.

Codex exec can cancel elicitation even when it advertises support. Configure
its outer MCP permission separately; an outer allow-list does not approve
the runner's effects. The model cannot pass a fetch grant to `tap_run`.
Effectful tool, command and file approvals still need an interactive client,
or a reviewed CLI invocation with its explicit `--approve`/`--limit` flags.

When two connections offer the same capability, pick one before the run:

```sh
tap bind --client codex gmail.threads.search "connection-name"
tap bind --list
```

Use the actual connection name from your client. This is a persistent binding,
not approval to perform writes through it.

## Refresh a stale saved pointer

An old digest never executes new package bytes. Inspect the package currently
installed under its ref with `tap_load`, then run:

```sh
tap discover migrate-saved
```

This also refreshes TAP-owned SKILL.md pointers when the collection has the
same exact ref. It skips foreign skills, a different package version, and
pointer folders with additional files. Saving a chosen new version updates
its pointers as part of the normal save workflow.

## Share an output deliberately

Primitive outputs are created with mode 0600 and new directories with 0700.
Another OS user cannot read them; this protects tool results and local data.
Run CI producer and consumer steps under the same user where possible.

For a deliberate handoff, let the owning user copy only the reviewed output
into a separately managed shared directory, for example on Unix:

```sh
install -m 0640 output.json /path/to/shared/output.json
```

Set the destination's group and directory traversal permissions for the
intended reader using your existing CI or OS policy. The copy does not
change private run records, interpreter caches or future primitive outputs.
Windows sharing uses its ACLs; Unix mode tests do not establish Windows
access behavior.

## Standard corporate proxy settings

Current source honors `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` through Go's
standard HTTP transport. These are existing conventions, not new TAP
variables. Leave them unset on machines that do not use a proxy. The
published v0.1.4 and v0.1.10 runners bypassed the proxy in the verification;
use a release that includes the source fix before relying on it.

## A stalled Discover reader

SQLite reads have a fixed 30-second deadline in current source. Cursor's
queries share one store-read budget. An expired read reports the affected
store and recommends closing the agent or reading a store snapshot; it
does not present partial history as a successful parity result. This is a
reader deadline, not a bound on the whole mining/evaluation pipeline.
