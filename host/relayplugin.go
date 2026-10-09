package main

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

// relayPlugin is the TAP relay for OpenCode and Kilo: a plugin that lets the
// runner a session starts reach that session's own MCP connections
// (bridge/sessionrelay.go).
//
//go:embed relayplugin/tap-relay.js
var relayPlugin []byte

// relayPluginName is the file both clients load from their plugin folder.
const relayPluginName = "tap-relay.js"

// setRelayPlugin writes the relay into the plugin folder beside a client's
// configuration file, or removes it. It reports whether anything changed.
func setRelayPlugin(configFile string, remove bool) (bool, error) {
	path := filepath.Join(filepath.Dir(configFile), "plugin", relayPluginName)
	old, err := os.ReadFile(path)
	if remove {
		if err != nil {
			return false, nil
		}
		return true, os.Remove(path)
	}
	if err == nil && bytes.Equal(old, relayPlugin) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, relayPlugin, 0o644)
}
