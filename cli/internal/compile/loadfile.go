package compile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// loadProgram reads a referenced src/*.star transform program to inline into a
// TransformConfig.Program (09 §6: the compiler inlines the referenced source;
// the executor performs no file resolution at run time).
func loadProgram(dir, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return "", fmt.Errorf("reading transform program %q: %w", rel, err)
	}
	return string(b), nil
}

// loadDataFile loads a `*_from` data-file param (e.g. signatures_from:
// src/signatures.yaml, plan_from: src/extract_plan.yaml) into a Go value that
// becomes a static (literal) binding — the referenced content is inlined at
// compile time. YAML and JSON are parsed to structured values; anything else is
// inlined as a raw string.
func loadDataFile(dir, rel string) (interface{}, error) {
	path := filepath.Join(dir, rel)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading data file %q: %w", rel, err)
	}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".yaml", ".yml":
		var v interface{}
		if err := yaml.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("parsing %q: %w", rel, err)
		}
		return normalizeYAML(v), nil
	case ".json":
		var v interface{}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("parsing %q: %w", rel, err)
		}
		return v, nil
	default:
		return string(b), nil
	}
}

// normalizeYAML converts yaml.v3's map[string]interface{} / []interface{} tree
// into one that is JSON-marshalable with stable, string-keyed maps. yaml.v3
// already yields map[string]interface{} for mappings, but nested values may
// carry non-string scalar types that JSON handles fine; the only real fix-up
// needed is guarding against map[interface{}]interface{} (not produced by
// yaml.v3, but cheap to normalize defensively).
func normalizeYAML(v interface{}) interface{} {
	switch tv := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(tv))
		for k, sub := range tv {
			out[k] = normalizeYAML(sub)
		}
		return out
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(tv))
		for k, sub := range tv {
			out[fmt.Sprintf("%v", k)] = normalizeYAML(sub)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(tv))
		for i, sub := range tv {
			out[i] = normalizeYAML(sub)
		}
		return out
	default:
		return v
	}
}

func sortedKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
