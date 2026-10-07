# Public GitHub release brief

Print the latest published release metadata and, when GitHub returns it, a bounded sample of check runs for one ref in a public repository. A check-runs HTTP error produces an explicit partial result with the status and missing evidence; malformed JSON or a malformed successful response fails closed. This is evidence, not a CI or release verdict, and it does not prove that the ref is the release's source commit.

## Run

```sh
tap --approve examples/api-release-brief '{"owner":"Telara-Labs","repo":"TAP-Runtime","ref":"main"}'
```

Review the manifest before granting `--approve`: it authorizes the declared GitHub API origin for this invocation. The standalone CLI refuses the request without this flag; an agent client with approval support can ask interactively. Public unauthenticated API rate limits apply. No GitHub token is used. Check output URLs and the release/ref relationship yourself before acting.

The primitive calls only `GET` endpoints and caps response and output sizes. It samples at most eight check runs and includes API totals where available; healthy responses are labeled `sampled`, never treated as full coverage. If the check-runs endpoint returns an HTTP error, release metadata is still preserved and `checks.state` is `unavailable` with `http_status` and `missing_evidence`. Non-2xx latest-release responses, oversized bodies, invalid JSON, and malformed successful response shapes fail. The interface schema is descriptive; guest code also validates owner/repository/ref values.
