module github.com/Telara-Labs/TAP-Runtime/contract

// Kept small and on an older Go on purpose: Telara's services import this
// module to check what is published, and must not inherit the runner's
// dependencies by doing so.
go 1.25.0

require (
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1
	golang.org/x/net v0.52.0
	gopkg.in/yaml.v3 v3.0.1
)
