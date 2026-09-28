package model

import (
	"fmt"
	"path/filepath"
)

// Manifest is the typed projection of primitive.yaml (03-primitive-structure.md §3).
// Raw holds the full parsed document so validators can additionally walk it
// for closed-key checks without losing fidelity to whatever the author wrote.
type Manifest struct {
	Path       string
	Dir        string
	Raw        map[string]interface{}
	APIVersion string
	Kind       string

	Metadata     Metadata
	Interface    InterfaceSpec
	Requirements Requirements
	Effects      Effects
	Execution    Execution

	SuggestedMode string
}

type Metadata struct {
	Name              string
	Publisher         string
	Version           string
	Description       string
	OutputDescription string
	License           string
	Source            string
	ArtifactDigest    string
	ForkOf            interface{}
	Raw               map[string]interface{}
}

// FullName renders the reverse-DNS identity per README.md's naming
// convention: publisher/name when a publisher is declared (the newer v1
// form used by web-changelog-watch), else the bare dotted name (the
// v1alpha1 form used by gitlab-pipeline-triage). Both are accepted: the
// examples are ground truth and disagree on this, so tap treats publisher
// as optional.
func (m Metadata) FullName() string {
	if m.Publisher != "" {
		return m.Publisher + "/" + m.Name
	}
	return m.Name
}

type InterfaceSpec struct {
	InputSchemaRef     string
	InputSchemaInline  map[string]interface{}
	OutputSchemaRef    string
	OutputSchemaInline map[string]interface{}
	Raw                map[string]interface{}
}

type CredentialSlot struct {
	Slot        string
	Integration string
	Type        string
	Actions     []string
	Accepts     []string
	Identity    string
	Raw         map[string]interface{}
}

type OriginSlot struct {
	Slot   string // requirements.browser.origins[].slot (bound at install)
	Origin string // a literal origin instead of a slot, if the author chose to hardcode one
	Raw    interface{}
}

type BrowserReq struct {
	Session    string
	Origins    []OriginSlot
	Navigation string
	Downloads  bool
	Uploads    bool
	Clipboard  bool
	Raw        map[string]interface{}
}

// EgressHostEntry is one requirements.network.egressHosts[] entry: either a
// literal hostname, or a `{slot: name}` egress-host slot bound at install
// time -- identical semantics to browser.origins[] slots (CHANGELOG.md v1
// ruling 2, "egress-host slots"), for per-tenant SaaS hosts like
// `*.atlassian.net` that have no honest fixed literal.
type EgressHostEntry struct {
	Slot    string
	Literal string
	Raw     interface{}
}

type NetworkReq struct {
	EgressHosts []EgressHostEntry
	Raw         map[string]interface{}
}

type ReasoningReq struct {
	Provider     string
	Capabilities []string
	MaxTokens    int
	DataClasses  []string
	Raw          map[string]interface{}
}

type Requirements struct {
	Network     NetworkReq
	Credentials []CredentialSlot
	Browser     *BrowserReq
	Reasoning   *ReasoningReq
	Raw         map[string]interface{}
}

// Effects.Destructive was removed in v1 (CHANGELOG.md ruling 1): class now
// carries severity via the 5-value enum (read|write|destructive|financial|
// identity-admin). There is deliberately no Destructive field left on this
// struct; validate/manifest.go rejects the raw key outright if an author
// still writes it.
type Effects struct {
	Class      string
	Idempotent bool
	Raw        map[string]interface{}
}

type Execution struct {
	Runtime          string
	Entrypoint       string
	TimeoutSeconds   int
	Resumable        bool
	MaySuspend       []string
	RequiresFeatures []string
	// Triggers is execution.triggers (CHANGELOG.md v1 ruling 5): machine-
	// checkable capability metadata, values from {manual, schedule, event}.
	// Optional -- absence means "unasserted", not "manual-only".
	Triggers []string
	Raw      map[string]interface{}
}

// LoadManifest parses primitive.yaml at path into both the raw document (for
// closed-key checks) and a typed, best-effort projection (for semantic
// checks and for the other commands: diff/explain/test/dev).
func LoadManifest(path string) (*Manifest, error) {
	raw, _, err := LoadYAMLFile(path)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("%s: empty document", path)
	}

	m := &Manifest{
		Path: path,
		Dir:  filepath.Dir(path),
		Raw:  raw,
	}
	m.APIVersion = StringVal(raw, "apiVersion")
	m.Kind = StringVal(raw, "kind")

	if md, ok := AsMap(raw["metadata"]); ok {
		m.Metadata = Metadata{
			Name:              StringVal(md, "name"),
			Publisher:         StringVal(md, "publisher"),
			Version:           StringVal(md, "version"),
			Description:       StringVal(md, "description"),
			OutputDescription: StringVal(md, "output_description"),
			License:           StringVal(md, "license"),
			Source:            StringVal(md, "source"),
			ArtifactDigest:    StringVal(md, "artifactDigest"),
			ForkOf:            md["forkOf"],
			Raw:               md,
		}
	}

	if iface, ok := AsMap(raw["interface"]); ok {
		m.Interface.Raw = iface
		if in, ok := AsMap(iface["inputSchema"]); ok {
			if ref, ok := AsString(in["$ref"]); ok {
				m.Interface.InputSchemaRef = ref
			} else {
				m.Interface.InputSchemaInline = in
			}
		}
		if out, ok := AsMap(iface["outputSchema"]); ok {
			if ref, ok := AsString(out["$ref"]); ok {
				m.Interface.OutputSchemaRef = ref
			} else {
				m.Interface.OutputSchemaInline = out
			}
		}
	}

	if req, ok := AsMap(raw["requirements"]); ok {
		m.Requirements.Raw = req
		if net, ok := AsMap(req["network"]); ok {
			var hosts []EgressHostEntry
			if seq, ok := AsSlice(net["egressHosts"]); ok {
				for _, e := range seq {
					switch ev := e.(type) {
					case string:
						hosts = append(hosts, EgressHostEntry{Literal: ev, Raw: ev})
					case map[string]interface{}:
						hosts = append(hosts, EgressHostEntry{Slot: StringVal(ev, "slot"), Raw: ev})
					}
				}
			}
			m.Requirements.Network = NetworkReq{
				EgressHosts: hosts,
				Raw:         net,
			}
		}
		if creds, ok := AsSlice(req["credentials"]); ok {
			for _, c := range creds {
				cm, ok := AsMap(c)
				if !ok {
					continue
				}
				m.Requirements.Credentials = append(m.Requirements.Credentials, CredentialSlot{
					Slot:        StringVal(cm, "slot"),
					Integration: StringVal(cm, "integration"),
					Type:        StringVal(cm, "type"),
					Actions:     StringSliceVal(cm, "actions"),
					Accepts:     StringSliceVal(cm, "accepts"),
					Identity:    StringVal(cm, "identity"),
					Raw:         cm,
				})
			}
		}
		if br, ok := AsMap(req["browser"]); ok {
			b := &BrowserReq{
				Session:    StringVal(br, "session"),
				Navigation: StringVal(br, "navigation"),
				Downloads:  BoolVal(br, "downloads", false),
				Uploads:    BoolVal(br, "uploads", false),
				Clipboard:  BoolVal(br, "clipboard", false),
				Raw:        br,
			}
			if origins, ok := AsSlice(br["origins"]); ok {
				for _, o := range origins {
					switch ov := o.(type) {
					case string:
						b.Origins = append(b.Origins, OriginSlot{Origin: ov, Raw: ov})
					case map[string]interface{}:
						b.Origins = append(b.Origins, OriginSlot{
							Slot:   StringVal(ov, "slot"),
							Origin: StringVal(ov, "origin"),
							Raw:    ov,
						})
					}
				}
			}
			m.Requirements.Browser = b
		}
		if rs, ok := AsMap(req["reasoning"]); ok {
			m.Requirements.Reasoning = &ReasoningReq{
				Provider:     StringVal(rs, "provider"),
				Capabilities: StringSliceVal(rs, "capabilities"),
				MaxTokens:    IntVal(rs, "maxTokens", 0),
				DataClasses:  StringSliceVal(rs, "dataClasses"),
				Raw:          rs,
			}
		}
	}

	if eff, ok := AsMap(raw["effects"]); ok {
		m.Effects = Effects{
			Class:      StringVal(eff, "class"),
			Idempotent: BoolVal(eff, "idempotent", false),
			Raw:        eff,
		}
	}

	if ex, ok := AsMap(raw["execution"]); ok {
		m.Execution = Execution{
			Runtime:          StringVal(ex, "runtime"),
			Entrypoint:       StringVal(ex, "entrypoint"),
			TimeoutSeconds:   IntVal(ex, "timeoutSeconds", 0),
			Resumable:        BoolVal(ex, "resumable", false),
			MaySuspend:       StringSliceVal(ex, "may_suspend"),
			RequiresFeatures: StringSliceVal(ex, "requires_features"),
			Triggers:         StringSliceVal(ex, "triggers"),
			Raw:              ex,
		}
	}

	m.SuggestedMode = StringVal(raw, "suggested_mode")

	return m, nil
}

// CredentialBySlot looks up a declared credential slot by name.
func (m *Manifest) CredentialBySlot(slot string) (CredentialSlot, bool) {
	for _, c := range m.Requirements.Credentials {
		if c.Slot == slot {
			return c, true
		}
	}
	return CredentialSlot{}, false
}

// OriginBySlot looks up a declared browser origin slot by name.
func (m *Manifest) OriginBySlot(slot string) (OriginSlot, bool) {
	if m.Requirements.Browser == nil {
		return OriginSlot{}, false
	}
	for _, o := range m.Requirements.Browser.Origins {
		if o.Slot == slot {
			return o, true
		}
	}
	return OriginSlot{}, false
}
