# tap-runtime: agent notes

## Releasing (TENG-3157)

A release is one command, run from this directory:

```
GOWORK=off go run ./release publish --version X.Y.Z --plan   # changes nothing
GOWORK=off go run ./release publish --version X.Y.Z
```

In order, it:

1. Commits `npm/package.json` only, as `chore(tap): release vX.Y.Z`.
2. Tags the commit.
3. Builds and signs the release from a clean export of that commit, using
   `~/.tap-release/release.key`, and verifies it.
4. Pushes `main` and the tag to GitLab (`origin`) and GitHub (`github`).
5. Creates the GitHub release.
6. Runs `.github/workflows/release.yml` and waits until npm serves
   `@telaralabs/tap@X.Y.Z`.

It prints one JSON report and exits 1 on failure. Rerunning it resumes from
the failed step.

- Read the `--plan` push steps before releasing. Pushing `main` publishes every
  local commit not yet on that remote, and the plan lists them.
- The next version is one above `npm view @telaralabs/tap version`.
- Never tag, build or upload a release by hand.

## Tests

`GOWORK=off go test ./...` here and in `discover/`. Do not commit binaries:
build into a scratch directory with `-o`.
