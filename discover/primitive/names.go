package primitive

import (
	"strings"
	"sync"
)

// servers maps an MCP tool label to the server that recorded it, so a
// tool's display name can drop the server's own prefix (telara_jira_x on
// server telara reads "jira x"). Filled from the calls each Discover reads.
var servers = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

func noteServer(tool, server string) {
	if server == "" || !strings.HasPrefix(tool, "mcp:") {
		return
	}
	servers.Lock()
	servers.m[tool] = server
	servers.Unlock()
}

// display is how an operation reads to a person: no client prefixes, words
// not underscores, a shell pipeline as commands joined by |.
func display(op string) string {
	switch {
	case strings.HasPrefix(op, "sh:"):
		parts := strings.Split(op, "+")
		for i, p := range parts {
			parts[i] = strings.TrimPrefix(p, "sh:")
		}
		return strings.Join(parts, " | ")
	case strings.HasPrefix(op, "op:"):
		// A gateway dispatch no direct tool matched: its selecting values.
		vals := strings.Split(strings.TrimPrefix(op, "op:"), ".")
		for i, j := 0, len(vals)-1; i < j; i, j = i+1, j-1 {
			vals[i], vals[j] = vals[j], vals[i]
		}
		return strings.ReplaceAll(strings.Join(vals, " "), "_", " ")
	case strings.HasPrefix(op, "mcp:"):
		base, choice, _ := strings.Cut(op, "#")
		name := strings.TrimPrefix(base, "mcp:")
		servers.Lock()
		srv := servers.m[base]
		servers.Unlock()
		if srv != "" && strings.HasPrefix(name, srv+"_") && len(name) > len(srv)+1 {
			name = name[len(srv)+1:]
		}
		name = strings.ReplaceAll(name, "_", " ")
		if choice != "" {
			name += " (" + strings.ReplaceAll(choice, "#", ", ") + ")"
		}
		return name
	}
	return op
}
