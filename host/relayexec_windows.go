//go:build windows

package main

import "errors"

// becomeRelay is not possible on Windows, where a process cannot replace its
// program: tap serve relays the session itself.
func becomeRelay(args []string) error {
	return errors.New("a process cannot replace its program on Windows")
}
