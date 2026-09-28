package testrunner

import (
	"os"
	"path/filepath"
	"testing"

	"telara.dev/tap/internal/model"
)

// writeMinimalPackage writes a minimal-but-loadable api-shape package (inline
// schemas, no $ref files needed) to a temp dir, for testrunner-focused unit
// tests that don't need full `tap validate` coverage.
func writeMinimalPackage(t *testing.T, files map[string]string) *model.Package {
	t.Helper()
	dir := t.TempDir()
	base := map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema:
    type: object
    properties: {}
  outputSchema:
    type: object
    properties: {}

requirements:
  network:
    egressHosts: [example.com, {slot: tenant_site}]
  credentials: []

effects:
  class: read
  idempotent: true

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
	}
	for k, v := range files {
		base[k] = v
	}
	for rel, content := range base {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkg, err := model.LoadPackage(dir)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	return pkg
}

// TestRun_WhenGatesAPIStep is the CHANGELOG.md v1 CLI fix item 1 regression:
// the test runner must honor `when:` on api: steps (previously only
// transform/reason steps were gated -- STATUS.md A3.3's finding). A step
// gated false must be skipped WITHOUT requiring a fixture entry for its
// tool; if the gate were ignored, this case would fail with
// "no fixture entry for tool" instead of passing.
func TestRun_WhenGatesAPIStep(t *testing.T) {
	pkg := writeMinimalPackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  enabled:
    type: boolean
    default: false

steps:
  - id: maybe_fetch
    when: inputs.enabled == true
    api:
      integration: example
      tool: get_thing
      params: {}

outputs:
  fetched: {from: steps.maybe_fetch}
`,
		"tests/contract.test.yaml": `cases:
  - name: gate_false_skips_api_step_without_fixture
    input: {enabled: false}
    expect:
      output_schema_valid: true
      steps_executed: 0
      output:
        fetched: null
`,
	})
	report, err := Run(pkg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range report.Cases {
		if !c.Passed {
			t.Fatalf("case %q failed: %v", c.Name, c.Failures)
		}
	}
}

// TestRun_WhenGatesBrowserStep mirrors TestRun_WhenGatesAPIStep for a
// browser: step.
func TestRun_WhenGatesBrowserStep(t *testing.T) {
	pkg := writeMinimalPackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  enabled:
    type: boolean
    default: false

steps:
  - id: maybe_page
    when: inputs.enabled == true
    browser:
      action: extract
      origin: {slot: tenant_site}
      url: "https://example.dev/x"

outputs:
  page: {from: steps.maybe_page}
`,
		"tests/contract.test.yaml": `cases:
  - name: gate_false_skips_browser_step_without_fixture
    input: {enabled: false}
    expect:
      output_schema_valid: true
      steps_executed: 0
      output:
        page: null
`,
	})
	report, err := Run(pkg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range report.Cases {
		if !c.Passed {
			t.Fatalf("case %q failed: %v", c.Name, c.Failures)
		}
	}
}

// TestRun_NowTestCaseFieldInjectsReservedInput is the CHANGELOG.md v1 ruling
// 3 regression: a contract-test case may pin the reserved `now` input via
// `now: <iso>`, and it must be resolvable as {from: inputs.now} without ever
// being declared in workflow.yaml's `inputs:` block.
func TestRun_NowTestCaseFieldInjectsReservedInput(t *testing.T) {
	pkg := writeMinimalPackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs: {}

steps:
  - id: echo_now
    transform:
      language: starlark
      expression: "result = {\"now\": now}"
      params:
        now: {from: inputs.now}

outputs:
  now: {from: steps.echo_now.now}
`,
		"tests/contract.test.yaml": `cases:
  - name: now_pinned_by_test_case
    now: "2026-07-13T00:00:00Z"
    expect:
      output_schema_valid: true
      output:
        now: "2026-07-13T00:00:00Z"
`,
	})
	report, err := Run(pkg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range report.Cases {
		if !c.Passed {
			t.Fatalf("case %q failed: %v", c.Name, c.Failures)
		}
	}
}

// TestEgressHostAllowed_Slot exercises the egress-host slot mechanism
// (CHANGELOG.md v1 ruling 2) directly: a literal entry matches by exact
// host; a `{slot: name}` entry matches only when bound to that host, and an
// unbound (or host-mismatched) slot must NOT grant a blanket allow -- that
// would silently defeat the very undeclared-egress negative tests the
// mechanism exists to support.
func TestEgressHostAllowed_Slot(t *testing.T) {
	hosts := []model.EgressHostEntry{
		{Literal: "gitlab.com"},
		{Slot: "jira_site"},
	}
	if !egressHostAllowed(hosts, "gitlab.com", nil) {
		t.Fatalf("expected literal host to be allowed")
	}
	if egressHostAllowed(hosts, "evil.example.com", nil) {
		t.Fatalf("expected undeclared host to be refused")
	}
	if egressHostAllowed(hosts, "acme-corp.atlassian.net", nil) {
		t.Fatalf("expected slot-declared host to be refused when the slot is unbound")
	}
	binds := map[string]string{"jira_site": "https://acme-corp.atlassian.net"}
	if !egressHostAllowed(hosts, "acme-corp.atlassian.net", binds) {
		t.Fatalf("expected host matching the bound slot value to be allowed")
	}
	if egressHostAllowed(hosts, "other-corp.atlassian.net", binds) {
		t.Fatalf("expected a host that doesn't match the bound slot value to be refused")
	}
}
