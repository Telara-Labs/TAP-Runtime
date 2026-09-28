package model

import "testing"

func TestParseFrom(t *testing.T) {
	if from, ok := ParseFrom(map[string]interface{}{"from": "inputs.x"}); !ok || from != "inputs.x" {
		t.Fatalf("got %q, %v", from, ok)
	}
	if _, ok := ParseFrom("literal"); ok {
		t.Fatalf("expected literal string to not parse as a from-ref")
	}
	if _, ok := ParseFrom(map[string]interface{}{"other": "x"}); ok {
		t.Fatalf("expected map without 'from' key to not parse")
	}
}

func TestFileRefParams(t *testing.T) {
	plain, fileRefs := FileRefParams(map[string]interface{}{
		"signatures_from": "src/signatures.yaml",
		"jobs":            map[string]interface{}{"from": "steps.jobs.items"},
		"cap":             20,
	})
	if fileRefs["signatures"] != "src/signatures.yaml" {
		t.Fatalf("fileRefs = %v", fileRefs)
	}
	if len(plain) != 2 {
		t.Fatalf("plain = %v", plain)
	}
	if _, ok := plain["signatures_from"]; ok {
		t.Fatalf("signatures_from should not remain in plain params")
	}
}

func TestUnknownKeys(t *testing.T) {
	m := map[string]interface{}{"a": 1, "b": 2, "c": 3}
	got := UnknownKeys(m, []string{"a", "b"})
	if len(got) != 1 || got[0] != "c" {
		t.Fatalf("got %v", got)
	}
}
