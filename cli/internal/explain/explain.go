// Package explain renders a primitive.yaml as a human-readable permission
// panel: credentials + actions, egress, origins, effects, lease budget --
// the summary a reviewer reads at handshake/adoption time (03-primitive-structure.md
// §4, 06-security.md).
package explain

import (
	"fmt"
	"strings"

	"telara.dev/tap/internal/model"
)

type CredentialRow struct {
	Slot        string   `json:"slot"`
	Integration string   `json:"integration"`
	Type        string   `json:"type,omitempty"`
	Actions     []string `json:"actions"`
	Accepts     []string `json:"accepts,omitempty"`
	Identity    string   `json:"identity,omitempty"`
}

type OriginRow struct {
	Slot   string `json:"slot,omitempty"`
	Origin string `json:"origin,omitempty"`
}

type Panel struct {
	Name               string          `json:"name"`
	Version            string          `json:"version"`
	Description        string          `json:"description"`
	OutputDescription  string          `json:"output_description"`
	EffectsClass       string          `json:"effects_class"`
	Idempotent         bool            `json:"idempotent"`
	Credentials        []CredentialRow `json:"credentials"`
	EgressHosts        []string        `json:"egress_hosts"`
	BrowserSession     string          `json:"browser_session,omitempty"`
	Origins            []OriginRow     `json:"origins,omitempty"`
	Navigation         string          `json:"navigation,omitempty"`
	Downloads          bool            `json:"downloads"`
	Uploads            bool            `json:"uploads"`
	Clipboard          bool            `json:"clipboard"`
	ReasoningProvider  string          `json:"reasoning_provider,omitempty"`
	ReasoningMaxTokens int             `json:"reasoning_max_tokens,omitempty"`
	ReasoningCaps      []string        `json:"reasoning_capabilities,omitempty"`
	Runtime            string          `json:"runtime"`
	Entrypoint         string          `json:"entrypoint"`
	TimeoutSeconds     int             `json:"timeout_seconds"`
	RequiresFeatures   []string        `json:"requires_features"`
	SuggestedMode      string          `json:"suggested_mode,omitempty"`
}

func BuildPanel(m *model.Manifest) Panel {
	p := Panel{
		Name:              m.Metadata.FullName(),
		Version:           m.Metadata.Version,
		Description:       m.Metadata.Description,
		OutputDescription: m.Metadata.OutputDescription,
		EffectsClass:      m.Effects.Class,
		Idempotent:        m.Effects.Idempotent,
		EgressHosts:       egressHostStrings(m.Requirements.Network.EgressHosts),
		Runtime:           m.Execution.Runtime,
		Entrypoint:        m.Execution.Entrypoint,
		TimeoutSeconds:    m.Execution.TimeoutSeconds,
		RequiresFeatures:  m.Execution.RequiresFeatures,
		SuggestedMode:     m.SuggestedMode,
	}
	for _, c := range m.Requirements.Credentials {
		p.Credentials = append(p.Credentials, CredentialRow{
			Slot: c.Slot, Integration: c.Integration, Type: c.Type, Actions: c.Actions, Accepts: c.Accepts, Identity: c.Identity,
		})
	}
	if b := m.Requirements.Browser; b != nil {
		p.BrowserSession = b.Session
		p.Navigation = b.Navigation
		p.Downloads = b.Downloads
		p.Uploads = b.Uploads
		p.Clipboard = b.Clipboard
		for _, o := range b.Origins {
			p.Origins = append(p.Origins, OriginRow{Slot: o.Slot, Origin: o.Origin})
		}
	}
	if r := m.Requirements.Reasoning; r != nil {
		p.ReasoningProvider = r.Provider
		p.ReasoningMaxTokens = r.MaxTokens
		p.ReasoningCaps = r.Capabilities
	}
	return p
}

// Render formats the panel as a human-readable table.
func Render(p Panel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s @ %s\n", p.Name, p.Version)
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", len(p.Name)+len(p.Version)+3))
	fmt.Fprintf(&b, "\nDescription:\n  %s\n", wrap(p.Description))
	if p.OutputDescription != "" {
		fmt.Fprintf(&b, "\nOutput:\n  %s\n", wrap(p.OutputDescription))
	}

	fmt.Fprintf(&b, "\nEffects:\n")
	fmt.Fprintf(&b, "  class:       %s\n", p.EffectsClass)
	fmt.Fprintf(&b, "  idempotent:  %v\n", p.Idempotent)

	fmt.Fprintf(&b, "\nCredentials:\n")
	if len(p.Credentials) == 0 {
		fmt.Fprintf(&b, "  (none)\n")
	}
	for _, c := range p.Credentials {
		extra := ""
		if c.Type != "" {
			extra += fmt.Sprintf(" type=%s", c.Type)
		}
		if c.Identity != "" {
			extra += fmt.Sprintf(" identity=%s", c.Identity)
		}
		if len(c.Accepts) > 0 {
			extra += fmt.Sprintf(" accepts=%v [PINNED]", c.Accepts)
		}
		fmt.Fprintf(&b, "  - slot=%s integration=%s actions=%v%s\n", c.Slot, c.Integration, c.Actions, extra)
	}

	fmt.Fprintf(&b, "\nNetwork egress:\n")
	if len(p.EgressHosts) == 0 {
		fmt.Fprintf(&b, "  (none declared)\n")
	} else {
		fmt.Fprintf(&b, "  %v\n", p.EgressHosts)
	}

	if p.BrowserSession != "" || len(p.Origins) > 0 {
		fmt.Fprintf(&b, "\nBrowser:\n")
		fmt.Fprintf(&b, "  session:    %s\n", p.BrowserSession)
		fmt.Fprintf(&b, "  navigation: %s\n", p.Navigation)
		fmt.Fprintf(&b, "  downloads=%v uploads=%v clipboard=%v\n", p.Downloads, p.Uploads, p.Clipboard)
		for _, o := range p.Origins {
			if o.Slot != "" {
				fmt.Fprintf(&b, "  - origin slot: %s (bound at install)\n", o.Slot)
			} else {
				fmt.Fprintf(&b, "  - origin: %s\n", o.Origin)
			}
		}
	}

	if p.ReasoningProvider != "" {
		fmt.Fprintf(&b, "\nReasoning lease:\n")
		fmt.Fprintf(&b, "  provider:     %s\n", p.ReasoningProvider)
		fmt.Fprintf(&b, "  maxTokens:    %d\n", p.ReasoningMaxTokens)
		fmt.Fprintf(&b, "  capabilities: %v\n", p.ReasoningCaps)
	}

	fmt.Fprintf(&b, "\nExecution:\n")
	fmt.Fprintf(&b, "  runtime:           %s\n", p.Runtime)
	fmt.Fprintf(&b, "  entrypoint:        %s\n", p.Entrypoint)
	fmt.Fprintf(&b, "  timeoutSeconds:    %d\n", p.TimeoutSeconds)
	fmt.Fprintf(&b, "  requires_features: %v\n", p.RequiresFeatures)
	if p.SuggestedMode != "" {
		fmt.Fprintf(&b, "  suggested_mode:    %s\n", p.SuggestedMode)
	}

	return b.String()
}

func wrap(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}

// egressHostStrings renders each requirements.network.egressHosts[] entry
// for the human-readable panel: a literal host as-is, a `{slot: name}`
// egress-host slot (CHANGELOG.md v1 ruling 2) as "{slot: name}" so a
// reviewer can see it's bound at install, not a fixed value.
func egressHostStrings(hosts []model.EgressHostEntry) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h.Slot != "" {
			out = append(out, fmt.Sprintf("{slot: %s}", h.Slot))
			continue
		}
		out = append(out, h.Literal)
	}
	return out
}
