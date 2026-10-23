package redact

import (
	"strings"
	"testing"
)

func TestDuplicateStructuredKeysCannotReturnOriginalSecrets(t *testing.T) {
	for _, input := range []string{`{"password":"tiny","password":"$TOKEN"}`, `{"password":"hunter22x","password":"$TOKEN"}`, `{"nested":{"token":"small","token":"$TOKEN"}}`} {
		got := Redact(input)
		for _, secret := range []string{"tiny", "hunter22x", "small"} {
			if strings.Contains(got, secret) {
				t.Fatal("shadowed credential survived", got)
			}
		}
	}
}

func TestRulesetFingerprintIncludesSemanticBehavior(t *testing.T) {
	base := rulesDigest()
	saved := credentialWords
	credentialWords = append(append([]string{}, saved...), "syntheticcredential")
	t.Cleanup(func() { credentialWords = saved })
	if rulesDigest() == base {
		t.Fatal("semantic credential table did not affect digest")
	}
}
