// Package diffcmd implements `tap diff`: the interface + PERMISSION diff
// between two package directories (typically two versions of the same
// primitive), with additions highlighted per 04-cli.md ("permission
// expansion is highlighted; this is what reviewers read").
package diffcmd

import (
	"sort"

	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/schemautil"
)

type SetDiff struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

func (d SetDiff) IsExpansion() bool { return len(d.Added) > 0 }
func (d SetDiff) Empty() bool       { return len(d.Added) == 0 && len(d.Removed) == 0 }

type CredentialDiff struct {
	SlotsAdded    []string           `json:"slots_added,omitempty"`
	SlotsRemoved  []string           `json:"slots_removed,omitempty"`
	ActionsBySlot map[string]SetDiff `json:"actions_by_slot,omitempty"`
}

type Result struct {
	NameA, NameB               string
	VersionA, VersionB         string
	DescriptionA, DescriptionB string
	DescriptionChanged         bool

	InputPropsDiff  SetDiff `json:"input_props_diff"`
	OutputPropsDiff SetDiff `json:"output_props_diff"`

	Credentials    CredentialDiff `json:"credentials"`
	EgressHosts    SetDiff        `json:"egress_hosts"`
	OriginSlots    SetDiff        `json:"origin_slots"`
	ReasoningCaps  SetDiff        `json:"reasoning_capabilities"`
	MaxTokensDelta int            `json:"max_tokens_delta"`

	EffectsClassA, EffectsClassB string
	PermissionExpansion          bool `json:"permission_expansion"`
}

// Diff compares two loaded packages.
func Diff(a, b *model.Package) Result {
	ma, mb := a.Manifest, b.Manifest
	r := Result{
		NameA: ma.Metadata.FullName(), NameB: mb.Metadata.FullName(),
		VersionA: ma.Metadata.Version, VersionB: mb.Metadata.Version,
		DescriptionA: ma.Metadata.Description, DescriptionB: mb.Metadata.Description,
		EffectsClassA: ma.Effects.Class, EffectsClassB: mb.Effects.Class,
	}
	r.DescriptionChanged = ma.Metadata.Description != mb.Metadata.Description

	r.InputPropsDiff = diffStringSets(schemaPropNames(a, "input"), schemaPropNames(b, "input"))
	r.OutputPropsDiff = diffStringSets(schemaPropNames(a, "output"), schemaPropNames(b, "output"))

	r.Credentials = diffCredentials(ma, mb)
	r.EgressHosts = diffStringSets(egressHostStrings(ma), egressHostStrings(mb))
	r.OriginSlots = diffStringSets(originSlots(ma), originSlots(mb))

	var capsA, capsB []string
	tokA, tokB := 0, 0
	if ma.Requirements.Reasoning != nil {
		capsA = ma.Requirements.Reasoning.Capabilities
		tokA = ma.Requirements.Reasoning.MaxTokens
	}
	if mb.Requirements.Reasoning != nil {
		capsB = mb.Requirements.Reasoning.Capabilities
		tokB = mb.Requirements.Reasoning.MaxTokens
	}
	r.ReasoningCaps = diffStringSets(capsA, capsB)
	r.MaxTokensDelta = tokB - tokA

	r.PermissionExpansion = r.Credentials.ActionsExpanded() || len(r.Credentials.SlotsAdded) > 0 ||
		r.EgressHosts.IsExpansion() || r.OriginSlots.IsExpansion() || r.ReasoningCaps.IsExpansion() ||
		(ma.Effects.Class == "read" && mb.Effects.Class == "write")

	return r
}

func (c CredentialDiff) ActionsExpanded() bool {
	for _, sd := range c.ActionsBySlot {
		if sd.IsExpansion() {
			return true
		}
	}
	return false
}

// egressHostStrings renders each requirements.network.egressHosts[] entry
// as a comparable string for set-diffing: a literal host as-is, a
// `{slot: name}` egress-host slot (CHANGELOG.md v1 ruling 2) as its slot
// name -- so binding a new tenant host to an existing slot is not itself a
// permission expansion, only adding/removing a host or slot is.
func egressHostStrings(m *model.Manifest) []string {
	out := make([]string, 0, len(m.Requirements.Network.EgressHosts))
	for _, h := range m.Requirements.Network.EgressHosts {
		if h.Slot != "" {
			out = append(out, "slot:"+h.Slot)
			continue
		}
		out = append(out, h.Literal)
	}
	return out
}

func originSlots(m *model.Manifest) []string {
	var out []string
	if m.Requirements.Browser == nil {
		return out
	}
	for _, o := range m.Requirements.Browser.Origins {
		if o.Slot != "" {
			out = append(out, o.Slot)
		} else {
			out = append(out, o.Origin)
		}
	}
	return out
}

func diffCredentials(a, b *model.Manifest) CredentialDiff {
	byslotA := map[string]model.CredentialSlot{}
	byslotB := map[string]model.CredentialSlot{}
	for _, c := range a.Requirements.Credentials {
		byslotA[c.Slot] = c
	}
	for _, c := range b.Requirements.Credentials {
		byslotB[c.Slot] = c
	}
	cd := CredentialDiff{ActionsBySlot: map[string]SetDiff{}}
	for slot := range byslotB {
		if _, ok := byslotA[slot]; !ok {
			cd.SlotsAdded = append(cd.SlotsAdded, slot)
		}
	}
	for slot := range byslotA {
		if _, ok := byslotB[slot]; !ok {
			cd.SlotsRemoved = append(cd.SlotsRemoved, slot)
		}
	}
	sort.Strings(cd.SlotsAdded)
	sort.Strings(cd.SlotsRemoved)
	for slot, cb := range byslotB {
		ca, ok := byslotA[slot]
		if !ok {
			continue
		}
		sd := diffStringSets(ca.Actions, cb.Actions)
		if !sd.Empty() {
			cd.ActionsBySlot[slot] = sd
		}
	}
	return cd
}

func diffStringSets(a, b []string) SetDiff {
	setA := map[string]bool{}
	setB := map[string]bool{}
	for _, s := range a {
		setA[s] = true
	}
	for _, s := range b {
		setB[s] = true
	}
	var d SetDiff
	for s := range setB {
		if !setA[s] {
			d.Added = append(d.Added, s)
		}
	}
	for s := range setA {
		if !setB[s] {
			d.Removed = append(d.Removed, s)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	return d
}

func schemaPropNames(pkg *model.Package, which string) []string {
	var ref string
	var inline map[string]interface{}
	if which == "input" {
		ref = pkg.Manifest.InputSchemaPath()
		inline = pkg.Manifest.Interface.InputSchemaInline
	} else {
		ref = pkg.Manifest.OutputSchemaPath()
		inline = pkg.Manifest.Interface.OutputSchemaInline
	}
	if ref == "" && inline == nil {
		return nil
	}
	raw, _, err := schemautil.LoadSchemaDoc(ref, inline)
	if err != nil {
		return nil
	}
	props, ok := model.AsMap(raw["properties"])
	if !ok {
		return nil
	}
	return model.Keys(props)
}
