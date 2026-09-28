package testrunner

import "testing"

func TestEvalAssert_Contains(t *testing.T) {
	output := map[string]interface{}{
		"jobs": []interface{}{
			map[string]interface{}{"name": "a", "category": "infra_flake"},
			map[string]interface{}{"name": "b", "category": "code_failure"},
		},
	}
	if err := evalAssert(output, `jobs contains {name: a, category: infra_flake}`); err != nil {
		t.Fatalf("expected match, got %v", err)
	}
	if err := evalAssert(output, `jobs NOT contains {name: c}`); err != nil {
		t.Fatalf("expected NOT-contains to hold, got %v", err)
	}
	if err := evalAssert(output, `jobs contains {name: nonexistent}`); err == nil {
		t.Fatalf("expected contains failure for nonexistent name")
	}
	if err := evalAssert(output, `jobs NOT contains {name: a}`); err == nil {
		t.Fatalf("expected NOT-contains failure since 'a' is present")
	}
}

func TestEvalAssert_SortedDescendingUndatedLast(t *testing.T) {
	output := map[string]interface{}{
		"entries": []interface{}{
			map[string]interface{}{"date": "2026-07-08"},
			map[string]interface{}{"date": "2026-06-19"},
			map[string]interface{}{"date": nil},
		},
	}
	if err := evalAssert(output, "entries sorted by date descending, undated last"); err != nil {
		t.Fatalf("expected sorted order to hold, got %v", err)
	}

	bad := map[string]interface{}{
		"entries": []interface{}{
			map[string]interface{}{"date": nil},
			map[string]interface{}{"date": "2026-07-08"},
		},
	}
	if err := evalAssert(bad, "entries sorted by date descending, undated last"); err == nil {
		t.Fatalf("expected failure when a dated entry follows an undated one")
	}
}

// TestEvalAssert_SortedDescending_NumericAware is the CHANGELOG.md v1 CLI
// fix item 2 regression (STATUS.md A3.1 finding): a plain string compare
// misorders mixed digit widths ("9" > "10" lexically, backwards
// numerically). Fields that parse as numbers on both sides must compare
// numerically; the date-string case above must keep working unchanged.
func TestEvalAssert_SortedDescending_NumericAware(t *testing.T) {
	output := map[string]interface{}{
		"queue": []interface{}{
			map[string]interface{}{"urgency_score": 10},
			map[string]interface{}{"urgency_score": 9},
			map[string]interface{}{"urgency_score": 2},
		},
	}
	if err := evalAssert(output, "queue sorted by urgency_score descending, undated last"); err != nil {
		t.Fatalf("expected numeric-descending order to hold (10, 9, 2), got %v", err)
	}

	// A naive string compare would consider "9" < "10" reversed (i.e. see
	// "10" as smaller than "9"), so 9-then-10 (ascending numerically, which
	// IS descending-violating) must fail either way, but the interesting
	// regression case is that 10-then-9 (numerically descending, correct)
	// must NOT fail just because "10" < "9" as strings.
	badNumeric := map[string]interface{}{
		"queue": []interface{}{
			map[string]interface{}{"urgency_score": 2},
			map[string]interface{}{"urgency_score": 9},
		},
	}
	if err := evalAssert(badNumeric, "queue sorted by urgency_score descending, undated last"); err == nil {
		t.Fatalf("expected ascending numeric order to fail the descending assertion")
	}

	// float64-vs-int kind tolerance (YAML/JSON decoders differ).
	mixedKinds := map[string]interface{}{
		"queue": []interface{}{
			map[string]interface{}{"urgency_score": 10.0},
			map[string]interface{}{"urgency_score": 9},
		},
	}
	if err := evalAssert(mixedKinds, "queue sorted by urgency_score descending, undated last"); err != nil {
		t.Fatalf("expected int/float64 numeric comparison to hold, got %v", err)
	}
}

func TestGetPath(t *testing.T) {
	root := map[string]interface{}{
		"summary": map[string]interface{}{"verdict": "green"},
		"entries": []interface{}{
			map[string]interface{}{"version": "4.2.0"},
		},
	}
	if v, ok := GetPath(root, "summary.verdict"); !ok || v != "green" {
		t.Fatalf("summary.verdict = %v, %v", v, ok)
	}
	if v, ok := GetPath(root, "entries.length"); !ok || v != 1 {
		t.Fatalf("entries.length = %v, %v", v, ok)
	}
	if v, ok := GetPath(root, "entries.0.version"); !ok || v != "4.2.0" {
		t.Fatalf("entries.0.version = %v, %v", v, ok)
	}
	if _, ok := GetPath(root, "entries.5.version"); ok {
		t.Fatalf("expected out-of-range index to miss")
	}
}

func TestDeepEqualLoose_NumberKindTolerance(t *testing.T) {
	if !DeepEqualLoose(2, 2.0) {
		t.Fatalf("expected int/float64 tolerance")
	}
	if !DeepEqualLoose([]interface{}{}, []interface{}{}) {
		t.Fatalf("expected empty slices equal")
	}
	if DeepEqualLoose(2, 3) {
		t.Fatalf("expected mismatch")
	}
}
