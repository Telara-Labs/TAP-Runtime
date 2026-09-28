package testrunner

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func unmarshalJSONMap(b []byte) (map[string]interface{}, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// GetPath navigates a dot-path (e.g. "summary.verdict", "entries.0.version",
// "entries.length") over a generic Go value tree. ".length" on a slice
// returns its length; numeric segments index into slices.
func GetPath(root interface{}, path string) (interface{}, bool) {
	if path == "" {
		return root, true
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]interface{}:
			nv, ok := v[seg]
			if !ok {
				return nil, false
			}
			cur = nv
		case []interface{}:
			if seg == "length" {
				cur = len(v)
				continue
			}
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(v) {
				return nil, false
			}
			cur = v[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// DeepEqualLoose compares two generic values (as produced by YAML/JSON
// decoding) tolerating int-vs-float64 kind differences from the two
// decoders.
func DeepEqualLoose(a, b interface{}) bool {
	an, aok := normalizeNumber(a)
	bn, bok := normalizeNumber(b)
	if aok && bok {
		return an == bn
	}
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, aval := range av {
			bval, ok := bv[k]
			if !ok || !DeepEqualLoose(aval, bval) {
				return false
			}
		}
		return true
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !DeepEqualLoose(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
	}
}

func normalizeNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}
