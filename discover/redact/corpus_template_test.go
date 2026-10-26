package redact

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestExpandedSyntheticCorpusKeepsOriginalBytes(t *testing.T) {
	const want = "c06aa6f1a75f8ecbf634461a381fd30335f5786c4abac0ce773843578a7ffbe4"
	if got := fmt.Sprintf("%x", sha256.Sum256(CorpusJSON())); got != want {
		t.Fatalf("expanded corpus changed: got %s, want %s", got, want)
	}
	if got := len(Corpus()); got != 903 {
		t.Fatalf("corpus count = %d", got)
	}
}
