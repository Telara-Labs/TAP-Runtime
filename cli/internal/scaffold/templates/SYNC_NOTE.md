These two template trees are manual copies of
`telara-tap/skills/tap-creator/templates/primitive-skeleton-{api,web}/`.

They cannot be a single `go:embed`-shared source: this CLI module
(`telara.dev/tap`) and the tap-creator skill live in separate directories
that are not both under this module's root, and `go:embed` can only embed
files reachable under the embedding package's own module tree (CHANGELOG.md
v1 CLI fix item 5).

**When the skill's templates change, copy the changed files here too** (and
vice versa) -- `tap init` / `tap init --web` and the skill are meant to
produce byte-identical starting points (SKILL.md's claim, made true at G0).
There is no automated check for drift between the two trees; a manual sync
is the accepted cost of the module boundary.
