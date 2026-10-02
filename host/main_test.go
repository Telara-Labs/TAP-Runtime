package main

import (
	"os"
	"testing"
)

// No test may read or write the real user's configuration: it holds the
// choices a person made (tap bind) and the packages they trust.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tap-config-")
	if err != nil {
		panic(err)
	}
	userConfigDir = func() (string, error) { return dir, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
