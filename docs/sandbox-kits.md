# Docker Sandbox Kits

`tap kit export` writes a [Docker Sandbox Kit](https://github.com/docker/sandbox-kit-spec)
(specification v3) that puts the TAP runner into a sandbox and lets the sandbox
reach exactly the origins your primitives declare under `fetch`. It writes two
files and nothing else: it does not build or push an image.

```sh
tap kit export                        # every installed primitive
tap kit export --out my-kit ./pkg-a ./pkg-b   # only these packages
```

Without package directories it uses the primitives installed in your local
TAP collection (the current copy of each; saved version history is not
included). It writes, into `--out` (default `./tap-kit`):

- `tap.yaml`, a `kind: mixin` descriptor that provides `tap@VERSION`.
- `tap.dockerfile`, the recipe that builds the overlay.

Build it with the sandbox-kit frontend named on the descriptor's first line,
for example `docker buildx build -f tap-kit/tap.yaml tap-kit`.

## What the network policy allows

The descriptor declares one `com.docker.sandbox/network-policy@2` capability.
Its runtime `allow` list is the exact union of the packages' `fetch`
declarations: one entry per host, naming the methods any package declares for
it (a `fetch` entry with no methods means `GET`), sorted, with no wildcard
added. A host two packages both declare appears once, with both packages'
methods. If no package declares a `fetch`, the capability is left out and the
Kit grants no network.

The policy cannot hold every origin a manifest can. The export refuses, naming
the package and the origin, rather than widening it, when an origin:

- uses `http`, because the policy cannot restrict a host to one scheme;
- names a port other than 443, because the policy ignores ports and would
  allow the host on all of them;
- uses a wildcard host such as `https://*.atlassian.net`, because the policy
  only bounds literal hosts to methods.

No credentials are declared. TAP borrows the connections of the agent it runs
inside and holds none of its own.

## What the overlay contains

The runner is the released binary for the Kit's version, downloaded at build
time and checked against the release's `SHA256SUMS` before it is installed at
`/usr/local/bin/tap`. The checksum file comes from the same release as the
binary, so it catches a corrupt download, not a compromised release; verify
the release signature separately if you need that. The binary is static, so
the overlay does not need Node or anything else on the workload.

The interpreters the runner would otherwise download on first use are fetched
during the build and placed in the `agent` user's cache, so the sandbox needs
no network access beyond the primitives' own origins.

The Kit's version is the runner's version, so export from an installed release
of `tap`; a build from source refuses. It covers the primitives you name or
have installed. A primitive that runs others through `uses` needs the
children's origins too: the children are installed primitives, so they are
included when you export without package directories.

## Checking a Kit

The specification's `kit-tck` validates a built image. `go test ./host -run
TestKitConformance` builds an exported Kit with the local Docker daemon and
runs `kit-tck validate` on it when both are present, and skips otherwise.
