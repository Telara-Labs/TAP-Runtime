// Command discover runs `tap discover` from this module: it reads this
// machine's agent history and ends in the primitive menu.
package main

import (
	"os"

	"github.com/Telara-Labs/TAP-Runtime/discover"
)

func main() {
	os.Exit(discover.Command(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
