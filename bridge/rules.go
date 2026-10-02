package bridge

import (
	"encoding/json"
	"os"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

// Asker is implemented by a bridge that can read the person's own "ask me
// first" rule for a tool. The runner then puts a call to that tool in front of
// them even when the primitive declares it a read (TENG-3101).
type Asker interface {
	Asks(t bind.Tool) (bool, error)
}

// codexServerRules is what a Codex config says about one MCP server's tools.
// Codex keeps it per server under mcp_servers (read through config/read):
// enabled, enabled_tools (an allow list), disabled_tools (a deny list), and
// tools.<name>.approval_mode, where "prompt" asks every time.
type codexServerRules struct {
	disabled      bool
	allow         map[string]bool // nil when the config has no allow list
	deny          map[string]bool
	alwaysPrompts map[string]bool
}

func (r codexServerRules) denies(tool string) bool {
	if r.disabled || r.deny[tool] {
		return true
	}
	return r.allow != nil && !r.allow[tool]
}

// codexRulesFrom reads the mcp_servers table of a config/read answer.
func codexRulesFrom(config map[string]any) map[string]codexServerRules {
	out := map[string]codexServerRules{}
	servers, _ := config["mcp_servers"].(map[string]any)
	for name, v := range servers {
		sm, _ := v.(map[string]any)
		r := codexServerRules{deny: map[string]bool{}, alwaysPrompts: map[string]bool{}}
		if en, ok := sm["enabled"].(bool); ok && !en {
			r.disabled = true
		}
		if list, ok := sm["enabled_tools"].([]any); ok {
			r.allow = map[string]bool{}
			for _, t := range list {
				if s, ok := t.(string); ok {
					r.allow[s] = true
				}
			}
		}
		if list, ok := sm["disabled_tools"].([]any); ok {
			for _, t := range list {
				if s, ok := t.(string); ok {
					r.deny[s] = true
				}
			}
		}
		tools, _ := sm["tools"].(map[string]any)
		for tn, tv := range tools {
			tm, _ := tv.(map[string]any)
			if tm["approval_mode"] == "prompt" {
				r.alwaysPrompts[tn] = true
			}
		}
		out[name] = r
	}
	return out
}

// geminiRulesFrom reads mcpServers from a Gemini CLI settings.json: includeTools
// is an allow list and excludeTools a deny list, per server.
func geminiRulesFrom(settings map[string]any) map[string]codexServerRules {
	out := map[string]codexServerRules{}
	servers, _ := settings["mcpServers"].(map[string]any)
	for name, v := range servers {
		sm, _ := v.(map[string]any)
		r := codexServerRules{deny: map[string]bool{}, alwaysPrompts: map[string]bool{}}
		if list, ok := sm["includeTools"].([]any); ok {
			r.allow = map[string]bool{}
			for _, t := range list {
				if s, ok := t.(string); ok {
					r.allow[s] = true
				}
			}
		}
		if list, ok := sm["excludeTools"].([]any); ok {
			for _, t := range list {
				if s, ok := t.(string); ok {
					r.deny[s] = true
				}
			}
		}
		out[name] = r
	}
	return out
}

// GeminiRules reads the tool rules of Gemini CLI from the user's settings and
// from the project's, the project's adding to the user's.
func GeminiRules(files ...string) func(bind.Tool) (denied bool) {
	merged := map[string]codexServerRules{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		settings := parseJSONObject(b)
		for name, r := range geminiRulesFrom(settings) {
			m, ok := merged[name]
			if !ok {
				merged[name] = r
				continue
			}
			for k := range r.deny {
				m.deny[k] = true
			}
			if r.allow != nil {
				if m.allow == nil {
					m.allow = r.allow
				} else {
					for k := range m.allow {
						if !r.allow[k] {
							delete(m.allow, k)
						}
					}
				}
			}
			merged[name] = m
		}
	}
	return func(t bind.Tool) bool {
		r, ok := merged[t.Server]
		return ok && r.denies(t.Name)
	}
}

func parseJSONObject(b []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return map[string]any{}
	}
	return m
}
