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
// A client that lends its connections without listing them (Kilo, Gemini CLI)
// has each declared capability resolved when the primitive runs (resolve.go),
// so an unpinned tool is not refused here.
func unlendableTools(client string, m *mf.Manifest) string {
	if client == "" || client == "unknown" || m == nil {
		return ""
	}
	if lendsConnections(client) {
		return ""
	}
	var cannot []string
	for _, t := range m.Tools {
		if t.Optional {
			continue
		}
		cannot = append(cannot, fmt.Sprintf("%s (%s)", t.Alias, t.Capability))
	}
	if len(cannot) == 0 {
		return ""
	}
	return fmt.Sprintf("%s cannot lend its connections to a primitive, so tools: %s can never bind here. Declare a program the primitive runs (git, kubectl) under commands:, and a web read under fetch:, or mark the tool optional: true", client, strings.Join(cannot, ", "))
}
