//go:build !linux

package buildbox

import (
	"context"
	"fmt"
	"runtime"
)

// Available reports why a box cannot be made here.
func (b Box) Available() error {
	return fmt.Errorf("a build box needs Linux namespaces; this is %s", runtime.GOOS)
}

// Run refuses.
func (b Box) Run(ctx context.Context, dir, command string) ([]byte, error) {
	return nil, b.Available()
}

// Init is the helper's second stage. It does not exist here.
func Init(args []string) error { return Box{}.Available() }
