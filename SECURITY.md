# Security

TAP Runtime runs programs on your machine and makes tool calls with your
agents' connections, so its sandbox, its approval gates and its release
signatures are security boundaries.

## Reporting a vulnerability

Report it privately through GitHub: the repository's **Security** tab,
**Report a vulnerability**. Do not open a public issue. Include what you ran,
what happened, and what you expected the runner to refuse.

## In scope

- A primitive reaching the filesystem, network, environment or a host
  program it did not declare, or escaping the WebAssembly sandbox.
- A `write` or `destructive` effect running without approval, or more times
  than approved.
- A run record that lets a resumed run repeat a change.
- A release or interpreter download accepted without matching its pinned
  signature or sha256.
- `tap discover` sending anything off the machine, or copying a credential
  from session history into a report, fixture or draft.

## Verifying a release

Each release holds `SHA256SUMS`, its signature `SHA256SUMS.sig`, and the
public key; see [docs/install.md](docs/install.md) for checking them.
