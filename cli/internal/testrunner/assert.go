package testrunner

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// evalAssert evaluates one `assert:` DSL line against the resolved output.
// The DSL is intentionally minimal (contains / NOT contains / a single
// "sorted by <field> descending, undated last" template) -- enough for the
// two golden packages' contract tests, not a general expression language
// (recorded as a DECISION in the final report).
var (
	containsRe = regexp.MustCompile(`^(\S+)\s+(NOT\s+)?contains\s+(\{.*\})\s*$`)
	sortedRe   = regexp.MustCompile(`^(\S+)\s+sorted by (\S+) descending, undated last$`)
)

func evalAssert(output interface{}, line string) error {
	line = strings.TrimSpace(line)
	if m := containsRe.FindStringSubmatch(line); m != nil {
		path, negate, objLiteral := m[1], m[2] != "", m[3]
		var want map[string]interface{}
		if err := yaml.Unmarshal([]byte(objLiteral), &want); err != nil {
			return fmt.Errorf("assert %q: bad object literal: %w", line, err)
		}
		arr, ok := GetPath(output, path)
		if !ok {
			return fmt.Errorf("assert %q: path %s not found in output", line, path)
		}
		list, ok := arr.([]interface{})
		if !ok {
			return fmt.Errorf("assert %q: path %s is not an array", line, path)
		}
		found := containsMatchingElement(list, want)
		if negate && found {
			return fmt.Errorf("assert %q: found a matching element but expected none", line)
		}
		if !negate && !found {
			return fmt.Errorf("assert %q: no element of %s matched %v", line, path, want)
		}
		return nil
	}
	if m := sortedRe.FindStringSubmatch(line); m != nil {
		path, field := m[1], m[2]
		arr, ok := GetPath(output, path)
		if !ok {
			return fmt.Errorf("assert %q: path %s not found", line, path)
		}
		list, ok := arr.([]interface{})
		if !ok {
			return fmt.Errorf("assert %q: path %s is not an array", line, path)
		}
		return assertSortedDescendingUndatedLast(list, field, line)
	}
	return fmt.Errorf("assert %q: unrecognized assertion form", line)
}

func containsMatchingElement(list []interface{}, want map[string]interface{}) bool {
	for _, e := range list {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		match := true
		for k, wv := range want {
			ev, ok := em[k]
			if !ok || !DeepEqualLoose(ev, wv) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func assertSortedDescendingUndatedLast(list []interface{}, field, line string) error {
	seenUndated := false
	var prev string
	var prevNum float64
	prevSet := false
	prevIsNum := false
	for i, e := range list {
		em, ok := e.(map[string]interface{})
		if !ok {
			return fmt.Errorf("assert %q: element %d is not an object", line, i)
		}
		v, present := em[field]
		if !present || v == nil {
			seenUndated = true
			continue
		}
		if seenUndated {
			return fmt.Errorf("assert %q: dated element at index %d follows an undated one", line, i)
		}
		s := fmt.Sprintf("%v", v)
		// Numeric-aware comparison (CHANGELOG.md v1 CLI fix item 2 / STATUS.md
		// A3.1 finding): a plain string compare misorders mixed digit widths
		// (e.g. "9" > "10" lexically, backwards numerically). When both the
		// current and previous values parse as numbers, compare numerically;
		// otherwise fall back to the original stringified comparison (dates
		// and other non-numeric sort fields are unaffected).
		n, isNum := parseNumber(v)
		if prevSet {
			if isNum && prevIsNum {
				if n > prevNum {
					return fmt.Errorf("assert %q: element %d (%s=%v) is out of descending order after %v", line, i, field, n, prevNum)
				}
			} else if s > prev {
				return fmt.Errorf("assert %q: element %d (%s=%s) is out of descending order after %s", line, i, field, s, prev)
			}
		}
		prev = s
		prevNum = n
		prevIsNum = isNum
		prevSet = true
	}
	return nil
}

// parseNumber reports whether v is (or stringifies to) a number, and its
// float64 value if so -- YAML/JSON decoding can hand back int, int64, or
// float64 depending on source, plus fixtures sometimes carry numeric-looking
// strings.
func parseNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
