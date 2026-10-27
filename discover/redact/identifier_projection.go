package redact

import (
	"strconv"

	"gopkg.in/yaml.v3"
)

// ProjectIdentifiers preserves uniqueness when inspected schema identifiers
// collapse to the same display string. Only validated identifier-list contexts
// participate. Cardinality, order and every unchanged item are retained.
// format must be the consumer's deterministic, closed marker formatter, with
// output no longer than twice its input plus 64 bytes; nil means identity.
// A nil projection means this value is not an eligible identifier list.
func (c FieldContext) ProjectIdentifiers(value any, format func(string) string, precharge func(int) bool) ([]string, bool) {
	return c.projectIdentifiers(value, format, func(text string) string { return inspectField(c.state, text, false) }, precharge)
}

func (c FieldContext) projectIdentifiers(value any, format, inspect func(string) string, precharge func(int) bool) ([]string, bool) {
	if !c.state.schemaIdentifierList || c.state.data || !c.state.metadata {
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
	size, mapping, sequence := containerSize(value)
	if !sequence || mapping {
		return nil, true
	}
	if size > maxFieldContextValidationWork/8 || !charge(1+8*size) {
		return nil, false
	}
	original := make([]string, size)
	projected := make([]string, size)
	sourceSeen := make(map[string]bool, size)
	occupied := make(map[string]bool, 3*size)
	counts := make(map[string]int, size)
	keeper := make(map[string]int, size)
	for i := 0; i < size; i++ {
		var item any
		if node, ok := value.(*yaml.Node); ok {
			if node.Anchor != "" || node.Kind != yaml.SequenceNode {
				return nil, true
			}
			item = node.Content[i]
		} else {
			item = value.([]any)[i]
		}
		if node, ok := item.(*yaml.Node); ok {
			if node.Anchor != "" || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
				return nil, true
			}
			item = node.Value
		}
		text, ok := metadataScalar(item).(string)
		if !ok {
			return nil, true
		}
		if !charge(1 + len(text)) {
			return nil, false
		}
		if sourceSeen[text] {
			// Pre-existing duplicate strings, including markers, never gain an
			// exemption merely by resembling a previous redaction result.
			return nil, true
		}
		sourceSeen[text] = true
		original[i] = text
		occupied[text] = true
	}
	for i, text := range original {
		if !charge(1 + len(text)) {
			return nil, false
		}
		inspected := inspect(text)
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
		projected[i] = inspected
		occupied[inspected] = true
		counts[inspected]++
		if inspected == text {
			keeper[inspected] = i
		}
	}
	for i, text := range projected {
		if counts[text] <= 1 {
			continue
		}
		keep, exists := keeper[text]
		if !exists {
			keeper[text] = i
			continue
		}
		if keep == i {
			continue
		}
		// With valid unique source strings, only changed items can reach
		// this branch. Position and a bounded collision counter reveal no
		// credential identity. Reserve every candidate against source and
		// projected strings before allocating its spelling.
		limit := len(occupied) + 1
		chosen := false
		for attempt := 0; attempt <= limit; attempt++ {
			if !charge(1 + len(text) + 64) {
				return nil, false
			}
			candidate := text + " [item " + strconv.Itoa(i+1) + "]"
			if attempt > 0 {
				candidate += " " + strconv.Itoa(attempt)
			}
			if !occupied[candidate] {
				occupied[candidate] = true
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
