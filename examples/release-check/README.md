# Release check

A primitive made of two others. It runs

- [`api-release-brief`](../api-release-brief/), which reads a public GitHub
  release and its check runs, and
- [`task-summary-python`](../languages/python/), which reads the open tasks
  from the task file,

and adds one check of its own: a release is `ready` only when its check runs
passed, were all read, and no task is open. The reasons are returned either way.

It declares no fetch origin and no file. Each child runs under its own
manifest, so the release brief reaches `https://api.github.com` and the task
summary reads `examples/languages/fixtures/tasks.json`, and this primitive
could do neither itself. The `uses` entries pin both children by digest.

## Run it

From the repository root, save the two children where `tap` looks for
primitives, then run the parent:

```sh
tap discover save examples/api-release-brief
tap discover save examples/languages/python
tap manifest check examples/release-check
tap --approve examples/release-check "$(cat examples/release-check/sample-input.json)"
```

`--approve` covers the whole tree for this run, including the release brief's
read of GitHub's API; review both manifests first. Without it, the first
request to GitHub is asked about by `api-release-brief`, under the name
`release-check › release_brief`. The task file is found relative to the
directory you run it from.

If you change a child, its digest changes and the pin here no longer matches.
Save the new version and `tap manifest check` says which pin is stale. To
refresh a pin, leave `version`, `digest` and `effect` out of its entry (keep
`alias` and `ref`) and run:

```sh
tap manifest complete --write examples/release-check
```

Completion fills what is left out from the installed primitive and never
replaces what is written. It refuses a ref that matches several installed
primitives and names them.

## What it shows

- `tap.run_many` asks both children at once; `tap.run` asks one.
- A failed child raises, so a missing answer is never read as an empty one.
- The output of this primitive is what it computed, with the evidence its
  children gave.
