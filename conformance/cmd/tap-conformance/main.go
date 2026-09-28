// Command tap-conformance puts a TAP runner through the conformance lanes.
//
//	tap-conformance -- <command that starts the runner as an MCP server>
//
// It exits 0 when every lane passes.
package main

import (
	"fmt"
	"os"

	"gitlab.com/telara-labs/tap-runtime/conformance"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tap-conformance -- <runner command> [args...]")
		os.Exit(2)
	}
	lanes := conformance.Run(args, os.Stdout)
	failed := 0
	for _, l := range lanes {
		if !l.Passed {
			failed++
		}
	}
	fmt.Printf("\n%d lanes, %d passed, %d failed\n", len(lanes), len(lanes)-failed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
