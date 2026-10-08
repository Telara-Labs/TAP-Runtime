//go:build windows

package main

import "errors"

// replace is not possible on Windows, where tap serve keeps its own relay
// and never starts this program.
func replace(program string, argv []string) error {
	return errors.New("a process cannot replace its program on Windows")
}
