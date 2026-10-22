package redact

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The digest is recorded for every epoch. A rule changed without an epoch
// bump fails here; so does a new epoch not yet recorded.
func TestRulesetDigestNeedsAnEpoch(t *testing.T) {
	b, err := os.ReadFile("testdata/ruleset.golden")
	if err != nil {
		t.Fatal(err)
	}
	recorded, latest := map[int]string{}, 0
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.HasPrefix(line, "#") {
			continue
		}
		e, err := strconv.Atoi(f[0])
		if err != nil {
			t.Fatalf("golden line %q", line)
		}
		recorded[e], latest = f[1], max(latest, e)
	}
	rs := CurrentRuleset()
	want, ok := recorded[rs.Epoch]
	switch {
	case !ok:
		t.Fatalf("epoch %d is not recorded: append %q to testdata/ruleset.golden", rs.Epoch, strconv.Itoa(rs.Epoch)+" "+rs.Digest)
	case want != rs.Digest:
		t.Fatalf("the redaction rules changed under epoch %d: bump RulesetEpoch and record its digest %s", rs.Epoch, rs.Digest)
	case rs.Epoch != latest:
		t.Fatalf("epoch %d is behind the recorded epoch %d", rs.Epoch, latest)
	}
}

// The digest comes from the rules: any change to a shape, its name, or the
// name rules changes it.
func TestRulesetDigestFollowsTheRules(t *testing.T) {
	base := rulesDigest()
	if len(base) != 64 || base != CurrentRuleset().Digest {
		t.Fatalf("digest %q", base)
	}
	saved := SecretShapes
	t.Cleanup(func() { SecretShapes = saved })
	for name, change := range map[string]func(){
		"added shape": func() {
			SecretShapes = append(append(SecretShapes[:0:0], saved...), struct {
				name string
				re   *regexp.Regexp
			}{"example", regexp.MustCompile(`\bexample_[a-z]{8}`)})
		},
		"renamed shape": func() {
			SecretShapes = append(SecretShapes[:0:0], saved...)
			SecretShapes[0].name += "x"
		},
		"changed pattern": func() {
			SecretShapes = append(SecretShapes[:0:0], saved...)
			SecretShapes[0].re = regexp.MustCompile(SecretShapes[0].re.String() + "x")
		},
	} {
		change()
		if rulesDigest() == base {
			t.Errorf("%s: digest unchanged", name)
		}
		SecretShapes = saved
	}
	if rulesDigest() != base {
		t.Fatal("digest is not stable")
	}
}
