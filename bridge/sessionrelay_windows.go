//go:build windows

package bridge

import "os"

// The relay is Unix-only; FindSessionRelay returns before these are used.
func ownedByMe(os.FileInfo) bool { return false }
func processAlive(int) bool      { return false }
