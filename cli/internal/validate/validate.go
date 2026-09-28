// Package validate implements `tap validate`: manifest schema (closed field
// allowlist), JSON-Schema validity of interface schemas, description rules
// (03 §3.1), credential-slot rules (03 §3.0), and the workflow lint suite
// (09 §10), reporting findings under the 04-cli.md §3 failure taxonomy.
package validate

import (
	"telara.dev/tap/internal/diag"
	"telara.dev/tap/internal/model"
)

// Validate runs every rule against a loaded package.
func Validate(pkg *model.Package) diag.Findings {
	var out diag.Findings
	out = append(out, ValidateManifest(pkg)...)
	out = append(out, ValidateWorkflow(pkg)...)
	out = append(out, ValidateCrossPackage(pkg)...)
	return out
}
