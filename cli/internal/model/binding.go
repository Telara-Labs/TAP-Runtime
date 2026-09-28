package model

import "strings"

// ParseFrom returns the path string of a `{from: ...}` binding map, and
// whether v was such a map. Per 09-workflow-spec.md §5, literal wins over
// expression and the two are never combined, so a `from:` map here is
// assumed to carry no sibling keys worth inspecting.
func ParseFrom(v interface{}) (string, bool) {
	m, ok := AsMap(v)
	if !ok {
		return "", false
	}
	f, ok := m["from"]
	if !ok {
		return "", false
	}
	s, ok := f.(string)
	return s, ok
}

// PathSegments splits a binding path like "steps.jobs.items.0.id" into
// segments, per 09 §5: "dot + non-negative numeric index only".
func PathSegments(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, ".")
}

// FileRefParams splits a step's params map into plain bindings and
// `*_from` file-reference params (09 §7 T1 / SPEC-FEEDBACK #1 in the
// gitlab-pipeline-triage README: "expression_from / params.*_from file
// inclusion" — the compiler/runtime loads the referenced src/ file and
// binds it to the global named by stripping the `_from` suffix).
func FileRefParams(params map[string]interface{}) (plain map[string]interface{}, fileRefs map[string]string) {
	plain = map[string]interface{}{}
	fileRefs = map[string]string{}
	for k, v := range params {
		if strings.HasSuffix(k, "_from") {
			if s, ok := v.(string); ok {
				fileRefs[strings.TrimSuffix(k, "_from")] = s
				continue
			}
		}
		plain[k] = v
	}
	return plain, fileRefs
}
