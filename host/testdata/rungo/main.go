// rungo is a compiled primitive that runs a child through the Go SDK.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/Telara-Labs/TAP-Runtime/sdk/go/tap"
)

func main() {
	c := tap.New()
	ok, err := c.Run("kid", map[string]any{"print": `{"answer":42}`})
	if err != nil {
		c.Return("", err.Error(), 1)
		return
	}
	_, failed := c.Run("kid", map[string]any{"print": "x", "exit": 3})
	var te *tap.Error
	code := "none"
	if errors.As(failed, &te) {
		code = string(te.Code)
	}
	c.Return(fmt.Sprintf("%s %s\n", ok.Result, code), "", 0)
	_ = os.Args
}
