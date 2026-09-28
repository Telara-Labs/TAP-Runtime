package testrunner

import (
	"fmt"
	"strings"

	"telara.dev/tap/internal/model"
)

// EvalContext holds resolved input values and per-step outputs so `from:`
// bindings can be resolved during execution, mirroring 09 §5's binding
// dialect (inputs.x / steps.id.field...).
type EvalContext struct {
	Inputs map[string]interface{}
	Steps  map[string]interface{} // step id -> raw output (nil if skipped)
}

func NewEvalContext(inputs map[string]interface{}) *EvalContext {
	return &EvalContext{Inputs: inputs, Steps: map[string]interface{}{}}
}

// Resolve looks up a `from:` path against the context.
func (ctx *EvalContext) Resolve(path string) (interface{}, error) {
	segs := strings.Split(path, ".")
	if len(segs) == 0 {
		return nil, fmt.Errorf("empty from: path")
	}
	switch segs[0] {
	case "inputs":
		if len(segs) < 2 {
			return nil, fmt.Errorf("from: %q missing input name", path)
		}
		v, ok := ctx.Inputs[segs[1]]
		if !ok {
			return nil, nil // absent optional input resolves to nil, not an error at runtime
		}
		if len(segs) == 2 {
			return v, nil
		}
		out, ok := GetPath(v, strings.Join(segs[2:], "."))
		if !ok {
			return nil, nil
		}
		return out, nil
	case "steps":
		if len(segs) < 2 {
			return nil, fmt.Errorf("from: %q missing step id", path)
		}
		v, ok := ctx.Steps[segs[1]]
		if !ok || v == nil {
			return nil, nil // step skipped (when: false) or not yet run: resolves to null (09 SPEC-FEEDBACK #3)
		}
		if len(segs) == 2 {
			return v, nil
		}
		out, ok := GetPath(v, strings.Join(segs[2:], "."))
		if !ok {
			return nil, nil
		}
		return out, nil
	default:
		return nil, fmt.Errorf("from: %q must start with inputs. or steps.", path)
	}
}

// ResolveValue resolves a raw params-map value: a `{from: ...}` binding, or
// a literal pass-through (09 §5: "literal wins over expression").
func (ctx *EvalContext) ResolveValue(v interface{}) (interface{}, error) {
	if from, ok := model.ParseFrom(v); ok {
		return ctx.Resolve(from)
	}
	return v, nil
}

// ResolveParams resolves every entry in a plain (non-file-ref) params map.
func (ctx *EvalContext) ResolveParams(params map[string]interface{}) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for k, v := range params {
		rv, err := ctx.ResolveValue(v)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", k, err)
		}
		out[k] = rv
	}
	return out, nil
}
