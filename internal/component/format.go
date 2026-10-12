// Package component runs WebAssembly components (the Component Model, WASI
// Preview 2) on the same wazero engine that runs WASI Preview 1 modules.
//
// wazero runs core modules only. A component is a set of core modules plus
// instructions for wiring them together and to the interfaces it imports.
// This package reads those instructions, instantiates each core module in
// wazero in the order the component gives, and supplies every imported
// interface itself. Nothing reaches the host except through the functions
// in wasi.go: the guest gets standard input and output (the broker's
// protocol), standard error, its arguments, clocks and random bytes. It gets
// no preopened directory, no environment variables and no network: socket
// creation and name lookup are refused, and an import of any interface
// outside the WASI 0.2 packages listed in allowedPackages is refused when the
// component is loaded.
//
// Only the synchronous canonical ABI with UTF-8 strings is supported. A
// component that uses async, threads, its own resource types or a component
// import other than a WASI interface is refused with a message that says so.
package component

import "fmt"

// Format is the kind of WebAssembly binary an entrypoint holds.
type Format string

const (
	// Module is a core WebAssembly module (WASI Preview 1).
	Module Format = "wasip1-module"
	// Component is a WebAssembly component (Component Model, WASI Preview 2).
	Component Format = "wasip2-component"
)

var (
	moduleHeader    = []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}
	componentHeader = []byte{0x00, 'a', 's', 'm', 0x0d, 0x00, 0x01, 0x00}
)

// Detect reads the 8-byte preamble. A core module has version 1 and layer
// 0; a component has version 0x0d and layer 1. Anything else is refused.
func Detect(b []byte) (Format, error) {
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return "", fmt.Errorf("not a WebAssembly binary")
	}
	switch string(b[:8]) {
	case string(moduleHeader):
		return Module, nil
	case string(componentHeader):
		return Component, nil
	}
	if b[6] == 0x01 && b[7] == 0x00 {
		return "", fmt.Errorf("WebAssembly component encoding version %d is not supported; rebuild with a toolchain that emits the Component Model's version 0x0d", b[4])
	}
	return "", fmt.Errorf("unsupported WebAssembly binary version")
}
