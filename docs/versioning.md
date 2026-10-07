# Review a version before adopting it

TAP has three separate versions: the runner release (`tap version`), a
primitive's `publisher/name@version`, and the manifest format
(`primitives.telara.dev/v3`). A runner upgrade does not mean a primitive
upgrade. A primitive's version label does not prove compatibility.

## Compare primitive versions

`tap diff` is available since runner 0.2.1. From the repository root, build
the runner and compare the included packages:

```sh
go build -o /tmp/tap-version-demo ./host
/tmp/tap-version-demo diff examples/versioning/v1 examples/versioning/v2
```

The report shows exact refs and execution digests, the removed `name` input
and new required `user` input, the renamed `message` output, and changed
entrypoint code. Existing callers need to send `user` and read `greeting`
before adopting v2. These two examples return fixed greetings so the
interface change is easy to inspect.

To see a permission addition:

```sh
/tmp/tap-version-demo diff examples/versioning/v2 examples/versioning/v2-permissions
```

That reports a new `notes/*` write declaration even though the program bytes
are unchanged. The example declares the permission for inspection; it does
not write a file.

For automation, put `--json` before the directories. Exit codes are **0** for
identical execution digests, **1** for review required, and **2** for invalid
packages or usage. A pipeline can stop on either nonzero status. A changed
package never receives an automatic "compatible" verdict. Schema edits are
potentially breaking, including changes the comparator cannot prove safe.
Every changed manifest field has its exact before/after values; entrypoint
code is shown by its hash so arbitrary program contents are not printed.

The report flags changed bytes under the same version. Publish a new
version, retain the old one, and review the new digest. A patch or minor
label does not exempt a package from review.

## What the checks cover

`tap_search` and `tap_load` identify an exact ref and digest. Use both with
`tap_run`; the runner checks the digest again before starting. Different
versions can coexist. A stale digest cannot run changed manifest/entrypoint
bytes. Package trust is scoped to those bytes; it does not authorize a new
write or fetch origin. See [headless trust](headless-and-sharing.md).

`tap diff` compares **primitive.yaml and the entrypoint**, the same bytes
covered by the execution digest. It never executes a package or changes
trust. It shows declared authority additions and all declaration edits;
the authority summary does not establish that other edits are safe.
It cannot establish behavioral compatibility, inspect imported dependencies,
validate the actual output, or predict upstream MCP/API changes. Review the
source diff, run your caller's acceptance tests, and retain the previous
ref/digest for rollback. Compare files that are not being edited concurrently.

## Review runner upgrades

Read [the runtime migration history](../CHANGELOG.md) and the linked tag
comparison for the releases you are moving between. Versions before 1.0
can change behavior. In particular, callers must handle the long-run
handoff introduced in 0.1.27 and present in 0.2.0; input-shape enforcement
changed in 0.1.24. Restart clients after replacing the runner so they refresh
their tool list. Always run an acceptance test with your intended runner
and primitive, rather than inferring compatibility from their labels.
