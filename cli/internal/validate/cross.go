package validate

import (
	"fmt"

	"telara.dev/tap/internal/diag"
	"telara.dev/tap/internal/model"
)

// ValidateCrossPackage checks consistency between primitive.yaml and
// workflow.yaml: every credential/origin/egress a step implies must be
// declared in requirements (undeclared-requirement), and no write-class
// tool call may live inside a read-class manifest (effect-mismatch).
func ValidateCrossPackage(pkg *model.Package) diag.Findings {
	var out diag.Findings
	if pkg.Workflow == nil {
		return out
	}
	m := pkg.Manifest
	w := pkg.Workflow
	entry := m.Execution.Entrypoint
	if entry == "" {
		entry = "workflow.yaml"
	}

	hasAPIStep := false
	for i, s := range w.Steps {
		path := fmt.Sprintf("%s#steps[%d:%s]", entry, i, s.ID)
		if s.API != nil {
			hasAPIStep = true
			if s.API.Credential != "" {
				if _, ok := m.CredentialBySlot(s.API.Credential); !ok {
					out = append(out, diag.Error(diag.ClassUndeclaredRequirement, "undeclared-credential", path+".api.credential",
						fmt.Sprintf("step references credential slot %q which is not declared in requirements.credentials", s.API.Credential),
						"add a requirements.credentials[] entry with that slot name, or fix the typo"))
				}
			}
			if isWriteVerbTool(s.API.Tool) && m.Effects.Class == "read" {
				out = append(out, diag.Error(diag.ClassEffectMismatch, "write-tool-in-read-manifest", path+".api.tool",
					fmt.Sprintf("tool %q looks write-class but effects.class is 'read'", s.API.Tool),
					"either drop the step/switch to a read-only tool, or bump effects.class to write and re-run tap diff for the permission-expansion review"))
			}
		}
		if s.Browser != nil {
			if m.Requirements.Browser == nil {
				out = append(out, diag.Error(diag.ClassUndeclaredRequirement, "undeclared-browser-requirement", path+".browser",
					"step uses a browser: step but requirements.browser is not declared", ""))
			} else if s.Browser.Origin.Slot != "" {
				if _, ok := m.OriginBySlot(s.Browser.Origin.Slot); !ok {
					out = append(out, diag.Error(diag.ClassUndeclaredRequirement, "undeclared-origin-slot", path+".browser.origin",
						fmt.Sprintf("step references origin slot %q which is not declared in requirements.browser.origins", s.Browser.Origin.Slot),
						"add a requirements.browser.origins[] entry with that slot name"))
				}
			}
		}
	}
	if hasAPIStep && len(m.Requirements.Network.EgressHosts) == 0 && (m.Requirements.Browser == nil) {
		out = append(out, diag.Warn(diag.ClassUndeclaredRequirement, "empty-egress-hosts", entry+"#steps",
			"package has api: steps but requirements.network.egressHosts is empty", "declare the host(s) the bound integration will call"))
	}

	return out
}
