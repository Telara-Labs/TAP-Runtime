package redact

import "testing"

func TestFieldRedactionIsIdempotent(t *testing.T) {
	cases := []string{
		"password: |\n  syntheticOpaqueValue42\n",
		"password: '<redacted credential>'",
		"password: '[REDACTED: sensitive value]'",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			once := Field(nil, input)
			twice := Field(nil, once)
			if twice != once {
				t.Fatalf("Field is not idempotent: once %q, twice %q", once, twice)
			}
		})
	}
}
