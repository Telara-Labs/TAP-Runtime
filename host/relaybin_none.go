//go:build !relayembed || windows

package main

// relayBinary is empty in a build without the small relay (a source build,
// or Windows, where a process cannot replace its program): tap serve relays
// the session itself.
var relayBinary []byte
