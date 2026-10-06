package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// checkRunArgs holds tap_run's arguments to the primitive's declared input:
// a primitive whose inputSchema names fields reads one JSON object from its
// first argument (the convention discover's generated programs and the
// authoring guide share). Arguments of another shape are refused before the
// program starts, with what to send, instead of failing inside the program.
func checkRunArgs(m *mf.Manifest, args []string) string {
	if m == nil || m.Interface == nil {
		return ""
	}
	schema := m.Interface.InputSchema
	props, _ := schema["properties"].(map[string]any)
	required := schemaStrings(schema["required"])
	if len(props) == 0 && len(required) == 0 {
		return ""
	}
	var obj map[string]any
	if len(args) != 1 || json.Unmarshal([]byte(args[0]), &obj) != nil || obj == nil {
		return "this primitive takes one argument: a JSON object of its inputs, as a string, for example args: [\"" +
			strings.ReplaceAll(exampleInput(props, required), `"`, `\"`) + "\"]. tap_load shows its inputSchema."
	}
	var missing []string
	for _, k := range required {
		if _, ok := obj[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("the input object lacks required %s; tap_load shows its inputSchema", strings.Join(missing, ", "))
	}
	return ""
}

func schemaStrings(v any) []string {
	items, _ := v.([]any)
	var out []string
	for _, x := range items {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// exampleInput is the required fields (or, without any, the declared ones)
// as a JSON object with placeholder values.
func exampleInput(props map[string]any, required []string) string {
	keys := required
	if len(keys) == 0 {
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%q: ...", k)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func inputProps(m *mf.Manifest) map[string]any {
	if m == nil || m.Interface == nil {
		return nil
	}
	props, _ := m.Interface.InputSchema["properties"].(map[string]any)
	return props
}
