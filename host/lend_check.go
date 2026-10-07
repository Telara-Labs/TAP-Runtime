package main

import (
	"fmt"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// unlendableTools says why a primitive cannot run on a client that cannot
// lend its connections, and how to fix the package; empty when it can run.
// On OpenCode an agent declared git as a tool (capability git.shell), saved
// it, and every later run was refused: the program was a host program, which
// belongs under commands, and the save gave no sign it would never run there.
func unlendableTools(client string, m *mf.Manifest) string {
	if client == "" || client == "unknown" || lendsConnections(client) || m == nil {
		return ""
	}
	var required []string
	for _, t := range m.Tools {
		if !t.Optional {
			required = append(required, fmt.Sprintf("%s (%s)", t.Alias, t.Capability))
		}
	}
	if len(required) == 0 {
		return ""
	}
	return fmt.Sprintf("%s cannot lend its connections to a primitive, so tools: %s can never bind here. Declare a program the primitive runs (git, kubectl) under commands:, and a web read under fetch:, or mark the tool optional: true", client, strings.Join(required, ", "))
}
