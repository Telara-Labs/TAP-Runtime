module gitlab.com/telara-labs/tap-runtime/discover

// Kept small and on the same older Go as contract: telara-cli imports this
// module for `telara tap discover`, and must not inherit the runner's
// dependencies by doing so. It reads local files only; see
// TestPackageCannotReachTheNetwork.
go 1.25.0

require (
	gitlab.com/telara-labs/tap-runtime/contract v0.0.0-20260929043506-c37e9b089cfa
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1 // indirect
	golang.org/x/net v0.52.0 // indirect
)
