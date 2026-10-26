package redact

import (
	"strings"
	"testing"
)

func TestPreserveTextFloorAfterComposition(t *testing.T) {
	for _, secret := range []string{"ghp_" + strings.Repeat("A", 36), "glpat-" + strings.Repeat("A", 16), "syntheticOpaqueValue42"} {
		raw := secret
		if secret == "syntheticOpaqueValue42" {
			raw = "api_key=" + secret
		}
		for _, view := range []string{raw, "fixed engine guidance. " + raw} {
			if strings.Contains(PreserveTextFloor(raw, view), secret) {
				t.Fatal("composed credential escaped the unconditional floor")
			}
		}
	}
	for _, ordinary := range []string{"call telara_task_create first", "properties:\n  password: {type: string, description: ordinary catalog prose}\n", `{"properties":{"password":{"type":"string","description":"ordinary catalog prose"}}}`} {
		if got := PreserveTextFloor("upstream failed: "+ordinary, ordinary); got != ordinary {
			t.Fatalf("display context reclassified ordinary source %q", got)
		}
	}
	source := "api_key=syntheticOpaqueValue42"
	if got := PreserveTextFloor(source, "value was syntheticOpaqueValue42"); strings.Contains(got, "syntheticOpaqueValue42") {
		t.Fatal("source decision lost after formatting")
	}
}

func TestComposedValuesRetainOriginalAssignmentContext(t *testing.T) {
	ordinary := "next_page_token:next_page_token next_page_token"
	if got := RedactComposedValues(ordinary); got != ordinary {
		t.Fatalf("composition invented a credential assignment %q", got)
	}
	for _, secret := range []string{"ghp_" + strings.Repeat("A", 36), "glpat-" + strings.Repeat("A", 16), "Bearer " + strings.Repeat("A", 16)} {
		if strings.Contains(RedactComposedValues(secret), secret) {
			t.Fatal("reconstructed value escaped")
		}
	}
	if got := RedactComposedValues(strings.Repeat("ghp_"+strings.Repeat("A", 36)+" ", 5000)); got != "<redacted inspection limit>" {
		t.Fatal("composition match bound not enforced")
	}
}
