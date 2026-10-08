//go:build !windows

package main

import (
	"os"
	"syscall"
)

// replace runs program in place of this one, on the same standard input and
// output.
func replace(program string, argv []string) error {
	return syscall.Exec(program, argv, os.Environ())
}
