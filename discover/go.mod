module github.com/Telara-Labs/TAP-Runtime/discover

// Kept small and on the same older Go as contract: telara-cli imports this
// module for `telara tap discover`, and must not inherit the runner's
// dependencies by doing so. It reads local files only; see
// TestPackageCannotReachTheNetwork.
go 1.25.0

require (
	github.com/Telara-Labs/TAP-Runtime/contract v0.0.0-20261005135826-0e41a67c9fdd
	golang.org/x/mod v0.35.0
	golang.org/x/term v0.41.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)
