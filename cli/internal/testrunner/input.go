package testrunner

import (
	"telara.dev/tap/internal/model"
)

// deriveDefaultInput builds a full default input object for a case that did
// not specify `input:` at all -- it fills manifest/workflow defaults,
// falls back to the first declared workflow example for a still-missing
// required field, and as a last resort (only for url-shaped fields tied to
// a bound browser origin slot) uses the bound origin itself. Cases that DO
// specify `input:` (even partially, as the negative
// missing_required_input_fails_at_gate case does) are used as-is with no
// filling -- see DECISION in the final report.
func deriveDefaultInput(pkg *model.Package, binds map[string]string) map[string]interface{} {
	out := map[string]interface{}{}
	wf := pkg.Workflow
	if wf == nil {
		return out
	}
	var singleOrigin string
	if pkg.Manifest.Requirements.Browser != nil && len(pkg.Manifest.Requirements.Browser.Origins) == 1 {
		if b, ok := binds[pkg.Manifest.Requirements.Browser.Origins[0].Slot]; ok {
			singleOrigin = b
		}
	}
	for name, ps := range wf.Inputs {
		if ps.HasDefault {
			out[name] = ps.Default
			continue
		}
		if !ps.Required {
			continue
		}
		if len(ps.Examples) > 0 {
			out[name] = ps.Examples[0]
			continue
		}
		if (ps.Format == "url" || ps.Format == "uri") && singleOrigin != "" {
			out[name] = singleOrigin
			continue
		}
		// else: leave missing; schema validation will surface it if the
		// field turns out to be truly required and unfillable.
	}
	return out
}
