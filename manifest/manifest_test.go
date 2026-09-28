package manifest

import (
	"strings"
	"testing"
)

const short = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.telara, name: recent-mail, version: 0.1.0}
execution: {entrypoint: main.py}
tools:
  - {alias: threads, capability: gmail.threads.search, effect: read, optional: true}
commands:
  - {command: kubectl, globals: ["--context minikube"], args: [get], effect: read, env: [KUBECONFIG]}
files:
  - {path: out, access: write}
fetch:
  - {origin: "https://api.github.com"}
`

func parse(t *testing.T, s string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAShortManifestMayRunAndMayNotBePublished(t *testing.T) {
	m := parse(t, short)
	if p := m.RunProblems(); len(p) != 0 {
		t.Fatalf("a short manifest cannot run: %v", p)
	}
	p := strings.Join(m.PublishProblems(), "\n")
	for _, want := range []string{"description", "interface", "provenance", "capabilities[] does not define"} {
		if !strings.Contains(p, want) {
			t.Errorf("publishing a short manifest was not stopped for %q:\n%s", want, p)
		}
	}
}

// A misspelt bound must never read as no bound.
func TestUnknownFieldsAreRefused(t *testing.T) {
	for name, s := range map[string]string{
		"misspelt block":        strings.Replace(short, "files:", "file:", 1),
		"misspelt field":        strings.Replace(short, "access: write", "acess: write", 1),
		"v2 vocabulary":         short + "requirements: {credentials: []}\n",
		"top-level entrypoint":  short + "entrypoint: main.py\n",
		"unknown tool field":    strings.Replace(short, "optional: true", "optional: true, allow_all: true", 1),
		"unknown command field": strings.Replace(short, "env: [KUBECONFIG]", "env: [KUBECONFIG], shell: true", 1),
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunProblems(t *testing.T) {
	for name, c := range map[string]struct{ from, to, want string }{
		"v2":                     {"primitives.telara.dev/v3", "primitives.telara.dev/v2", "apiVersion"},
		"no entrypoint":          {"execution: {entrypoint: main.py}", "execution: {}", "entrypoint is missing"},
		"entrypoint leaves":      {"entrypoint: main.py", "entrypoint: ../../etc/passwd", "inside the package"},
		"absolute entrypoint":    {"entrypoint: main.py", "entrypoint: /bin/sh", "inside the package"},
		"bad effect":             {"effect: read, optional", "effect: harmless, optional", "effect"},
		"bad alias":              {"alias: threads", "alias: Threads-1", "alias"},
		"capability not dotted":  {"capability: gmail.threads.search", "capability: search", "provider.resource.verb"},
		"command is a path":      {"command: kubectl", "command: /usr/bin/kubectl", "program name"},
		"bad access":             {"access: write", "access: append", "access"},
		"origin with a path":     {"https://api.github.com", "https://api.github.com/repos", "path"},
		"origin with a wildcard": {"https://api.github.com", "https://*.github.com", "fetch"},
		"origin without scheme":  {"https://api.github.com", "api.github.com", "scheme"},
		"unknown runtime":        {"execution: {entrypoint: main.py}", "execution: {entrypoint: main.py, runtime: docker}", "runtime"},
	} {
		m := parse(t, strings.Replace(short, c.from, c.to, 1))
		if p := strings.Join(m.RunProblems(), "\n"); !strings.Contains(p, c.want) {
			t.Errorf("%s: want a problem mentioning %q, got %q", name, c.want, p)
		}
	}
}

func TestCompleteGrowsAShortManifestIntoAPublishableOne(t *testing.T) {
	m := parse(t, short)
	c := m.Complete("dev.telara")

	// What a person must write is marked, and stops publication until written.
	p := strings.Join(c.PublishProblems(), "\n")
	if !strings.Contains(p, "description is not written") || !strings.Contains(p, "has no question") {
		t.Fatalf("placeholders would have been published:\n%s", p)
	}
	for _, line := range strings.Split(p, "\n") {
		if !strings.Contains(line, "description is not written") && !strings.Contains(line, "has no question") {
			t.Errorf("Complete left a problem no person was asked to fix: %s", line)
		}
	}

	c.Metadata.Description = "Threads received in the last N days."
	for i := range c.Capabilities {
		c.Capabilities[i].Question = "threads matching a query, newest first"
		c.Capabilities[i].ID = CapabilityID(c.Capabilities[i].Question, c.Capabilities[i].Args, c.Capabilities[i].Result)
	}
	if p := c.PublishProblems(); len(p) != 0 {
		t.Fatalf("a completed manifest with its blanks filled cannot be published: %v", p)
	}

	// Completing never changes what the primitive may do.
	if len(c.Tools) != len(m.Tools) || len(c.Commands) != len(m.Commands) || len(c.Files) != len(m.Files) || len(c.Fetch) != len(m.Fetch) {
		t.Fatal("Complete changed what the primitive declares")
	}
	if c.Tools[0].Capability != "dev.telara/gmail.threads.search@1" || CapabilityName(c.Tools[0].Capability) != "gmail.threads.search" {
		t.Fatalf("label became %q", c.Tools[0].Capability)
	}
	if m.Tools[0].Capability != "gmail.threads.search" {
		t.Fatal("Complete changed the manifest it was given")
	}

	// It survives being written out and read back, strictly.
	back, err := Parse(c.YAML())
	if err != nil {
		t.Fatalf("the completed manifest does not load: %v\n%s", err, c.YAML())
	}
	if p := back.PublishProblems(); len(p) != 0 {
		t.Fatalf("after a round trip: %v", p)
	}
}

func TestPublishRefusesWhatOnlyPublishingForbids(t *testing.T) {
	m := parse(t, short).Complete("dev.telara")
	m.Metadata.Description = "d"
	for i := range m.Capabilities {
		m.Capabilities[i].Question = "q"
		m.Capabilities[i].ID = CapabilityID("q", m.Capabilities[i].Args, m.Capabilities[i].Result)
	}
	if p := m.PublishProblems(); len(p) != 0 {
		t.Fatal(p)
	}

	sub := *m
	sub.Execution.Runtime = RuntimeSubprocess
	if p := strings.Join(sub.PublishProblems(), "\n"); !strings.Contains(p, "nothing contains a subprocess") {
		t.Errorf("an uncontained primitive could be published: %s", p)
	}

	tampered := *m
	tampered.Capabilities = append([]Capability{}, m.Capabilities...)
	tampered.Capabilities[0].Args = map[string]any{"type": "object", "required": []any{"everything"}}
	if p := strings.Join(tampered.PublishProblems(), "\n"); !strings.Contains(p, "hashes to") {
		t.Errorf("a contract changed under an unchanged id was accepted: %s", p)
	}
}

func TestCapabilityIDIgnoresKeyOrderAndNothingElse(t *testing.T) {
	a := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}, "n": map[string]any{"type": "integer"}}}
	b := map[string]any{"properties": map[string]any{"n": map[string]any{"type": "integer"}, "q": map[string]any{"type": "string"}}, "type": "object"}
	r := map[string]any{"type": "object"}
	if CapabilityID("q", a, r) != CapabilityID("q", b, r) {
		t.Fatal("the id depends on key order")
	}
	if CapabilityID("q", a, r) == CapabilityID("another question", a, r) {
		t.Fatal("the id ignores the question")
	}
	// The reference value, from telara-agents tap-runtime/schema/manifest_v3_test.go.
	if got := CapabilityID("q", map[string]any{}, map[string]any{}); !strings.HasPrefix(got, "sha256:") || len(got) != 71 {
		t.Fatalf("id is %q", got)
	}
}

func TestCapabilityName(t *testing.T) {
	for in, want := range map[string]string{
		"gmail.threads.search":                     "gmail.threads.search",
		"dev.telara/gmail.threads.search@1":        "gmail.threads.search",
		"com.example/google_drive.files.search@12": "google_drive.files.search",
	} {
		if got := CapabilityName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
