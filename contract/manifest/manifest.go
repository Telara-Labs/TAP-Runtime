// Package manifest loads primitive.yaml.
//
// One format, two levels. To RUN, a
// manifest needs a name, an entrypoint and what the primitive uses. To
// PUBLISH it must satisfy manifest.v3.schema.json in full: a description,
// input and output schemas, a contract for every capability, provenance.
// Complete fills in what can be derived and marks what a person must write.
//
// A short manifest is a full one with parts left out, never a different
// shape, so a primitive grows into publication without being rewritten.
package manifest

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/contract/canon"
	"github.com/Telara-Labs/TAP-Runtime/contract/glob"
	"github.com/santhosh-tekuri/jsonschema/v5"
	"golang.org/x/net/publicsuffix"
	"gopkg.in/yaml.v3"
)

//go:embed manifest.v3.schema.json
var schemaJSON []byte

const APIVersion = "primitives.telara.dev/v3"

type Manifest struct {
	APIVersion   string       `yaml:"apiVersion" json:"apiVersion"`
	Kind         string       `yaml:"kind" json:"kind"`
	Metadata     Metadata     `yaml:"metadata" json:"metadata"`
	Interface    *Interface   `yaml:"interface,omitempty" json:"interface,omitempty"`
	Capabilities []Capability `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Tools        []Tool       `yaml:"tools,omitempty" json:"tools,omitempty"`
	Commands     []Command    `yaml:"commands,omitempty" json:"commands,omitempty"`
	Files        []File       `yaml:"files,omitempty" json:"files,omitempty"`
	Fetch        []Fetch      `yaml:"fetch,omitempty" json:"fetch,omitempty"`
	Oracle       any          `yaml:"oracle,omitempty" json:"oracle,omitempty"`
	Requires     any          `yaml:"requires,omitempty" json:"requires,omitempty"`
	Execution    Execution    `yaml:"execution" json:"execution"`
	Provenance   *Provenance  `yaml:"provenance,omitempty" json:"provenance,omitempty"`
	Tests        any          `yaml:"tests,omitempty" json:"tests,omitempty"`
}

type Metadata struct {
	Publisher         string `yaml:"publisher" json:"publisher"`
	Name              string `yaml:"name" json:"name"`
	Version           string `yaml:"version" json:"version"`
	Description       string `yaml:"description,omitempty" json:"description,omitempty"`
	OutputDescription string `yaml:"output_description,omitempty" json:"output_description,omitempty"`
	License           string `yaml:"license,omitempty" json:"license,omitempty"`
	Source            string `yaml:"source,omitempty" json:"source,omitempty"`
	ForkOf            any    `yaml:"forkOf,omitempty" json:"forkOf,omitempty"`
}

type Interface struct {
	InputSchema  map[string]any `yaml:"inputSchema" json:"inputSchema"`
	OutputSchema map[string]any `yaml:"outputSchema" json:"outputSchema"`
}

type Capability struct {
	Label    string         `yaml:"label" json:"label"`
	ID       string         `yaml:"id" json:"id"`
	Question string         `yaml:"question" json:"question"`
	Args     map[string]any `yaml:"args" json:"args"`
	Result   map[string]any `yaml:"result" json:"result"`
}

type Pin struct {
	Client string `yaml:"client,omitempty" json:"client,omitempty"`
	Server string `yaml:"server,omitempty" json:"server,omitempty"`
	Tool   string `yaml:"tool,omitempty" json:"tool,omitempty"`
}

type Tool struct {
	Alias      string `yaml:"alias" json:"alias"`
	Capability string `yaml:"capability" json:"capability"`
	Effect     string `yaml:"effect" json:"effect"`
	Optional   bool   `yaml:"optional,omitempty" json:"optional,omitempty"`
	Pin        *Pin   `yaml:"pin,omitempty" json:"pin,omitempty"`
}

type Command struct {
	Command string   `yaml:"command" json:"command"`
	Globals []string `yaml:"globals,omitempty" json:"globals,omitempty"`
	// Args is nil when the manifest says nothing, and empty when it says [].
	Args   []string `yaml:"args" json:"args"`
	Effect string   `yaml:"effect" json:"effect"`
	Env    []string `yaml:"env,omitempty" json:"env,omitempty"`
}

type File struct {
	Path   string `yaml:"path" json:"path"`
	Access string `yaml:"access" json:"access"`
}

type Fetch struct {
	Origin  string   `yaml:"origin" json:"origin"`
	Methods []string `yaml:"methods,omitempty" json:"methods,omitempty"`
}

type Execution struct {
	Runtime        string         `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Entrypoint     string         `yaml:"entrypoint" json:"entrypoint"`
	TimeoutSeconds int            `yaml:"timeoutSeconds,omitempty" json:"timeoutSeconds,omitempty"`
	Resumable      bool           `yaml:"resumable,omitempty" json:"resumable,omitempty"`
	MaySuspend     []string       `yaml:"may_suspend,omitempty" json:"may_suspend,omitempty"`
	Triggers       []string       `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	Limits         map[string]int `yaml:"limits,omitempty" json:"limits,omitempty"`
	Params         map[string]any `yaml:"params,omitempty" json:"params,omitempty"`
}

type Provenance struct {
	Source    string `yaml:"source" json:"source"`
	Toolchain string `yaml:"toolchain" json:"toolchain"`
	Build     string `yaml:"build" json:"build"`
	Sealed    bool   `yaml:"sealed,omitempty" json:"sealed,omitempty"`
}

const (
	RuntimeWasm       = "tap-wasm-v1"
	RuntimeSubprocess = "tap-subprocess-v1"
)

var (
	aliasRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	nameRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	pathRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,254}$`)
	fullLabelRe = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)+/([a-z][a-z0-9_.-]{0,127})@[0-9]+$`)
	shortLabel  = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)+$`)
	envRe       = regexp.MustCompile(`^[A-Za-z_*?\[][A-Za-z0-9_*?\[\]!^-]*$`)
	effects     = map[string]bool{"read": true, "write": true, "destructive": true, "financial": true, "identity-admin": true}
)

// Load reads and strictly decodes primitive.yaml. A field the format does not
// have is an error: a misspelt bound must never read as no bound.
func Load(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "primitive.yaml"))
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("primitive.yaml: %w", err)
	}
	return &m, nil
}

// CapabilityName is the part of a capability label that says what it is:
// gmail.threads.search, from either the short form or the full
// publisher/name@version form.
func CapabilityName(label string) string {
	if m := fullLabelRe.FindStringSubmatch(label); m != nil {
		return m[2]
	}
	return label
}

// Runtime is the declared runtime, or the floor when none is declared.
func (m *Manifest) Runtime() string {
	if m.Execution.Runtime == "" {
		return RuntimeWasm
	}
	return m.Execution.Runtime
}

// RunProblems lists what stops this manifest from being run. Empty means it
// may run.
func (m *Manifest) RunProblems() []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	if m.APIVersion != APIVersion {
		add("apiVersion is %q; this runner admits %s only", m.APIVersion, APIVersion)
	}
	if m.Kind != "Primitive" {
		add("kind is %q; it must be Primitive", m.Kind)
	}
	if !nameRe.MatchString(m.Metadata.Name) {
		add("metadata.name %q must be lower-case letters, digits and hyphens, starting with a letter", m.Metadata.Name)
	}
	switch {
	case m.Execution.Entrypoint == "":
		add("execution.entrypoint is missing")
	case !pathRe.MatchString(m.Execution.Entrypoint) || strings.Contains(m.Execution.Entrypoint, ".."):
		add("execution.entrypoint %q must be a path inside the package", m.Execution.Entrypoint)
	}
	if r := m.Runtime(); r != RuntimeWasm && r != RuntimeSubprocess {
		add("execution.runtime %q is not one this runner knows", r)
	}
	seen := map[string]bool{}
	for i, t := range m.Tools {
		switch {
		case !aliasRe.MatchString(t.Alias):
			add("tools[%d].alias %q must be lower-case letters, digits and underscores, starting with a letter", i, t.Alias)
		case seen[t.Alias]:
			add("tools[%d].alias %q is declared twice", i, t.Alias)
		}
		seen[t.Alias] = true
		if !effects[t.Effect] {
			add("tools[%d] (%s) declares effect %q; it must be read, write, destructive, financial or identity-admin", i, t.Alias, t.Effect)
		}
		if !fullLabelRe.MatchString(t.Capability) && !shortLabel.MatchString(t.Capability) {
			add("tools[%d] (%s) capability %q must be written provider.resource.verb, or publisher/provider.resource.verb@version", i, t.Alias, t.Capability)
		}
		if t.Pin != nil && (t.Pin.Server == "" || t.Pin.Tool == "") && t.Pin.Client == "" {
			add("tools[%d] (%s) has a pin that names nothing", i, t.Alias)
		}
	}
	for i, c := range m.Commands {
		if c.Command == "" || strings.ContainsAny(c.Command, "/ ") || glob.HasMeta(c.Command) {
			add("commands[%d].command %q must be a program name, not a path or a pattern", i, c.Command)
		}
		if !effects[c.Effect] {
			add("commands[%d] (%s) declares effect %q", i, c.Command, c.Effect)
		}
		for _, g := range c.Globals {
			if !strings.HasPrefix(g, "-") {
				add("commands[%d] (%s) global %q must start with a flag", i, c.Command, g)
			}
		}
		// What a command may be given is declared, never left
		// open by saying nothing. args: [] is a declaration, and
		// means the command takes none. What is refused is saying nothing.
		if c.Args == nil {
			add("commands[%d] (%s) declares no args; list the arguments it may be given, as patterns: [\"*\"] for any, [] for none", i, c.Command)
		}
		for _, a := range c.Args {
			if a == "" {
				add("commands[%d] (%s) has an empty argument pattern", i, c.Command)
			}
		}
		for _, e := range c.Env {
			if !envRe.MatchString(e) {
				add("commands[%d] (%s) env %q must be a variable name or a pattern of one, such as AWS_*", i, c.Command, e)
			}
			if strings.Trim(e, "*?") == "" {
				add("commands[%d] (%s) env %q would give the program the whole environment; name what it needs", i, c.Command, e)
			}
		}
	}
	for i, f := range m.Files {
		if f.Path == "" {
			add("files[%d] has no path", i)
		}
		if f.Access != "read" && f.Access != "write" {
			add("files[%d] (%s) declares access %q; it must be read or write", i, f.Path, f.Access)
		}
	}
	for i, f := range m.Fetch {
		// A wildcard label is not a valid host to the URL parser's taste in
		// every position, so it is parsed with a stand-in and checked apart.
		u, err := url.Parse(strings.Replace(f.Origin, "://*.", "://wildcard-label.", 1))
		if err == nil && strings.Contains(f.Origin, "://*.") {
			u.Host = strings.Replace(u.Host, "wildcard-label.", "*.", 1)
		}
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
			add("fetch[%d] origin %q must be a scheme and a host, such as https://api.example.com", i, f.Origin)
		case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil:
			add("fetch[%d] origin %q must not carry a path, a query or a user", i, f.Origin)
		default:
			if why := wildcardProblem(u.Hostname()); why != "" {
				add("fetch[%d] origin %q %s", i, f.Origin, why)
			}
		}
	}
	return p
}

// wildcardProblem says what is wrong with a host that uses a wildcard, or ""
// One subdomain level may be a wildcard: *.atlassian.net. Never
// a bare *, never a wildcard anywhere but the first label, and never over a
// public suffix: *.com and *.co.uk would match every site under them.
func wildcardProblem(host string) string {
	if !strings.Contains(host, "*") {
		return ""
	}
	if !strings.HasPrefix(host, "*.") || strings.Contains(host[2:], "*") {
		return "may use a wildcard only as its first label, as in https://*.example.com"
	}
	rest := host[2:]
	if suffix, _ := publicsuffix.PublicSuffix(rest); suffix == rest || !strings.Contains(rest, ".") {
		return "would match every site under " + rest + ", which is a public suffix"
	}
	return ""
}

// CapabilityID is the identity of a capability: sha256 over its contract in
// canonical form, RFC 8785. Two publishers who write the same contract get
// the same id, whatever language they write it in.
func CapabilityID(question string, args, result map[string]any) string {
	// RFC 8785, so that a publisher writing in any language computes the
	// same id for the same contract. Go's own encoder escapes < > and &,
	// which no other language's does.
	b, err := canon.CanonicalValue(map[string]any{"args": args, "question": question, "result": result})
	if err != nil {
		// A contract that cannot be written canonically has no identity.
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PublishProblems lists what stops this manifest from being published. It
// includes everything that stops it from running.
func (m *Manifest) PublishProblems() []string {
	p := m.RunProblems()
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }

	// An uncontained primitive cannot be published.
	if m.Runtime() != RuntimeWasm {
		add("execution.runtime is %s; only %s may be published, because nothing contains a subprocess", m.Runtime(), RuntimeWasm)
	}
	sc, err := jsonschema.CompileString("https://primitives.telara.dev/schemas/tap/manifest.v3.schema.json", string(schemaJSON))
	if err != nil {
		return append(p, "the v3 schema does not compile: "+err.Error())
	}
	var doc any
	b, _ := json.Marshal(m)
	json.Unmarshal(b, &doc)
	if err := sc.Validate(doc); err != nil {
		if ve, ok := err.(*jsonschema.ValidationError); ok {
			for _, leaf := range leaves(ve) {
				add("%s: %s", strings.TrimPrefix(leaf.InstanceLocation, "/"), leaf.Message)
			}
		} else {
			add("%v", err)
		}
	}
	byLabel := map[string]Capability{}
	for _, c := range m.Capabilities {
		byLabel[c.Label] = c
		if want := CapabilityID(c.Question, c.Args, c.Result); c.ID != want {
			add("capability %s carries id %s; its contract hashes to %s", c.Label, c.ID, want)
		}
		if strings.TrimSpace(c.Question) == "" || strings.HasPrefix(c.Question, TODO) {
			add("capability %s has no question; say in one sentence what the tool is asked", c.Label)
		}
	}
	for _, t := range m.Tools {
		if _, ok := byLabel[t.Capability]; !ok {
			add("tool %s names capability %s, which capabilities[] does not define", t.Alias, t.Capability)
		}
	}
	if strings.TrimSpace(m.Metadata.Description) == "" || strings.HasPrefix(m.Metadata.Description, TODO) {
		add("metadata.description is not written")
	}
	sort.Strings(p)
	return dedupe(p)
}

func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

func dedupe(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// TODO marks a field Complete could not derive and a person must write.
const TODO = "TODO:"

// Complete returns a copy with everything a publishable manifest needs that
// can be derived from the short one, and TODO markers where a person has to
// write something. It never changes what the primitive is allowed to do.
func (m *Manifest) Complete(publisher string) *Manifest {
	c := *m
	if c.Metadata.Publisher == "" {
		c.Metadata.Publisher = publisher
	}
	if c.Metadata.Version == "" {
		c.Metadata.Version = "0.1.0"
	}
	if c.Metadata.Description == "" {
		c.Metadata.Description = TODO + " say what this primitive does"
	}
	if c.Interface == nil {
		c.Interface = &Interface{InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}
	}
	c.Execution.Runtime = c.Runtime()
	if c.Provenance == nil {
		pv := &Provenance{Source: c.Execution.Entrypoint}
		switch filepath.Ext(c.Execution.Entrypoint) {
		case ".wasm":
			pv.Source, pv.Toolchain, pv.Build = "src/", TODO+" the toolchain and its version", TODO+" the exact build command"
		default:
			pv.Toolchain = "interpreter obtained by the runner, pinned by sha256"
			pv.Build = "none: the entrypoint is the source"
		}
		c.Provenance = pv
	}
	have := map[string]bool{}
	c.Capabilities = append([]Capability{}, m.Capabilities...)
	for _, cp := range c.Capabilities {
		have[cp.Label] = true
	}
	c.Tools = append([]Tool{}, m.Tools...)
	for i, t := range c.Tools {
		if !fullLabelRe.MatchString(t.Capability) {
			t.Capability = c.Metadata.Publisher + "/" + t.Capability + "@1"
			c.Tools[i] = t
		}
		if !have[t.Capability] {
			have[t.Capability] = true
			q := TODO + " what is this tool asked, in one sentence"
			args, result := map[string]any{"type": "object"}, map[string]any{"type": "object"}
			c.Capabilities = append(c.Capabilities, Capability{Label: t.Capability, ID: CapabilityID(q, args, result), Question: q, Args: args, Result: result})
		}
	}
	return &c
}

// YAML writes the manifest as it would appear in primitive.yaml.
func (m *Manifest) YAML() []byte {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	enc.Encode(m)
	return b.Bytes()
}
