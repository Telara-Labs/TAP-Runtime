package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ConfiguredServer is one MCP server a client is configured with, as the
// client's own configuration names it. The runner reads it to find which
// servers may fill a capability on a client that does not list its tools.
// Nothing here is a credential: headers, environment and tokens are left
// where they are.
type ConfiguredServer struct {
	Name    string
	Type    string // local, remote, stdio, http, as the client spells it
	Command string // the command and its arguments, joined by spaces
	URL     string
	// Connected is the client's own report that the server is connected.
	// StatusKnown is false when the client reports nothing about it, as
	// Gemini CLI does: a configured server is then all that can be said.
	Connected   bool
	StatusKnown bool
}

// Configured is a bridge that can say which MCP servers its client is
// configured with, without listing their tools.
type Configured interface {
	ConfiguredServers() ([]ConfiguredServer, error)
}

// ConfiguredServers reads the servers from Kilo's own resolved
// configuration (GET /config, which merges the user's and the project's
// files) and whether each is connected (GET /mcp). Nothing is changed.
func (k *Kilo) ConfiguredServers() ([]ConfiguredServer, error) {
	b, err := k.get("/config")
	if err != nil {
		return nil, err
	}
	var cfg struct {
		MCP map[string]struct {
			Type    string          `json:"type"`
			Command json.RawMessage `json:"command"`
			URL     string          `json:"url"`
			Enabled *bool           `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("kilo /config: %w", err)
	}
	st, err := k.get("/mcp")
	if err != nil {
		return nil, err
	}
	var status map[string]struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(st, &status); err != nil {
		return nil, fmt.Errorf("kilo /mcp: %w", err)
	}
	var out []ConfiguredServer
	for name, s := range cfg.MCP {
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		out = append(out, ConfiguredServer{Name: name, Type: s.Type, Command: commandText(s.Command), URL: s.URL,
			Connected: status[name].Status == "connected", StatusKnown: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// commandText joins a command given as a list or as one string.
func commandText(raw json.RawMessage) string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, " ")
	}
	var s string
	json.Unmarshal(raw, &s)
	return s
}

// GeminiServers reads the MCP servers from Gemini CLI's settings files, the
// user's and the project's, a later file overriding an earlier one by name.
// Gemini reports no connection status, so StatusKnown is false.
func GeminiServers(files ...string) []ConfiguredServer {
	byName := map[string]ConfiguredServer{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		servers, _ := parseJSONObject(b)["mcpServers"].(map[string]any)
		for name, v := range servers {
			sm, _ := v.(map[string]any)
			s := ConfiguredServer{Name: name}
			s.Command, _ = sm["command"].(string)
			if args, ok := sm["args"].([]any); ok {
				for _, a := range args {
					if as, ok := a.(string); ok {
						s.Command += " " + as
					}
				}
			}
			s.URL, _ = sm["url"].(string)
			if s.URL == "" {
				s.URL, _ = sm["httpUrl"].(string)
			}
			s.Type, _ = sm["type"].(string)
			byName[name] = s
		}
	}
	out := make([]ConfiguredServer, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
