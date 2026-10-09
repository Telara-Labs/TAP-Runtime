package main

import (
	"encoding/json"
	"fmt"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// Preview resolves possible tool bindings, not the program's future control
// flow. It never dispatches a tool, chooses a server, or grants authority.
type connectionPreview struct {
	Status      string              `json:"status"`
	Scope       string              `json:"scope"`
	Connections []previewConnection `json:"connections"`
	Truncated   bool                `json:"truncated"`
}

type previewConnection struct {
	Alias            string `json:"alias"`
	DeclaredEffect   string `json:"declared_effect"`
	Optional         bool   `json:"optional"`
	Status           string `json:"status"`
	Server           string `json:"server,omitempty"`
	Tool             string `json:"tool,omitempty"`
	Annotated        string `json:"annotated_effect,omitempty"`
	Effective        string `json:"base_effect,omitempty"`
	ApprovalRequired *bool  `json:"base_approval_required,omitempty"`
	RuntimeCheck     bool   `json:"requires_runtime_check"`
	Operation        string `json:"operation,omitempty"`
}

func emptyPreview(status string) connectionPreview {
	return connectionPreview{Status: status, Scope: "possible declared bindings; execution rechecks connections and per-call effects", Connections: []previewConnection{}}
}

func (s *server) previewConnections(m *mf.Manifest) connectionPreview {
	if len(m.Tools) == 0 {
		return emptyPreview("available")
	}
	s.mu.Lock()
	client := clientFor(s.clientName)
	relay := s.relay != nil && relayClient(client)
	s.mu.Unlock()
	// Relay and pinned-only clients do not expose a verified live inventory.
	// Do not manufacture resolved connections from the package's own pins.
	if s.mcpURL == "" && s.vscodeSocket == "" && relay {
		return emptyPreview("inventory_unavailable")
	}
	var b bridge.Bridge
	var err error
	switch {
	case s.mcpURL != "":
		b, err = openMCP(s.mcpURL, s.mcpHeaderFile, s.mcpServerName)
	case s.vscodeSocket != "":
		b, err = bridge.NewVSCode(s.vscodeSocket)
	default:
		b, err = openBridge(client, s.proc, m)
	}
	if err != nil {
		// Client errors may contain endpoints, credentials or response bodies.
		return emptyPreview("inventory_unavailable")
	}
	defer b.Close()
	if _, pinnedOnly := b.(bridge.PinnedOnly); pinnedOnly {
		return emptyPreview("inventory_unavailable")
	}
	return previewBindings(m, b, newFileBindings(defaultBindingsPath()))
}

// inventoryBridge freezes one inventory for all aliases and prevents future
// admission changes from accidentally turning inspection into execution.
type inventoryBridge struct {
	bridge.Bridge
	tools []bind.Tool
}

func (b inventoryBridge) Inventory() ([]bind.Tool, error) { return b.tools, nil }
func (b inventoryBridge) Call(bind.Tool, map[string]any) (string, error) {
	return "", fmt.Errorf("tool execution is forbidden during preview")
}
func (b inventoryBridge) Asks(t bind.Tool) (bool, error) {
	if asker, ok := b.Bridge.(bridge.Asker); ok {
		return asker.Asks(t)
	}
	return false, nil
}

type previewStore struct{ bindingStore }

func (previewStore) set(string, string, string) error {
	return fmt.Errorf("binding changes are forbidden during preview")
}

func previewBindings(m *mf.Manifest, b bridge.Bridge, store bindingStore) connectionPreview {
	p := emptyPreview("available")
	seen := map[string]bool{}
	for _, d := range m.Tools {
		if d.Alias == "" || seen[d.Alias] || !validEffect(d.Effect) {
			return emptyPreview("invalid_declarations")
		}
		seen[d.Alias] = true
	}
	inv, err := b.Inventory()
	if err != nil {
		return emptyPreview("inventory_unavailable")
	}
	cached := inventoryBridge{Bridge: b, tools: inv}
	var saved bindingStore
	if store != nil {
		saved = previewStore{store}
	}
	for i, d := range m.Tools {
		if i >= 100 {
			p.Truncated = true
			break
		}
		row := previewConnection{Alias: d.Alias, DeclaredEffect: d.Effect, Optional: d.Optional, Status: "unresolved", RuntimeCheck: true}
		// Required resolution preserves the reason an optional alias cannot
		// bind, rather than quietly dropping it from the flat list.
		d.Optional = false
		ambiguous := false
		choose := func(Pick) (string, bool) { ambiguous = true; return "", false }
		a, missing, err := admitOnce(saved, choose, []toolDecl{d}, cached, m.Capabilities...)
		if err != nil {
			switch {
			case ambiguous:
				row.Status = "ambiguous"
			case missing:
				row.Status = "unavailable"
			}
		} else {
			bd := &a.Bindings[0]
			row.Status, row.Server, row.Tool = "resolved", bd.Server, bd.Tool
			row.Annotated, row.Effective = bd.Annotated, bd.effective()
			approval := rankOf(bind.Effect(row.Effective)) >= rankOf(bind.Write)
			row.ApprovalRequired = &approval
			row.RuntimeCheck, row.Operation = bd.dispatch != nil, bd.Operation
		}
		p.Connections = append(p.Connections, row)
		// Bound encoded size, omitting whole identities rather than inventing
		// shortened server/tool names. No schemas, payloads or raw errors.
		encoded, err := json.Marshal(p)
		if err != nil || len(encoded) > 16<<10 {
			p.Connections = p.Connections[:len(p.Connections)-1]
			p.Truncated = true
			break
		}
	}
	return p
}
