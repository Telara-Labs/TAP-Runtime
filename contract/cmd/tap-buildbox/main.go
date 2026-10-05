// tap-buildbox is the helper of package buildbox: the program that is
// started in new namespaces, replaces its root, and becomes the command.
// It is not run by hand.
package main

import (
	"fmt"
	"os"

	"github.com/Telara-Labs/TAP-Runtime/contract/buildbox"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "--init" {
		fmt.Fprintln(os.Stderr, "tap-buildbox is started by a build box and is not run by hand")
		os.Exit(2)
	}
	if err := buildbox.Init(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "tap-buildbox:", err)
		os.Exit(70)
	}
}
