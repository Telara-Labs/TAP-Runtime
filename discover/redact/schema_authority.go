package redact

import (
	"math/bits"
	"sort"

	"gopkg.in/yaml.v3"
)

func isBooleanSchema(value any) bool {
	_, boolean := metadataScalar(value).(bool)
	return boolean
}

// validatedSchemaChild establishes shape authority only for known schema
// edges. Every allocation and content validation is charged before it runs.
// Annotation values and unknown extensions never become schema edges.
// Invalid shape and exhausted work are distinct: the latter refuses inspection.
func validatedSchemaChild(key string, value any, precharge func(int) bool) (valid, admitted bool) {
	remaining := maxFieldContextValidationWork
	charge := func(work int) bool {
		if work < 0 || work > remaining || precharge == nil || !precharge(work) {
			return false
		}
		remaining -= work
		return true
	}
	chargeMetadata := func(value any) bool {
		if !charge(1) {
			return false
		}
		values := []any{value}
		for len(values) > 0 {
			last := len(values) - 1
			value := values[last]
			values[last] = nil
			values = values[:last]
			if !charge(1) {
				return false
			}
			switch v := value.(type) {
			case string:
				if !charge(len(v)) {
					return false
				}
			case []any:
				if !charge(1 + len(v)) {
					return false
				}
				values = append(values, v...)
			case map[string]any:
				if !charge(1 + len(v)) {
					return false
				}
				for name, child := range v {
					if !charge(len(name)) {
						return false
					}
					values = append(values, child)
				}
			case *yaml.Node:
				if !charge(1 + len(v.Value) + len(v.Content)) {
					return false
				}
				for _, child := range v.Content {
					values = append(values, child)
				}
			}
		}
		return true
	}
	var pending []any
	addSchema := func(value any) (bool, bool) {
		if !charge(1) {
			return false, false
		}
		if node, ok := value.(*yaml.Node); ok {
			if node.Anchor != "" || node.Kind == yaml.AliasNode {
				return false, true
			}
			if !charge(len(node.Value)) {
				return false, false
			}
		}
		if _, boolean := metadataScalar(value).(bool); boolean {
			return true, true
		}
		if !isMappingContainer(value) {
			return false, true
		}
		pending = append(pending, value)
		return true, true
	}
	addSequence := func(value any, nonempty bool) (bool, bool) {
		size, mapping, exists := containerSize(value)
		if !exists || mapping || nonempty && size == 0 {
			return false, true
		}
		if !charge(1 + size) {
			return false, false
		}
		if node, ok := value.(*yaml.Node); ok {
			if node.Anchor != "" || node.Kind == yaml.AliasNode {
				return false, true
			}
			for _, child := range node.Content {
				if valid, admitted := addSchema(child); !valid || !admitted {
					return valid, admitted
				}
			}
		} else {
			for _, child := range value.([]any) {
				if valid, admitted := addSchema(child); !valid || !admitted {
					return valid, admitted
				}
			}
		}
		return true, true
	}
	addChild := func(key string, value any) (bool, bool) {
		switch key {
		case "allOf", "anyOf", "oneOf", "prefixItems":
			return addSequence(value, true)
		case "items":
			if isSequenceContainer(value) {
				return addSequence(value, true)
			}
		}
		return addSchema(value)
	}
	// Charge map conversion and ordering before allocation. Normalized
	// duplicate vocabulary names cannot authorize a schema shape either.
	mapEntries := func(value any) (map[string]any, []string, bool, bool) {
		size, mapping, exists := containerSize(value)
		if !exists || !mapping {
			return nil, nil, false, true
		}
		if !charge(1 + size) {
			return nil, nil, false, false
		}
		if node, ok := value.(*yaml.Node); ok {
			if node.Anchor != "" {
				return nil, nil, false, true
			}
			for i := 0; i < len(node.Content); i += 2 {
				if !charge(1 + len(node.Content[i].Value)) {
					return nil, nil, false, false
				}
			}
		}
		members, ok := metadataMap(value)
		if !ok {
			return nil, nil, false, true
		}
		if !charge(size * (bits.Len(uint(size)) + 1)) {
			return nil, nil, false, false
		}
		keys := make([]string, 0, size)
		for name := range members {
			if !charge(1 + len(name)) {
				return nil, nil, false, false
			}
			keys = append(keys, name)
		}
		sort.Strings(keys)
		return members, keys, true, true
	}
	if valid, admitted := addChild(key, value); !valid || !admitted {
		return valid, admitted
	}
	for len(pending) > 0 {
		last := len(pending) - 1
		value := pending[last]
		pending[last] = nil
		pending = pending[:last]
		members, keys, valid, admitted := mapEntries(value)
		if !valid || !admitted {
			return valid, admitted
		}
		if !charge(len(keys)) {
			return false, false
		}
		seen := make(map[string]bool, len(keys))
		for _, name := range keys {
			key := Normalize(name)
			if seen[key] {
				return false, true
			}
			seen[key] = true
			value := members[name]
			switch {
			case schemaPropertyKeyword(key):
				names, ordered, valid, admitted := mapEntries(value)
				if !valid || !admitted {
					return valid, admitted
				}
				for _, declared := range ordered {
					child := names[declared]
					if key == "dependencies" && isSequenceContainer(child) {
						if !chargeMetadata(child) {
							return false, false
						}
						if !metadataStrings(child, false) {
							return false, true
						}
						continue
					}
					if valid, admitted := addSchema(child); !valid || !admitted {
						return valid, admitted
					}
				}
			case schemaChildKeyword(key):
				if valid, admitted := addChild(key, value); !valid || !admitted {
					return valid, admitted
				}
			case schemaMetadataKeyword(key):
				if !chargeMetadata(value) {
					return false, false
				}
				if !validSchemaMetadata(key, value) {
					return false, true
				}
			}
		}
	}
	return true, true
}
