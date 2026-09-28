package compile

import (
	"strings"

	"telara.dev/tap/internal/model"
)

// RewriteExpression maps an authoring `from:` path onto the executor's real
// expression dialect (09 §5, verified against resolveWorkflowExpression in
// workflow_dag_executor.go):
//
//	inputs.x                -> trigger.x        (workflow inputs arrive as trigger data)
//	steps.fetch.items.0.id  -> nodes.fetch.result.items.0.id
//	steps.fetch             -> nodes.fetch.result
//
// A tool node's raw output is {status, request_id, result: {...}}, so every
// step reference gains the `.result` hop (confirmed at executor lines 610-617);
// transform/model nodes emit their payload under the same `result` key by
// construction. The compiler NEVER emits workflow.* / outputs.* (accidental
// executor aliases) or {{ }} wrappers.
func RewriteExpression(fromPath string) string {
	segs := model.PathSegments(fromPath)
	if len(segs) == 0 {
		return fromPath
	}
	switch segs[0] {
	case "inputs":
		return "trigger." + strings.Join(segs[1:], ".")
	case "steps":
		if len(segs) == 1 {
			return fromPath // malformed; validate already rejected
		}
		id := segs[1]
		rest := segs[2:]
		out := "nodes." + id + ".result"
		if len(rest) > 0 {
			out += "." + strings.Join(rest, ".")
		}
		return out
	default:
		return fromPath
	}
}

// RewriteCondition rewrites a when:/branch-case condition (09 §6 grammar:
// `<path> ==|!= <literal>` or bare truthiness) into the executor's condition
// dialect by rewriting only the left-hand path. The literal RHS is passed
// through verbatim (the executor does a stringified comparison; no numeric
// coercion), and bare-truthiness rewrites the whole path.
func RewriteCondition(cond string) string {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return cond
	}
	for _, op := range []string{"==", "!="} {
		if idx := strings.Index(cond, op); idx >= 0 {
			lhs := strings.TrimSpace(cond[:idx])
			rhs := strings.TrimSpace(cond[idx+len(op):])
			return RewriteExpression(lhs) + " " + op + " " + rhs
		}
	}
	return RewriteExpression(cond)
}

// splitParams walks a params map and separates it into static parameters
// (literals, emitted as literal-value input bindings so a non-tool node also
// receives them as globals) and dynamic input bindings (`from:` refs, emitted
// as source_expression bindings). Nested composites carrying a `from:` deeper
// than the top level (e.g. {updated_after: {days_ago: {from: ...}}}, 09 §5)
// are flattened onto dot target-paths — the executor's setWorkflowPath builds
// the nested map at runtime. Literal leaves accumulate onto the static tree.
//
// It returns:
//   - statics: the literal-only subtree of params (nil if empty)
//   - bindings: one binding per `from:` ref found at any depth
func splitParams(params map[string]interface{}) (statics map[string]interface{}, bindings []Binding) {
	if len(params) == 0 {
		return nil, nil
	}
	statics = map[string]interface{}{}
	for _, key := range sortedKeys(params) {
		v := params[key]
		lit, bs := splitValue(key, v)
		if lit != nil || isPresentLiteral(v, bs) {
			if lit != nil {
				statics[key] = lit
			}
		}
		bindings = append(bindings, bs...)
	}
	if len(statics) == 0 {
		statics = nil
	}
	return statics, bindings
}

// isPresentLiteral reports whether a value contributed a literal even when the
// literal is a falsy zero (0/false/""), so those are not silently dropped.
func isPresentLiteral(v interface{}, bs []Binding) bool {
	if _, ok := model.ParseFrom(v); ok {
		return false
	}
	switch v.(type) {
	case map[string]interface{}:
		return len(bs) == 0
	default:
		return true
	}
}

// splitValue returns (literalPart, bindings) for a single param value rooted at
// targetPath. A `{from:}` map yields a single binding and no literal; a plain
// map recurses per key (accumulating a literal submap and nested bindings); a
// scalar/list is a literal leaf.
func splitValue(targetPath string, v interface{}) (interface{}, []Binding) {
	if from, ok := model.ParseFrom(v); ok {
		return nil, []Binding{{
			Name:             targetPath,
			TargetPath:       targetPath,
			SourceExpression: RewriteExpression(from),
		}}
	}
	if m, ok := v.(map[string]interface{}); ok {
		var bindings []Binding
		lit := map[string]interface{}{}
		for _, k := range sortedKeys(m) {
			sub, bs := splitValue(targetPath+"."+k, m[k])
			if sub != nil {
				lit[k] = sub
			}
			bindings = append(bindings, bs...)
		}
		if len(lit) == 0 {
			return nil, bindings
		}
		return lit, bindings
	}
	return v, nil
}
