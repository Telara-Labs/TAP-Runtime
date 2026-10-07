package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"gopkg.in/yaml.v3"
)

// An agent that cannot show TAP's prompts (no MCP elicitation: OpenCode,
// Kilo, Crush, Gemini CLI, goose run) gave no way to approve a saved
// primitive, so a primitive that only read gitlab.com was refused until the
// person ran tap trust, and the agent redid its steps by hand. Such an agent
// already lets its own model fetch web pages without asking, as its own
// configuration says, and a primitive that only reads reaches no further
// than that. It runs without a TAP prompt there, and the trust record says
// so. Anything that changes something is still asked, or refused where
// nobody can be asked; host programs are never trusted this way.

// readsOnly says whether a manifest declares nothing but reads.
func readsOnly(m *mf.Manifest) bool {
	if m == nil || len(m.Commands) > 0 {
		return false
	}
	for _, t := range m.Tools {
		if t.Effect != "read" {
			return false
		}
	}
	for _, f := range m.Files {
		if f.Access != "read" {
			return false
		}
	}
	for _, f := range m.Fetch {
		for _, method := range f.Methods {
			if u := strings.ToUpper(method); u != "GET" && u != "HEAD" {
				return false
			}
		}
	}
	return true
}

// readFetchKinds are the gate kinds of a manifest's read fetches.
func readFetchKinds(m *mf.Manifest) []string {
	var kinds []string
	for _, f := range m.Fetch {
		kinds = append(kinds, "send GET requests to "+f.Origin, "send HEAD requests to "+f.Origin)
	}
	return kinds
}

// agentReadsWebUnasked reports whether an agent's own configuration lets its
// model fetch the web without asking the person, and where that was read.
// Unknown agents, and settings that ask or deny, are no.
func agentReadsWebUnasked(clientName, home string) (bool, string) {
	c, ok := historyClient(clientName)
	if !ok {
		return false, "unknown agent " + clientName
	}
	switch c.ID {
	case "opencode", "kilo":
		// permission.webfetch: "allow" (the default), "ask" or "deny".
		file := filepath.Join(home, ".config", c.ID, c.ID+".json")
		var cfg struct {
			Permission json.RawMessage `json:"permission"`
		}
		if b, err := os.ReadFile(file); err == nil {
			json.Unmarshal(b, &cfg)
		}
		var perm map[string]any
		if json.Unmarshal(cfg.Permission, &perm) != nil {
			var all string
			if json.Unmarshal(cfg.Permission, &all) == nil && all != "" && all != "allow" {
				return false, fmt.Sprintf("%s permission is %q", file, all)
			}
			return true, file + " leaves webfetch allowed (its default)"
		}
		if v, ok := perm["webfetch"].(string); ok && v != "allow" {
			return false, fmt.Sprintf("%s permission.webfetch is %q", file, v)
		}
		if perm["webfetch"] == nil {
			return true, file + " leaves webfetch allowed (its default)"
		}
		return true, file + " permission.webfetch is allow"
	case "crush":
		// Crush asks for every tool unless permissions.allowed_tools lists it.
		file := filepath.Join(home, ".config", "crush", "crush.json")
		var cfg struct {
			Permissions struct {
				AllowedTools []string `json:"allowed_tools"`
			} `json:"permissions"`
		}
		if b, err := os.ReadFile(file); err == nil {
			json.Unmarshal(b, &cfg)
		}
		for _, t := range cfg.Permissions.AllowedTools {
			if t == "fetch" || t == "agentic_fetch" {
				return true, file + " allows " + t
			}
		}
		return false, file + " does not allow fetch without asking"
	case "goose":
		// GOOSE_MODE auto runs every tool without asking.
		file := filepath.Join(home, ".config", "goose", "config.yaml")
		var cfg struct {
			Mode string `yaml:"GOOSE_MODE"`
		}
		if b, err := os.ReadFile(file); err == nil {
			yaml.Unmarshal(b, &cfg)
		}
		if cfg.Mode == "auto" {
			return true, file + " GOOSE_MODE is auto"
		}
		return false, fmt.Sprintf("%s GOOSE_MODE is %q", file, cfg.Mode)
	}
	return false, c.Name + " is not known to allow web reads without asking"
}

// autoTrustReads decides whether a package may run without a TAP prompt in
// an agent that cannot show one, and says why.
func autoTrustReads(m *mf.Manifest, clientName, home string) (bool, string) {
	if !readsOnly(m) {
		return false, "it declares more than reads"
	}
	if len(m.Fetch) == 0 {
		return true, "it only reads"
	}
	return agentReadsWebUnasked(clientName, home)
}
