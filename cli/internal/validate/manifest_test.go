package validate

import (
	"os"
	"path/filepath"
	"testing"

	"telara.dev/tap/internal/diag"
	"telara.dev/tap/internal/model"
)

// writePackage writes a minimal-but-valid api-shape package to dir and
// returns it as a loaded model.Package, so tests can mutate one file (via
// the overrides map: relative path -> full replacement content) and assert
// on the resulting findings.
func writePackage(t *testing.T, overrides map[string]string) *model.Package {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: >-
    Fetch a thing and return it unchanged. Read-only.
  output_description: >-
    Echoes the thing back.
  license: Apache-2.0
  source: ""
  artifactDigest: sha256:TBD-at-publish
  forkOf: null

interface:
  inputSchema:
    $ref: schemas/input.json
  outputSchema:
    $ref: schemas/output.json

requirements:
  network:
    egressHosts: []
  credentials: []

effects:
  class: read
  idempotent: true

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
  resumable: false
  may_suspend: []
  requires_features: [transform.starlark]

suggested_mode: autonomous
`,
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true
    description: Text to echo back unchanged.

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
		"src/echo.star": "result = {\"message\": message}\n",
		"schemas/input.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["message"],
  "properties": {"message": {"type": "string"}}
}
`,
		"schemas/output.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["message"],
  "properties": {"message": {"type": "string"}}
}
`,
	}
	for k, v := range overrides {
		files[k] = v
	}
	for rel, content := range files {
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

func TestValidateManifest_CleanPackagePasses(t *testing.T) {
	pkg := writePackage(t, nil)
	findings := ValidateManifest(pkg)
	if findings.HasErrors() {
		t.Fatalf("expected no errors, got %v", findings)
	}
}

func TestValidateManifest_UnknownFieldRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.
  bogus_metadata_field: true

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: []}
  credentials: []

effects: {class: read, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
	})
	findings := ValidateManifest(pkg)
	if !hasBlocker(findings, "unknown-field") {
		t.Fatalf("expected unknown-field finding, got %v", findings)
	}
}

func TestValidateManifest_CredentialMissingActions(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: [example.com]}
  credentials:
    - slot: example
      integration: example

effects: {class: read, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
	})
	findings := ValidateManifest(pkg)
	if !hasBlocker(findings, "credential-missing-actions") {
		t.Fatalf("expected credential-missing-actions finding, got %v", findings)
	}
}

func TestValidateManifest_UnjustifiedAcceptsIsWarning(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: [example.com]}
  credentials:
    - slot: example
      integration: example
      actions: [things.read]
      accepts: [oauth2]

effects: {class: read, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
	})
	findings := ValidateManifest(pkg)
	if findings.HasErrors() {
		t.Fatalf("accepts: should only warn, got errors: %v", findings.Errors())
	}
	if !hasBlocker(findings, "credential-accepts-unjustified") {
		t.Fatalf("expected credential-accepts-unjustified warning, got %v", findings)
	}
}

func TestValidateManifest_BadEffectsClassRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: []}
  credentials: []

effects: {class: maybe, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
	})
	findings := ValidateManifest(pkg)
	if !hasBlocker(findings, "bad-effects-class") {
		t.Fatalf("expected bad-effects-class finding, got %v", findings)
	}
}

// TestValidateManifest_EffectsClassFullEnumAccepted is the CHANGELOG.md v1
// ruling 1 regression: effects.class must accept the full 5-value enum, not
// just read|write.
func TestValidateManifest_EffectsClassFullEnumAccepted(t *testing.T) {
	for _, class := range []string{"read", "write", "destructive", "financial", "identity-admin"} {
		t.Run(class, func(t *testing.T) {
			pkg := writePackage(t, map[string]string{
				"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: []}
  credentials: []

effects: {class: ` + class + `, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
			})
			findings := ValidateManifest(pkg)
			if hasBlocker(findings, "bad-effects-class") {
				t.Fatalf("expected class %q to be accepted, got %v", class, findings)
			}
		})
	}
}

// TestValidateManifest_EffectsDestructiveFieldRejected is the CHANGELOG.md
// v1 ruling 1 regression: effects.destructive was removed; the CLI must
// reject the key outright with the "removed in v1; class carries it"
// message, not silently ignore it or file a generic unknown-field finding.
func TestValidateManifest_EffectsDestructiveFieldRejected(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: []}
  credentials: []

effects: {class: read, idempotent: true, destructive: false}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
	})
	findings := ValidateManifest(pkg)
	if !hasBlocker(findings, "effects-destructive-removed") {
		t.Fatalf("expected effects-destructive-removed finding, got %v", findings)
	}
	if hasBlocker(findings, "unknown-field") {
		t.Fatalf("did not expect a redundant unknown-field finding alongside the named rejection, got %v", findings)
	}
}

// TestValidateManifest_EgressHostSlotAccepted is the CHANGELOG.md v1 ruling
// 2 regression: a `{slot: name}` egress-host entry must validate cleanly
// (mirrors the existing browser.origins[] slot mechanism).
func TestValidateManifest_EgressHostSlotAccepted(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network:
    egressHosts: [gitlab.com, {slot: jira_site}]
  credentials: []

effects: {class: read, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
  requires_features: [egress_slots]
`,
	})
	findings := ValidateManifest(pkg)
	if findings.HasErrors() {
		t.Fatalf("expected an egress-host slot entry to validate clean, got %v", findings.Errors())
	}
	if len(pkg.Manifest.Requirements.Network.EgressHosts) != 2 {
		t.Fatalf("expected 2 egress host entries, got %d", len(pkg.Manifest.Requirements.Network.EgressHosts))
	}
	if pkg.Manifest.Requirements.Network.EgressHosts[1].Slot != "jira_site" {
		t.Fatalf("expected the second entry to be parsed as slot jira_site, got %+v", pkg.Manifest.Requirements.Network.EgressHosts[1])
	}
}

// TestValidateManifest_NowNotAllowedInInputSchemaRequired is the
// CHANGELOG.md v1 ruling 3 regression: `now` must never appear in
// inputSchema.required -- it is a reserved, runtime-auto-injected input.
func TestValidateManifest_NowNotAllowedInInputSchemaRequired(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"schemas/input.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["message", "now"],
  "properties": {"message": {"type": "string"}, "now": {"type": "string"}}
}
`,
	})
	findings := ValidateManifest(pkg)
	if !hasBlocker(findings, "reserved-input-now-required") {
		t.Fatalf("expected reserved-input-now-required finding, got %v", findings)
	}
}

// TestValidateManifest_DescriptionAndCredentialSlotFindingsAreContentClass
// is the CHANGELOG.md v1 CLI fix item 6 regression: description and
// credential-slot findings must be filed under diag.ClassContent, the class
// added at G0 specifically for them (STATUS.md G0 agenda item 3).
func TestValidateManifest_DescriptionAndCredentialSlotFindingsAreContentClass(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: See https://example.com/live-resource for details

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: [example.com]}
  credentials:
    - slot: example
      integration: example

effects: {class: read, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
	})
	findings := ValidateManifest(pkg)
	descFinding := findByBlocker(findings, "description-embedded-url")
	if descFinding == nil {
		t.Fatalf("expected description-embedded-url finding, got %v", findings)
	}
	if descFinding.Class != diag.ClassContent {
		t.Fatalf("expected description finding under the content class, got %q", descFinding.Class)
	}
	slotFinding := findByBlocker(findings, "credential-missing-actions")
	if slotFinding == nil {
		t.Fatalf("expected credential-missing-actions finding, got %v", findings)
	}
	if slotFinding.Class != diag.ClassContent {
		t.Fatalf("expected credential-slot finding under the content class, got %q", slotFinding.Class)
	}
}

func TestValidateCrossPackage_UndeclaredCredential(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: fetch
    api:
      integration: example
      tool: get_thing
      credential: example
      params:
        id: {from: inputs.message}

outputs:
  message: {from: steps.fetch.message}
`,
	})
	findings := ValidateCrossPackage(pkg)
	if !hasBlocker(findings, "undeclared-credential") {
		t.Fatalf("expected undeclared-credential finding, got %v", findings)
	}
}

func TestValidateCrossPackage_EffectMismatch(t *testing.T) {
	pkg := writePackage(t, map[string]string{
		"primitive.yaml": `apiVersion: primitives.telara.dev/v1
kind: Primitive

metadata:
  name: test.pkg
  version: 0.1.0
  description: A thing.

interface:
  inputSchema: {$ref: schemas/input.json}
  outputSchema: {$ref: schemas/output.json}

requirements:
  network: {egressHosts: [example.com]}
  credentials:
    - slot: example
      integration: example
      actions: [things.write]

effects: {class: read, idempotent: true}

execution:
  runtime: telara-workflow-v1
  entrypoint: workflow.yaml
  timeoutSeconds: 60
`,
		"workflow.yaml": `apiVersion: primitives.telara.dev/v1
kind: Workflow

inputs:
  message:
    type: string
    required: true

steps:
  - id: mutate
    api:
      integration: example
      tool: create_thing
      credential: example
      params:
        id: {from: inputs.message}

outputs:
  message: {from: steps.mutate.message}
`,
	})
	findings := ValidateCrossPackage(pkg)
	if !hasBlocker(findings, "write-tool-in-read-manifest") {
		t.Fatalf("expected write-tool-in-read-manifest finding, got %v", findings)
	}
}
