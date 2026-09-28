// Package testrunner implements `tap test`: it executes tests/contract.test.yaml
// offline, replaying fixtures for api/browser steps, actually running
// Starlark transforms via go.starlark.net, honoring `when:` conditions,
// `lease_mock:`, and `fixture_variant:`, and asserting the `expect:` blocks.
package testrunner

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	"telara.dev/tap/internal/model"
)

type OverrideStep struct {
	ID         string
	EgressHost string
}

type Expectation struct {
	Raw map[string]interface{}
}

func (e Expectation) boolField(key string) (bool, bool) {
	v, ok := e.Raw[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func (e Expectation) intField(key string) (int, bool) {
	v, ok := e.Raw[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func (e Expectation) stringField(key string) (string, bool) {
	v, ok := e.Raw[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (e Expectation) outputMap() map[string]interface{} {
	m, _ := model.AsMap(e.Raw["output"])
	return m
}

func (e Expectation) assertList() []string {
	seq, _ := model.AsSlice(e.Raw["assert"])
	out := make([]string, 0, len(seq))
	for _, v := range seq {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

type TestCase struct {
	Name           string
	Fixtures       string
	HasInput       bool
	Input          map[string]interface{}
	LeaseMock      string
	FixtureVariant string
	// Now is the optional `now: <ISO-8601 datetime>` test-case field
	// (CHANGELOG.md v1 ruling 3): pins the reserved, runtime-auto-injected
	// `now` workflow input for this case so deterministic staleness/
	// monitoring logic is testable offline.
	Now          string
	OverrideStep *OverrideStep
	Expect       Expectation
	Raw          map[string]interface{}
}

type ContractFile struct {
	Path     string
	Dir      string
	Cases    []TestCase
	Bindings map[string]string // optional tests-local `bindings:` block, slot -> origin
}

// LoadContractFile parses tests/contract.test.yaml.
//
// FINDING: both example packages' `assert:` lines write unquoted object
// literals inline in a plain scalar, e.g.
//   - failed_jobs contains {name: unit-tests, category: code_failure, ...}
//
// which is not valid YAML in any spec-conformant parser (confirmed against
// both gopkg.in/yaml.v3 and PyYAML: a plain scalar cannot contain a bare
// "key: value" pair once a flow-mapping-shaped brace appears mid-string).
// Rather than editing the ground-truth example files, tap preprocesses only
// these specific "- ... { ... }" sequence-item lines by quoting them, which
// is semantics-preserving for tap's own assert-DSL parser (it strips
// surrounding quotes before matching).
func LoadContractFile(path string) (*ContractFile, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := parseContractYAML(quoteInlineBraceAssertLines(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%s: empty document", path)
	}
	cf := &ContractFile{Path: path}

	if binds, ok := model.AsMap(raw["bindings"]); ok {
		cf.Bindings = map[string]string{}
		for k, v := range binds {
			if s, ok := v.(string); ok {
				cf.Bindings[k] = s
			}
		}
	}

	cases, ok := model.AsSlice(raw["cases"])
	if !ok {
		return nil, fmt.Errorf("%s: no cases: [] found", path)
	}
	for _, cv := range cases {
		cm, ok := model.AsMap(cv)
		if !ok {
			continue
		}
		tc := TestCase{
			Name:           model.StringVal(cm, "name"),
			Fixtures:       model.StringVal(cm, "fixtures"),
			LeaseMock:      model.StringVal(cm, "lease_mock"),
			FixtureVariant: model.StringVal(cm, "fixture_variant"),
			Now:            model.StringVal(cm, "now"),
			Raw:            cm,
		}
		if in, ok := model.AsMap(cm["input"]); ok {
			tc.Input = in
			tc.HasInput = true
		}
		if ov, ok := model.AsMap(cm["override_step"]); ok {
			tc.OverrideStep = &OverrideStep{
				ID:         model.StringVal(ov, "id"),
				EgressHost: model.StringVal(ov, "egress_host"),
			}
		}
		if exp, ok := model.AsMap(cm["expect"]); ok {
			tc.Expect = Expectation{Raw: exp}
		}
		cf.Cases = append(cf.Cases, tc)
	}
	return cf, nil
}

var braceListItemRe = regexp.MustCompile(`^(\s*-\s+)(\S.*\{.*\})\s*$`)

// quoteInlineBraceAssertLines quotes "- <text> {...}" sequence-item lines
// that are not already quoted, so the surrounding document parses as valid
// YAML. See the FINDING on LoadContractFile.
func quoteInlineBraceAssertLines(src []byte) []byte {
	lines := strings.Split(string(src), "\n")
	for i, line := range lines {
		m := braceListItemRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent, content := m[1], m[2]
		if strings.HasPrefix(content, `"`) || strings.HasPrefix(content, "'") {
			continue // already quoted
		}
		escaped := strings.ReplaceAll(content, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		lines[i] = indent + `"` + escaped + `"`
	}
	return []byte(strings.Join(lines, "\n"))
}

func parseContractYAML(src []byte) (map[string]interface{}, error) {
	var out map[string]interface{}
	if err := yaml.Unmarshal(src, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadFixtures loads a fixtures JSON file into a generic map.
func LoadFixtures(path string) (map[string]interface{}, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := unmarshalJSONMap(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}
