package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"telara.dev/tap/internal/diag"
	"telara.dev/tap/internal/model"
)

var (
	workflowTopKeys = []string{"apiVersion", "kind", "inputs", "steps", "outputs"}
	paramSpecKeys   = []string{"type", "format", "required", "default", "description", "examples", "min", "max", "enum", "aliases"}
	stepKeys        = []string{"id", "api", "browser", "code", "primitive", "transform", "reason", "branch", "approval", "when", "retry", "after", "single"}
	apiStepKeys     = []string{"integration", "tool", "credential", "params", "paginate"}
	browserStepKeys = []string{"action", "origin", "url", "plan_from", "on_drift", "session"}
	transformKeys   = []string{"language", "expression", "expression_from", "params"}
	reasonKeys      = []string{"purpose", "input", "inputSchema", "outputSchema", "maxTokens", "capabilities", "dataClasses"}
	retryKeys       = []string{"max_attempts", "initial_backoff_seconds", "retry_on"}
	branchKeys      = []string{"cases"}
	branchCaseKeys  = []string{"when", "default", "then"}

	// dot + non-negative numeric index only (09 §5): reject wildcards,
	// slices, and negative indices.
	pathShapeRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)*$`)
)

type fromRef struct {
	Path     string
	Location string
}

// ValidateWorkflow runs the workflow.yaml checks: closed field allowlist and
// the 09 §10 lint suite that doesn't require cross-referencing the manifest.
// Manifest cross-checks (undeclared egress/credentials/origins, effect
// mismatch) live in ValidateCrossPackage since they need both documents.
func ValidateWorkflow(pkg *model.Package) diag.Findings {
	var out diag.Findings
	if pkg.WorkflowErr != nil {
		out = append(out, diag.Error(diag.ClassSchema, "workflow-parse-error", pkg.Manifest.Execution.Entrypoint, pkg.WorkflowErr.Error(), ""))
		return out
	}
	w := pkg.Workflow
	if w == nil {
		return out
	}
	entry := pkg.Manifest.Execution.Entrypoint
	if entry == "" {
		entry = "workflow.yaml"
	}

	if raw, err := os.ReadFile(w.Path); err == nil {
		if strings.Contains(string(raw), "{{") {
			out = append(out, diag.Error(diag.ClassSchema, "double-brace-binding", entry,
				"workflow contains '{{ }}' bindings; TAP's native dialect is bare paths (trigger.x, nodes.id.field) -- 09 §5",
				"rewrite using {from: ...} authoring syntax"))
		}
	}

	out = append(out, checkClosed(w.Raw, workflowTopKeys, entry)...)
	if w.Kind != "Workflow" {
		out = append(out, diag.Error(diag.ClassSchema, "bad-kind", entry+"#kind", fmt.Sprintf("kind must be Workflow, got %q", w.Kind), ""))
	}

	for name, ps := range w.Inputs {
		path := fmt.Sprintf("%s#inputs.%s", entry, name)
		out = append(out, checkClosed(ps.Raw, paramSpecKeys, path)...)
		if (ps.Format == "iso_date" || ps.Format == "date") && len(ps.Examples) == 0 {
			out = append(out, diag.Error(diag.ClassSchema, "missing-format-examples", path,
				"format: iso_date params must carry examples (inherited catalog lint, 09 §10)",
				"add examples: [\"YYYY-MM-DD\"]"))
		}
		// CHANGELOG.md v1 ruling 3: `now` is a reserved, runtime-auto-injected
		// input (ISO-8601 datetime); it must never be declared required from
		// the caller.
		if name == "now" && ps.Required {
			out = append(out, diag.Error(diag.ClassSchema, "reserved-input-now-required", path,
				"the \"now\" workflow input is reserved (auto-injected ISO-8601 datetime by the runtime); it must not be declared required from the caller",
				"remove required: true (or the whole declaration) -- reference it as {from: inputs.now} without declaring it"))
		}
	}

	out = append(out, checkUnusedInputs(pkg, w, entry)...)

	ids := map[string]int{}
	for i, s := range w.Steps {
		ids[s.ID] = i
	}

	for i, s := range w.Steps {
		path := fmt.Sprintf("%s#steps[%d:%s]", entry, i, s.ID)
		out = append(out, checkClosed(s.Raw, stepKeys, path)...)
		if s.ID == "" {
			out = append(out, diag.Error(diag.ClassSchema, "step-missing-id", path, "every step requires an id", ""))
		}
		if n := countStepTypes(s); n != 1 {
			out = append(out, diag.Error(diag.ClassSchema, "step-type-ambiguous", path,
				fmt.Sprintf("step must have exactly one of api/browser/code/primitive/transform/reason/branch/approval, found %d", n), ""))
		}

		var refs []fromRef
		var fileRefs []string // relative paths that must exist under pkg.Dir

		switch {
		case s.API != nil:
			out = append(out, checkClosed(s.API.Raw, apiStepKeys, path+".api")...)
			if s.API.Integration == "" {
				out = append(out, diag.Error(diag.ClassSchema, "api-missing-integration", path+".api", "api step requires integration", ""))
			}
			if s.API.Tool == "" {
				out = append(out, diag.Error(diag.ClassSchema, "api-missing-tool", path+".api", "api step requires tool", ""))
			} else if isBannedNodeType(s.API.Tool) {
				out = append(out, diag.Error(diag.ClassSchema, "banned-node-type", path+".api.tool",
					fmt.Sprintf("tool %q validates but always fails at execution (09 §6 sharp-edge list)", s.API.Tool), "remove this step"))
			}
			plain, fr := model.FileRefParams(s.API.Params)
			for k, v := range plain {
				scanBindingValue(v, path+".api.params."+k, &refs)
			}
			for k, rel := range fr {
				fileRefs = append(fileRefs, rel)
				_ = k
			}
		case s.Browser != nil:
			out = append(out, checkClosed(s.Browser.Raw, browserStepKeys, path+".browser")...)
			if s.Browser.PlanFrom != "" {
				fileRefs = append(fileRefs, s.Browser.PlanFrom)
			}
			scanBindingValue(s.Browser.URL, path+".browser.url", &refs)
		case s.Transform != nil:
			out = append(out, checkClosed(s.Transform.Raw, transformKeys, path+".transform")...)
			if s.Transform.Language != "starlark" {
				out = append(out, diag.Error(diag.ClassSchema, "unsupported-transform-language", path+".transform.language",
					fmt.Sprintf("only language: starlark is supported by this runtime, got %q", s.Transform.Language), ""))
			}
			if s.Transform.Expression == "" && s.Transform.ExpressionFrom == "" {
				out = append(out, diag.Error(diag.ClassSchema, "transform-missing-expression", path+".transform", "transform requires expression or expression_from", ""))
			}
			if s.Transform.ExpressionFrom != "" {
				fileRefs = append(fileRefs, s.Transform.ExpressionFrom)
			}
			plain, fr := model.FileRefParams(s.Transform.Params)
			for k, v := range plain {
				scanBindingValue(v, path+".transform.params."+k, &refs)
			}
			for _, rel := range fr {
				fileRefs = append(fileRefs, rel)
			}
		case s.Reason != nil:
			out = append(out, checkClosed(s.Reason.Raw, reasonKeys, path+".reason")...)
			if s.Reason.Purpose == "" {
				out = append(out, diag.Error(diag.ClassSchema, "reason-missing-purpose", path+".reason", "reason step requires purpose", ""))
			}
			scanBindingValue(s.Reason.Input, path+".reason.input", &refs)
		case s.Branch != nil:
			out = append(out, checkClosed(s.Branch.Raw, branchKeys, path+".branch")...)
			defaults := 0
			for ci, c := range s.Branch.Cases {
				out = append(out, checkClosed(c.Raw, branchCaseKeys, fmt.Sprintf("%s.branch.cases[%d]", path, ci))...)
				if c.Default {
					defaults++
				} else if c.When != "" {
					out = append(out, validateCondition(c.When, fmt.Sprintf("%s.branch.cases[%d].when", path, ci), w, ids)...)
				}
			}
			if defaults != 1 {
				out = append(out, diag.Error(diag.ClassSchema, "branch-default-count", path+".branch",
					fmt.Sprintf("branch must have exactly one default case, found %d", defaults),
					"compiler makes cases mutually exclusive; author must supply exactly one default (09 §4)"))
			}
		}

		if s.HasWhen {
			out = append(out, validateCondition(s.When, path+".when", w, ids)...)
		}

		if s.Retry != nil {
			out = append(out, checkClosed(s.Retry.Raw, retryKeys, path+".retry")...)
			if s.Retry.RetryOnKeyPresent && len(s.Retry.RetryOn) == 0 {
				out = append(out, diag.Error(diag.ClassSchema, "empty-retry-on", path+".retry.retry_on",
					"retry_on present but empty -- an empty retry_on retries every error in the executor (09 §4 sharp edge)",
					"omit retry_on to get the curated default [timeout, unavailable, rate_limit], or list specific transient error codes"))
			}
		}
		for _, a := range s.After {
			if _, ok := ids[a]; !ok {
				out = append(out, diag.Error(diag.ClassSchema, "dangling-after-ref", path+".after",
					fmt.Sprintf("after references unknown step id %q", a), ""))
			}
		}

		for _, rel := range fileRefs {
			if rel == "" {
				continue
			}
			if _, err := os.Stat(pkg.ResolvePath(rel)); err != nil {
				out = append(out, diag.Error(diag.ClassSchema, "missing-src-file", path,
					fmt.Sprintf("referenced file %q does not exist in package", rel), ""))
			}
		}

		for _, r := range refs {
			out = append(out, validateFromRef(r, w, ids)...)
		}
	}

	for name, v := range w.Outputs {
		var refs []fromRef
		scanBindingValue(v, fmt.Sprintf("%s#outputs.%s", entry, name), &refs)
		for _, r := range refs {
			out = append(out, validateFromRef(r, w, ids)...)
		}
	}

	out = append(out, checkDependencyCycles(w, ids, entry)...)
	out = append(out, checkInputSchemaOverlap(pkg)...)

	return out
}

func countStepTypes(s model.Step) int {
	n := 0
	for _, present := range []bool{s.API != nil, s.Browser != nil, s.Code != nil, s.Primitive != nil, s.Transform != nil, s.Reason != nil, s.Branch != nil, s.Approval != nil} {
		if present {
			n++
		}
	}
	return n
}

// scanBindingValue recursively collects every `{from: ...}` binding found
// under v, tagging each with a diagnostic location. It also recurses into
// nested maps/slices so composite expressions (e.g. {days_ago: {from: ...}})
// are still caught.
func scanBindingValue(v interface{}, loc string, refs *[]fromRef) {
	switch tv := v.(type) {
	case map[string]interface{}:
		if from, ok := model.ParseFrom(tv); ok {
			*refs = append(*refs, fromRef{Path: from, Location: loc})
			return
		}
		for k, sub := range tv {
			scanBindingValue(sub, loc+"."+k, refs)
		}
	case []interface{}:
		for i, sub := range tv {
			scanBindingValue(sub, fmt.Sprintf("%s[%d]", loc, i), refs)
		}
	}
}

// validateFromRef resolves a `from:` path against declared inputs/steps
// (09 §5's "single most valuable lint": unknown-prefix expressions resolve
// to a literal string silently at runtime, so tap must catch dangling refs
// at authoring time).
func validateFromRef(r fromRef, w *model.Workflow, ids map[string]int) diag.Findings {
	var out diag.Findings
	if strings.HasPrefix(r.Path, "workflow.") || strings.HasPrefix(r.Path, "outputs.") {
		out = append(out, diag.Error(diag.ClassSchema, "accidental-alias-expression", r.Location,
			fmt.Sprintf("from: %q uses workflow.*/outputs.* which are accidental executor aliases, not the intended reference (09 §5)", r.Path),
			"use inputs.* or steps.<id>.* instead"))
		return out
	}
	if !pathShapeRe.MatchString(r.Path) {
		out = append(out, diag.Error(diag.ClassSchema, "bad-path-shape", r.Location,
			fmt.Sprintf("from: %q is not dot + non-negative numeric index only; no wildcards/slices/negative indices (09 §5)", r.Path),
			"express anything fancier as a transform: step"))
		return out
	}
	segs := model.PathSegments(r.Path)
	if len(segs) < 2 {
		out = append(out, diag.Error(diag.ClassSchema, "dangling-from-ref", r.Location,
			fmt.Sprintf("from: %q does not resolve to a declared input or step", r.Path), ""))
		return out
	}
	switch segs[0] {
	case "inputs":
		name := segs[1]
		if _, ok := w.Inputs[name]; !ok {
			out = append(out, diag.Error(diag.ClassSchema, "dangling-from-ref", r.Location,
				fmt.Sprintf("from: %q references undeclared input %q", r.Path, name), ""))
		}
	case "steps":
		stepID := segs[1]
		if _, ok := ids[stepID]; !ok {
			out = append(out, diag.Error(diag.ClassSchema, "dangling-from-ref", r.Location,
				fmt.Sprintf("from: %q references undeclared step %q", r.Path, stepID), ""))
		}
	default:
		out = append(out, diag.Error(diag.ClassSchema, "dangling-from-ref", r.Location,
			fmt.Sprintf("from: %q must start with inputs. or steps.", r.Path), ""))
	}
	return out
}

var conditionRe = regexp.MustCompile(`^(\S+)\s*(==|!=)\s*(.+)$`)

// validateCondition checks `when:`/branch-case condition grammar per 09 §6:
// `==`/`!=` with a literal-only RHS and stringified comparison, or bare
// truthiness. The LHS path is also resolved as a from-ref.
func validateCondition(cond, loc string, w *model.Workflow, ids map[string]int) diag.Findings {
	var out diag.Findings
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return out
	}
	if m := conditionRe.FindStringSubmatch(cond); m != nil {
		lhs, rhs := m[1], strings.TrimSpace(m[3])
		out = append(out, validateFromRef(fromRef{Path: lhs, Location: loc}, w, ids)...)
		if !isWellFormedLiteral(rhs) {
			out = append(out, diag.Error(diag.ClassSchema, "condition-not-well-formed", loc,
				fmt.Sprintf("condition %q has a non-literal right-hand side; the executor's grammar only supports a literal RHS (09 §6)", cond),
				"compile richer logic into a transform: step producing a boolean"))
		}
		return out
	}
	// Bare truthiness: the whole string must be a resolvable path.
	out = append(out, validateFromRef(fromRef{Path: cond, Location: loc}, w, ids)...)
	return out
}

func isWellFormedLiteral(s string) bool {
	if s == "true" || s == "false" {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return true
	}
	return false
}

// checkDependencyCycles builds the from:-implied step DAG and rejects
// cycles (a cycle can never schedule; 09 §6 requires the compiler to prove
// liveness, and a cyclic graph fails trivially).
func checkDependencyCycles(w *model.Workflow, ids map[string]int, entry string) diag.Findings {
	var out diag.Findings
	deps := make(map[string]map[string]bool, len(w.Steps))
	for _, s := range w.Steps {
		deps[s.ID] = map[string]bool{}
		var refs []fromRef
		scanBindingValue(s.Raw, "", &refs)
		for _, r := range refs {
			segs := model.PathSegments(r.Path)
			if len(segs) >= 2 && segs[0] == "steps" {
				deps[s.ID][segs[1]] = true
			}
		}
		for _, a := range s.After {
			deps[s.ID][a] = true
		}
		if s.HasWhen {
			if m := conditionRe.FindStringSubmatch(strings.TrimSpace(s.When)); m != nil {
				segs := model.PathSegments(m[1])
				if len(segs) >= 2 && segs[0] == "steps" {
					deps[s.ID][segs[1]] = true
				}
			} else {
				segs := model.PathSegments(strings.TrimSpace(s.When))
				if len(segs) >= 2 && segs[0] == "steps" {
					deps[s.ID][segs[1]] = true
				}
			}
		}
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var cyclic []string
	var visit func(id string, stack []string) bool
	visit = func(id string, stack []string) bool {
		color[id] = gray
		for dep := range deps[id] {
			switch color[dep] {
			case gray:
				cyclic = append(cyclic, strings.Join(append(stack, id, dep), " -> "))
				return true
			case white:
				if visit(dep, append(stack, id)) {
					return true
				}
			}
		}
		color[id] = black
		return false
	}
	for _, s := range w.Steps {
		if color[s.ID] == white {
			visit(s.ID, nil)
		}
	}
	for _, c := range cyclic {
		out = append(out, diag.Error(diag.ClassSchema, "dependency-cycle", entry+"#steps",
			"step dependency cycle: "+c, "break the cycle; the executor cannot schedule a cyclic DAG"))
	}
	return out
}

// checkInputSchemaOverlap is a soft cross-check between workflow.yaml
// inputs and the manifest's inputSchema properties: every workflow input
// name should appear in inputSchema (advisory only -- the two documents
// use different type vocabularies, so this is a warning, not an error).
func checkInputSchemaOverlap(pkg *model.Package) diag.Findings {
	var out diag.Findings
	if pkg.Workflow == nil {
		return out
	}
	_, schema, err := loadInputSchemaRaw(pkg)
	if err != nil || schema == nil {
		return out
	}
	props, _ := model.AsMap(schema["properties"])
	for name := range pkg.Workflow.Inputs {
		if props != nil {
			if _, ok := props[name]; !ok {
				out = append(out, diag.Warn(diag.ClassSchema, "input-not-in-schema", "workflow.yaml#inputs."+name,
					fmt.Sprintf("workflow input %q has no matching property in the manifest inputSchema", name), ""))
			}
		}
	}
	return out
}

// checkUnusedInputs is the reverse-of-checkInputSchemaOverlap lint
// (CHANGELOG.md v1 CLI fix item 4 / STATUS.md G0 agenda item 7): a declared
// workflow input, or a manifest inputSchema property, that no step and no
// output references anywhere is a dead declaration -- the "never invent"
// rule (SKILL.md) cuts both ways: every declared input must be wired. This
// is a warning, not an error (A3.0's regen-acceptance run showed the ground
// truth package shipping exactly this kind of dead input and still
// validating clean today).
func checkUnusedInputs(pkg *model.Package, w *model.Workflow, entry string) diag.Findings {
	var out diag.Findings
	referenced := map[string]bool{}
	var refs []fromRef
	for _, s := range w.Steps {
		scanBindingValue(s.Raw, "", &refs)
		if s.HasWhen {
			refs = append(refs, whenConditionRefs(s.When)...)
		}
		if s.Branch != nil {
			for _, c := range s.Branch.Cases {
				refs = append(refs, whenConditionRefs(c.When)...)
			}
		}
	}
	for _, v := range w.Outputs {
		scanBindingValue(v, "", &refs)
	}
	for _, r := range refs {
		segs := model.PathSegments(r.Path)
		if len(segs) >= 2 && segs[0] == "inputs" {
			referenced[segs[1]] = true
		}
	}

	declared := map[string]bool{}
	for name := range w.Inputs {
		declared[name] = true
	}
	if _, schema, err := loadInputSchemaRaw(pkg); err == nil && schema != nil {
		if props, ok := model.AsMap(schema["properties"]); ok {
			for name := range props {
				declared[name] = true
			}
		}
	}

	declaredNames := make([]string, 0, len(declared))
	for name := range declared {
		declaredNames = append(declaredNames, name)
	}
	sort.Strings(declaredNames)
	for _, name := range declaredNames {
		// `now` is the reserved, runtime-auto-injected input (ruling 3): it
		// is legitimately usable via {from: inputs.now} without ever being
		// declared, so an unreferenced declaration of it is not dead in the
		// same sense as any other input.
		if name == "now" {
			continue
		}
		if !referenced[name] {
			out = append(out, diag.Warn(diag.ClassSchema, "unused-input", fmt.Sprintf("%s#inputs.%s", entry, name),
				fmt.Sprintf("input %q is declared (workflow input or inputSchema property) but no step or output references it", name),
				"wire it into a step/output, or remove the declaration -- every declared input must be wired (SKILL.md invent-vs-generalize rule)"))
		}
	}
	return out
}

// whenConditionRefs extracts the from-ref(s) implied by a when:/branch-case
// condition string: either the LHS of an `==`/`!=` comparison, or the whole
// string for bare truthiness.
func whenConditionRefs(cond string) []fromRef {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return nil
	}
	if m := conditionRe.FindStringSubmatch(cond); m != nil {
		return []fromRef{{Path: m[1]}}
	}
	return []fromRef{{Path: cond}}
}

func loadInputSchemaRaw(pkg *model.Package) (map[string]interface{}, map[string]interface{}, error) {
	ref := pkg.Manifest.InputSchemaPath()
	inline := pkg.Manifest.Interface.InputSchemaInline
	if ref == "" && inline == nil {
		return nil, nil, fmt.Errorf("no input schema")
	}
	var data []byte
	var err error
	if ref != "" {
		data, err = os.ReadFile(ref)
		if err != nil {
			return nil, nil, err
		}
	}
	if data == nil {
		return nil, inline, nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, err
	}
	return raw, raw, nil
}
