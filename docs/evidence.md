# Run evidence

After `tap_run`, use its run ID with `tap_evidence`.

```json
{"run_id":"20261006T220000Z-abcd1234","limit":100,"include_manifest":true}
```

The response identifies the package digest executed by the host and the exact
manifest snapshot taken from the same bytes parsed and hashed at run start.
The manifest's digest is SHA256 of its raw bytes. The package digest is SHA256
of raw manifest bytes followed by original entrypoint bytes. Neither is a
signature or remote-system attestation.

Declared permissions come from the saved manifest, including tool effects,
command declarations, file access and fetch origins/methods. They are declared
authority, not evidence that every permission was exercised. The journal's
begin events carry the resolved tool identity assessed for that attempt and
its effective effect. Server annotations are inputs to host classification,
not proof that the provider obeyed them. Nested operations are included when
the host resolves them. Runtime arguments, headers and result payloads are
not returned.

Begin means an attempt was recorded before dispatch. Pair it with the end
event under the same request ID; a refusal may mean no tool was dispatched.
An interrupted write may have happened remotely and remains unknown rather
than being repeated. `ran` does not mean the returned data is correct or that
the provider succeeded. The run's outcome distinguishes failures and unknown
results. This is evidence of what the local host recorded.

The snapshot is private to the run, content-addressed and integrity checked.
Editing or deleting the source package does not change it. Resume keeps the
original snapshot and recorded replies; a read attempted again can have another
begin with the binding used for that attempt. Historical journals missing
snapshots or call identity are never filled from today's package or connections.

Exact YAML is optional because manifests can contain sensitive defaults or
examples. `manifest.content_status` is `included`, `not_requested` or
`omitted_size_limit`. Snapshot availability and declared permissions have
separate status fields. YAML and permissions each have a 16 KiB encoded JSON
budget; the complete response fits within 60 KiB by omitting whole trailing
events and marking `truncated`. Snapshot inspection is capped at 8 MiB and
rejects corrupt snapshots and symlink paths. Journal inspection also has byte
and event limits; a bounded scan may report state `unknown`.

The trace covers journaled requests, not a complete network audit. Requests
without IDs, approval availability queries, no-journal runs and the host's
own resolution probes are not included. Runs are subject to existing local
journal retention, normally 30 days.
