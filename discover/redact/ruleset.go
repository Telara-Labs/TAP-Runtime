package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/Telara-Labs/TAP-Runtime/contract/canon"
)

// RulesetEpoch numbers the redaction rules. It goes up by one whenever a
// rule changes what is redacted, so a record can say which rules redacted
// it and a store can tell what to redact again. TestRulesetDigestNeedsAnEpoch
// fails when the rules change and the epoch does not.
const RulesetEpoch = 1

// Ruleset identifies the redaction rules in force: their epoch, and a digest
// derived from the rules themselves (never typed in), so two programs that
// claim the same epoch can check they hold the same rules.
type Ruleset struct {
	Epoch  int    `json:"epoch"`
	Digest string `json:"digest"`
}

var (
	rulesetOnce sync.Once
	ruleset     Ruleset
)

// CurrentRuleset is the ruleset this package applies. Its digest is the
// hex SHA-256 of the RFC 8785 encoding of every rule's name and pattern, in
// the order they are applied: the value shapes (SecretShapes), then the
// patterns that recognise credential names, assignments and variable
// references.
func CurrentRuleset() Ruleset {
	rulesetOnce.Do(func() {
		ruleset = Ruleset{Epoch: RulesetEpoch, Digest: rulesDigest()}
	})
	return ruleset
}

type rule struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

func rules() []rule {
	var out []rule
	for _, sh := range SecretShapes {
		out = append(out, rule{sh.name, sh.re.String()})
	}
	return append(out,
		rule{"sensitive name", SensitiveName.String()},
		rule{"assignment word", assignmentWord.String()},
		rule{"variable reference", reference.String()},
	)
}

func rulesDigest() string {
	b, err := canon.CanonicalValue(rules())
	if err != nil {
		panic("redact: rules do not encode: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
