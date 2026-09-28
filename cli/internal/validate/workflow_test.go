package validate

import (
	"testing"

	"telara.dev/tap/internal/diag"
)

func TestValidateWorkflow_DanglingFromRef(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: steps.nonexistent.field}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "dangling-from-ref") {
		t.Fatalf("expected dangling-from-ref finding, got %v", findings)
	}
}

func TestValidateWorkflow_BadPathShapeRejectsWildcard(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: "steps.echo.items[-1]"}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "bad-path-shape") {
		t.Fatalf("expected bad-path-shape finding, got %v", findings)
	}
}

func TestValidateWorkflow_EmptyRetryOnRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: inputs.message}
    retry: {max_attempts: 3, initial_backoff_seconds: 2, retry_on: []}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "empty-retry-on") {
		t.Fatalf("expected empty-retry-on finding, got %v", findings)
	}
}

func TestValidateWorkflow_DoubleBraceRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: echo
    transform:
      language: starlark
      expression: "result = {{ message }}"

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "double-brace-binding") {
		t.Fatalf("expected double-brace-binding finding, got %v", findings)
	}
}

func TestValidateWorkflow_MissingSrcFileRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/does_not_exist.star
      params:
        message: {from: inputs.message}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "missing-src-file") {
		t.Fatalf("expected missing-src-file finding, got %v", findings)
	}
}

func TestValidateWorkflow_DependencyCycleRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: a
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: steps.b.message}
  - id: b
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: steps.a.message}

outputs:
  message: {from: steps.a.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "dependency-cycle") {
		t.Fatalf("expected dependency-cycle finding, got %v", findings)
	}
}

// TestValidateWorkflow_UnusedInputWarns is the CHANGELOG.md v1 CLI fix item
// 4 regression (the reverse lint): a declared workflow input that no step
// or output references anywhere must produce an unused-input WARNING
// (STATUS.md A3.0 finding: the regen-acceptance ground truth's dead
// `pipeline_id` input passed validate clean before this fix).
func TestValidateWorkflow_UnusedInputWarns(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true
  dead_input:
    type: string

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: inputs.message}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	f := findByBlocker(findings, "unused-input")
	if f == nil {
		t.Fatalf("expected unused-input finding for dead_input, got %v", findings)
	}
	if f.Severity != diag.SeverityWarning {
		t.Fatalf("expected unused-input to be a warning, got severity %q", f.Severity)
	}
	if findings.HasErrors() {
		t.Fatalf("unused-input alone should not be an error-level finding, got %v", findings.Errors())
	}
}

// TestValidateWorkflow_NowInputExemptFromUnusedLint proves `now` (the
// reserved auto-injected input, CHANGELOG.md v1 ruling 3) is exempt from
// the unused-input lint even when declared and never wired to a step.
func TestValidateWorkflow_NowInputExemptFromUnusedLint(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true
  now:
    type: string

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: inputs.message}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if hasBlocker(findings, "unused-input") {
		t.Fatalf("did not expect an unused-input finding for the reserved now input, got %v", findings)
	}
}

// TestValidateWorkflow_NowInputRequiredRejected is the CHANGELOG.md v1
// ruling 3 regression: a workflow.yaml `now` input declared `required:
// true` must be rejected -- it is reserved and auto-injected, never
// caller-required.
func TestValidateWorkflow_NowInputRequiredRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true
  now:
    type: string
    required: true

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: inputs.message}
        stamp: {from: inputs.now}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "reserved-input-now-required") {
		t.Fatalf("expected reserved-input-now-required finding, got %v", findings)
	}
}

// TestValidateWorkflow_SingleStepModifierAccepted is the CHANGELOG.md v1
// ruling 9 regression ("ratified as-implemented"): `single: true` must be
// accepted by validate as a recognized step key (compiler enforcement of
// the actual error-on-empty behavior is a B2 deliverable, not this fix).
func TestValidateWorkflow_SingleStepModifierAccepted(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: echo
    single: true
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: inputs.message}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if hasBlocker(findings, "unknown-field") {
		t.Fatalf("expected single: true to be an accepted step key, got %v", findings)
	}
	if !pkg.Workflow.Steps[0].Single {
		t.Fatalf("expected the Single field to be parsed as true")
	}
}

func TestValidateWorkflow_ConditionMustBeWellFormed(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: echo
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: inputs.message}
  - id: maybe
    when: steps.echo.message == steps.echo.other
    transform:
      language: starlark
      expression_from: src/echo.star
      params:
        message: {from: inputs.message}

outputs:
  message: {from: steps.echo.message}
`,
	})
	findings := ValidateWorkflow(pkg)
	if !hasBlocker(findings, "condition-not-well-formed") {
		t.Fatalf("expected condition-not-well-formed finding, got %v", findings)
	}
}
