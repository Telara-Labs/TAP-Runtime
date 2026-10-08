package main

import (
	"fmt"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// unlendableTools says why a primitive cannot run on a client that cannot
// lend its connections, or lends only pinned ones, and how to fix the
// package; empty when it can run. On OpenCode an agent declared git as a
// tool (capability git.shell), saved it, and every later run was refused:
// the program was a host program, which belongs under commands, and the save
// gave no sign it would never run there. On Kilo an agent declared an
// unpinned "shell" tool the same way; Kilo does not list its tools, so only
// a pinned tool can bind there.
func unlendableTools(client string, m *mf.Manifest) string {
	if client == "" || client == "unknown" || m == nil {
		return ""
	}
	lends := lendsConnections(client)
	pinsOnly := client == "kilo" || relayClient(client)
	var cannot []string
	for _, t := range m.Tools {
		if t.Optional || (lends && (!pinsOnly || t.Pin != nil)) {
			continue
		}
		cannot = append(cannot, fmt.Sprintf("%s (%s)", t.Alias, t.Capability))
	}
	if len(cannot) == 0 {
		return ""
	}
	why := client + " cannot lend its connections to a primitive"
	if lends {
		why = client + " does not tell the runner which tools it has, so only a tool pinned as pin: {server: <server>, tool: <tool>} can bind"
	}
	fix := "or mark the tool optional: true"
	if lends {
		fix = "or pin the tool, or mark it optional: true"
	}
	return fmt.Sprintf("%s, so tools: %s can never bind here. Declare a program the primitive runs (git, kubectl) under commands:, and a web read under fetch:, %s", why, strings.Join(cannot, ", "), fix)
}
