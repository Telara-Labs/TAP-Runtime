// Package star executes TAP transform: steps' Starlark programs, mirroring
// the platform's Starlark sandbox convention (03 §8.1 / 09 §7 T1): params
// arrive as predeclared globals, no I/O or imports are exposed, and the
// value assigned to `result` is the step's output.
package star

import (
	"fmt"
	"math/big"
	"sort"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
)

// MaxExecutionSteps mirrors the "2M-step cap" the platform sandbox
// documents (09 §7 T1).
const MaxExecutionSteps = 2_000_000

func init() {
	// Both example packages' transforms reassign a top-level variable
	// sequentially (e.g. web-changelog-watch's normalize.star reassigns
	// `entries` after its initial binding) -- a normal, deterministic
	// pattern for a pure transform script, not a hermeticity concern (there
	// is still no I/O, no imports, no mutable shared state across calls:
	// each Exec gets a fresh Thread and fresh globals). go.starlark.net
	// disables this by default; tap's sandbox profile re-enables it.
	resolve.AllowGlobalReassign = true
}

// Exec runs a Starlark program with the given predeclared globals and
// returns the Go-native value of the `result` global. No filesystem,
// network, or import access is provided -- Starlark is sandboxed by
// construction (no such builtins are registered).
func Exec(filename, program string, globals map[string]interface{}) (interface{}, error) {
	predeclared := starlark.StringDict{}
	for k, v := range globals {
		sv, err := ToStarlark(v)
		if err != nil {
			return nil, fmt.Errorf("global %q: %w", k, err)
		}
		predeclared[k] = sv
	}

	thread := &starlark.Thread{Name: "tap-transform"}
	thread.SetMaxExecutionSteps(MaxExecutionSteps)

	out, err := starlark.ExecFile(thread, filename, program, predeclared)
	if err != nil {
		if evalErr, ok := err.(*starlark.EvalError); ok {
			return nil, fmt.Errorf("%s: %s", filename, evalErr.Backtrace())
		}
		return nil, fmt.Errorf("%s: %w", filename, err)
	}

	result, ok := out["result"]
	if !ok {
		return nil, fmt.Errorf("%s: program did not assign a `result` global", filename)
	}
	return FromStarlark(result)
}

// ToStarlark converts a Go value (as produced by YAML/JSON decoding) into a
// starlark.Value.
func ToStarlark(v interface{}) (starlark.Value, error) {
	switch tv := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(tv), nil
	case string:
		return starlark.String(tv), nil
	case int:
		return starlark.MakeInt(tv), nil
	case int64:
		return starlark.MakeInt64(tv), nil
	case float64:
		if tv == float64(int64(tv)) {
			return starlark.MakeInt64(int64(tv)), nil
		}
		return starlark.Float(tv), nil
	case []interface{}:
		elems := make([]starlark.Value, 0, len(tv))
		for _, e := range tv {
			sv, err := ToStarlark(e)
			if err != nil {
				return nil, err
			}
			elems = append(elems, sv)
		}
		return starlark.NewList(elems), nil
	case map[string]interface{}:
		d := starlark.NewDict(len(tv))
		keys := make([]string, 0, len(tv))
		for k := range tv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sv, err := ToStarlark(tv[k])
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(starlark.String(k), sv); err != nil {
				return nil, err
			}
		}
		return d, nil
	default:
		return nil, fmt.Errorf("unsupported Go type %T for starlark conversion", v)
	}
}

// FromStarlark converts a starlark.Value back into a Go-native value
// (nil/bool/int64/float64/string/[]interface{}/map[string]interface{}) so
// it can be JSON-marshaled or fed to further steps.
func FromStarlark(v starlark.Value) (interface{}, error) {
	switch tv := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(tv), nil
	case starlark.String:
		return string(tv), nil
	case starlark.Int:
		if i, ok := tv.Int64(); ok {
			return i, nil
		}
		bf := new(big.Float).SetInt(tv.BigInt())
		out, _ := bf.Float64()
		return out, nil
	case starlark.Float:
		return float64(tv), nil
	case *starlark.List:
		out := make([]interface{}, 0, tv.Len())
		for i := 0; i < tv.Len(); i++ {
			e, err := FromStarlark(tv.Index(i))
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case starlark.Tuple:
		out := make([]interface{}, 0, len(tv))
		for _, e := range tv {
			ev, err := FromStarlark(e)
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
		return out, nil
	case *starlark.Dict:
		out := map[string]interface{}{}
		for _, item := range tv.Items() {
			k, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("dict key %v is not a string", item[0])
			}
			ev, err := FromStarlark(item[1])
			if err != nil {
				return nil, err
			}
			out[k] = ev
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported starlark type %s for result conversion", v.Type())
	}
}
