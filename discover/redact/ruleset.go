package redact

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/Telara-Labs/TAP-Runtime/contract/canon"
)

// RulesetEpoch numbers the redaction rules. It goes up by one whenever a
// rule changes what is redacted, so a record can say which rules redacted
// it and a store can tell what to redact again. TestRulesetDigestNeedsAnEpoch
// fails when the rules change and the epoch does not.
const RulesetEpoch = 46

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
// references, the semantic word tables, and outputs of the behavior corpus.
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
	for i, re := range gatewayCredentialPatterns {
		out = append(out, rule{"frozen gateway credential " + strconv.Itoa(i), re.String()})
	}
	for _, sh := range SecretShapes {
		out = append(out, rule{sh.name, sh.re.String()})
	}
	return append(out,
		rule{"metadata email", metadataEmail.String()},
		rule{"metadata IPv4", metadataIPv4.String()},
		rule{"metadata IPv6", metadataIPv6.String()},
		rule{"metadata webhook URL", metadataURL.String()},
		rule{"metadata Groq", metadataGroq.String()},
		rule{"profiles", strings.Join((Ruleset{}).Profiles(), ",")},
		rule{"sensitive name", SensitiveName.String()},
		rule{"mixed credential name", mixedCredentialName.String()},
		rule{"credential class metadata", credentialClassName.String()},
		rule{"assignment header", assignmentHead.String()},
		rule{"assignment value", assignmentValue.String()},
		rule{"following mapping field", followingMappingField.String()},
		rule{"yaml mapping head", yamlMappingHead.String()},
		rule{"yaml flow head", yamlFlowHead.String()},
		rule{"schema value keywords", strings.Join(schemaValueKeywords, ",")},
		rule{"schema child keywords", strings.Join(schemaChildKeywords, ",")},
		rule{"schema property keywords", strings.Join(schemaPropertyKeywords, ",")},
		rule{"schema metadata keywords", strings.Join(schemaMetadataKeywords, ",")},
		rule{"OAuth flow names", strings.Join(oauthFlowNames, ",")},
		rule{"OAuth bearer methods", strings.Join(oauthBearerMethods, ",")},
		rule{"variable reference", reference.String()},
		rule{"raw variable reference", referencePrefix.String()},
		rule{"quoted field", quotedField.String()},
		rule{"qualified credential name", qualifiedCredentialName.String()},
		rule{"github schema token", githubIdentifierToken.String()},
		rule{"query name", queryName.String()},
		rule{"query value", queryValue.String()},
		rule{"signed query name", signedQueryName.String()},
		rule{"external identifier", secretIdentifier.String()},
		rule{"display marker", redactionMarker.String()},
		rule{"inspection marker", inspectionMarkerPattern.String()},
	)
}

func rulesDigest() string {
	var corpus []struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(behaviorCorpus, &corpus); err != nil {
		panic(err)
	}
	behavior := make([]string, 0, len(corpus))
	for _, c := range corpus {
		behavior = append(behavior, Argument(c.Name, c.Input))
	}
	b, err := canon.CanonicalValue(struct {
		Rules                                []rule
		Credentials, Configuration, Behavior []string
		SharedCorpus                         []CorpusCase
		SharedBehavior                       []string
		Engine                               int
	}{rules(), credentialWords, configurationWords, behavior, Corpus(), sharedBehavior(), 37})
	if err != nil {
		panic("redact: rules do not encode: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

//go:embed testdata/behavior.json
var behaviorCorpus []byte

// Include both public views and normalization in the fingerprint. Changes to
// embedded expectations also require a new epoch even if outputs are unchanged.
func sharedBehavior() []string {
	var out []string
	for _, c := range Corpus() {
		for _, profile := range (Ruleset{}).Profiles() {
			out = append(out, (Ruleset{}).RedactProfile(profile, c.Input))
		}
		out = append(out, Field(nil, c.Input), Field([]string{"password", "value"}, c.Input), ExternalField([]string{"password", "value"}, c.Input), Field([]string{"properties", "password", "default"}, c.Input), Field([]string{"properties", "password", "description"}, c.Input), Field([]string{"default"}, c.Input))
		out = append(out, Normalize(c.Input), Redact(c.Input), ExternalText(c.Input), SecretShape(c.Input), strconv.FormatBool(ContainsProviderToken(c.Input)), strconv.FormatBool(len(ScanArtifacts(map[string][]byte{"corpus": []byte(c.Input)})) != 0))
	}
	return out
}
