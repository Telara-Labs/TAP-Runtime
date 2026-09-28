package star

import (
	"strings"
	"testing"
)

func TestExec_BasicGlobalsAndResult(t *testing.T) {
	globals := map[string]interface{}{
		"jobs": []interface{}{
			map[string]interface{}{"name": "a", "status": "failed"},
			map[string]interface{}{"name": "b", "status": "success"},
		},
	}
	program := `
failed = [j for j in jobs if j["status"] == "failed"]
result = {"failed_count": len(failed), "names": [j["name"] for j in failed]}
`
	out, err := Exec("t.star", program, globals)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map result, got %T", out)
	}
	if m["failed_count"] != int64(1) {
		t.Fatalf("failed_count = %v, want 1", m["failed_count"])
	}
	names, ok := m["names"].([]interface{})
	if !ok || len(names) != 1 || names[0] != "a" {
		t.Fatalf("names = %v", m["names"])
	}
}

func TestExec_GlobalReassignmentAllowed(t *testing.T) {
	// Mirrors web-changelog-watch/src/normalize.star's pattern of
	// reassigning a top-level variable after its initial binding.
	program := `
entries = [1, 2, 3]
entries = [e for e in entries if e > 1]
result = entries
`
	out, err := Exec("t.star", program, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	list, ok := out.([]interface{})
	if !ok || len(list) != 2 {
		t.Fatalf("result = %v", out)
	}
}

func TestExec_NoResultGlobalIsAnError(t *testing.T) {
	_, err := Exec("t.star", "x = 1\n", nil)
	if err == nil || !strings.Contains(err.Error(), "did not assign a `result` global") {
		t.Fatalf("expected missing-result error, got %v", err)
	}
}

func TestExec_NoFilesystemOrNetworkBuiltins(t *testing.T) {
	// Sandboxed by construction: no I/O builtins are registered.
	for _, prog := range []string{
		"result = open('/etc/passwd')\n",
		"result = fail('nope')\n" + "load('foo.star')\n",
	} {
		_, err := Exec("t.star", prog, nil)
		if err == nil {
			t.Fatalf("expected error for program %q, got none", prog)
		}
	}
}

func TestExec_DictGetAndIndexing(t *testing.T) {
	globals := map[string]interface{}{
		"job": map[string]interface{}{"name": "unit-tests", "stage": "test"},
	}
	program := `
result = {"name": job["name"], "missing": job.get("nope", "default")}
`
	out, err := Exec("t.star", program, globals)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	m := out.(map[string]interface{})
	if m["name"] != "unit-tests" || m["missing"] != "default" {
		t.Fatalf("unexpected result: %v", m)
	}
}

func TestExec_StepCeilingEnforced(t *testing.T) {
	program := `
x = 0
for i in range(10000000):
    x += 1
result = x
`
	_, err := Exec("t.star", program, nil)
	if err == nil {
		t.Fatalf("expected step-ceiling cancellation for a 10M-iteration loop")
	}
}

func TestToFromStarlarkRoundTrip(t *testing.T) {
	in := map[string]interface{}{
		"s":     "hello",
		"n":     int64(42),
		"f":     3.5,
		"b":     true,
		"nil":   nil,
		"list":  []interface{}{"a", int64(1), false},
		"inner": map[string]interface{}{"k": "v"},
	}
	sv, err := ToStarlark(in)
	if err != nil {
		t.Fatalf("ToStarlark: %v", err)
	}
	out, err := FromStarlark(sv)
	if err != nil {
		t.Fatalf("FromStarlark: %v", err)
	}
	m := out.(map[string]interface{})
	if m["s"] != "hello" || m["n"] != int64(42) || m["f"] != 3.5 || m["b"] != true || m["nil"] != nil {
		t.Fatalf("round trip mismatch: %#v", m)
	}
}
