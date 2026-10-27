package redact

import (
	"net/url"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ProjectVocabularyKeys preserves valid URI keys after credential inspection.
// Authority comes only from Child's original valid, unique $vocabulary occurrence.
// orderedKeys must be the complete original member order. For decoded core JSON
// maps, use the existing sorted traversal order; never use Go map iteration order.
// Values and occurrence facts follow FieldContext's immutable source contract.
// format has ProjectIdentifiers' closed, deterministic formatter contract.
// A nil projection means this occurrence is ineligible. Refusal is atomic.
func (c FieldContext) ProjectVocabularyKeys(value any, orderedKeys []string, format func(string) string, precharge func(int) bool) ([]string, bool) {
	return c.projectVocabularyKeys(value, orderedKeys, format, func(text string) string { return inspectField(c.state, text, false) }, precharge)
}

func (c FieldContext) projectVocabularyKeys(value any, orderedKeys []string, format, inspect func(string) string, precharge func(int) bool) ([]string, bool) {
	if !c.state.schemaVocabularyKeys || c.state.projectionAmbiguous || c.state.vocabularyProjectionBlocked || !c.state.metadata || !c.state.metadataMap || c.state.data {
		return nil, true
	}
	remaining := maxFieldContextValidationWork
	charge := func(work int) bool {
		if work < 0 || work > remaining || precharge == nil || !precharge(work) {
			return false
		}
		remaining -= work
		return true
	}
	size, mapping, exists := containerSize(value)
	if !exists || !mapping || size != len(orderedKeys) {
		return nil, true
	}
	if size > maxFieldContextValidationWork/12 || !charge(1+12*size) {
		return nil, false
	}
	// Reserve every key byte before comparisons, hashing, conversion, URI
	// parsing or normalization. Inspecting lengths alone does not read contents.
	chargeKeyBytes := func(length int) bool {
		if length > (maxFieldContextValidationWork-1)/4 {
			return false
		}
		return charge(1 + 4*length)
	}
	for _, key := range orderedKeys {
		if !chargeKeyBytes(len(key)) {
			return nil, false
		}
	}
	var node *yaml.Node
	var members map[string]any
	if n, yamlValue := value.(*yaml.Node); yamlValue {
		if n.Kind != yaml.MappingNode || n.Anchor != "" || len(n.Content) != 2*size {
			return nil, true
		}
		for i := range orderedKeys {
			if !chargeKeyBytes(len(n.Content[2*i].Value)) {
				return nil, false
			}
		}
		node = n
	} else {
		var ok bool
		members, ok = value.(map[string]any)
		if !ok {
			return nil, true
		}
	}
	projected := make([]string, size)
	changed := make([]bool, size)
	seen := make(map[string]bool, size)
	normalizedSeen := make(map[string]bool, size)
	occupied := make(map[string]bool, 3*size)
	// Establish the complete original shape before inspecting or mutating any key.
	for i, key := range orderedKeys {
		var v any
		present := true
		if node != nil {
			k, value := node.Content[2*i], node.Content[2*i+1]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Anchor != "" || k.Value != key || value.Kind != yaml.ScalarNode || value.Tag != "!!bool" || value.Anchor != "" {
				return nil, true
			}
			v = value
		} else {
			v, present = members[key]
		}
		if !present || seen[key] {
			return nil, true
		}
		seen[key] = true
		normalized := Normalize(key)
		if normalizedSeen[normalized] {
			return nil, true
		}
		normalizedSeen[normalized] = true
		uri, err := url.Parse(key)
		if err != nil || !uri.IsAbs() {
			return nil, true
		}
		if _, boolean := metadataScalar(v).(bool); !boolean {
			return nil, true
		}
		// Generated candidates are ASCII; canonical occupancy reserves both
		// literal and normalized collisions without changing original spelling.
		occupied[normalized] = true
	}
	for i, key := range orderedKeys {
		if !charge(1 + len(key)) {
			return nil, false
		}
		inspected := inspect(key)
		if !charge(1 + len(inspected)) {
			return nil, false
		}
		if format != nil {
			bound := 2*len(inspected) + 64
			if !charge(bound) {
				return nil, false
			}
			inspected = format(inspected)
			if len(inspected) > bound {
				return nil, false
			}
		}
		changed[i] = inspected != key
		projected[i] = key
		// Intermediate scanner/formatter results cannot collide with a generated URI.
		if !chargeKeyBytes(len(inspected)) {
			return nil, false
		}
		occupied[Normalize(inspected)] = true
	}
	for i := range orderedKeys {
		if !changed[i] {
			continue
		}
		limit := len(occupied) + 1
		chosen := false
		for attempt := 0; attempt <= limit; attempt++ {
			// Fixed grammar plus bounded position/counter has at most 96 output bytes.
			// Reserve all temporary integer strings and concatenations before allocation.
			if !charge(256) {
				return nil, false
			}
			candidate := "urn:telara:redacted:vocabulary:" + strconv.Itoa(i+1)
			if attempt > 0 {
				candidate += ":" + strconv.Itoa(attempt)
			}
			normalizedCandidate := Normalize(candidate) // Covered by the 256-unit reservation.
			if !occupied[normalizedCandidate] {
				occupied[normalizedCandidate] = true
				projected[i] = candidate
				chosen = true
				break
			}
		}
		if !chosen {
			return nil, false
		}
	}
	return projected, true
}
