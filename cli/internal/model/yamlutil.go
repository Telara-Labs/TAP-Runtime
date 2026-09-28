package model

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadYAMLFile reads and parses a YAML file into a generic map. yaml.v3
// unmarshals mappings into map[string]interface{} (not
// map[interface{}]interface{} as v2 did), which is what the closed-key
// checkers below assume.
func LoadYAMLFile(path string) (map[string]interface{}, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, b, fmt.Errorf("%s: %w", path, err)
	}
	return out, b, nil
}

// AsMap type-asserts v as map[string]interface{}, returning nil, false
// otherwise.
func AsMap(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	return m, ok
}

// AsSlice type-asserts v as []interface{}.
func AsSlice(v interface{}) ([]interface{}, bool) {
	s, ok := v.([]interface{})
	return s, ok
}

// AsString type-asserts v as string.
func AsString(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// StringVal returns v as a string, or "" if it isn't one / is absent.
func StringVal(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// BoolVal returns v as a bool, defaulting to def if absent/wrong type.
func BoolVal(m map[string]interface{}, key string, def bool) bool {
	if m == nil {
		return def
	}
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// IntVal returns v as an int, defaulting to def if absent/wrong type.
func IntVal(m map[string]interface{}, key string, def int) int {
	if m == nil {
		return def
	}
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return def
}

// StringSliceVal returns a []string from a YAML sequence value, tolerating
// absence.
func StringSliceVal(m map[string]interface{}, key string) []string {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	seq, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(seq))
	for _, e := range seq {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Keys returns the sorted key set of a map, for stable diagnostics.
func Keys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UnknownKeys returns keys present in m that are not in allowed.
func UnknownKeys(m map[string]interface{}, allowed []string) []string {
	allowedSet := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = true
	}
	var out []string
	for _, k := range Keys(m) {
		if !allowedSet[k] {
			out = append(out, k)
		}
	}
	return out
}

// JoinPath builds a dotted diagnostic path.
func JoinPath(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, ".")
}
