package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Text is the common MCP denominator. Send one representation, rather than
// repeating a full payload in both content and structuredContent. Automation
// callers can request detail=true for the original JSON contract.
func (s *server) toolOutput(id *json.RawMessage, name string, value any, detail bool) {
	b, err := json.Marshal(value)
	if err != nil {
		s.toolError(id, "local TAP result could not be encoded")
		return
	}
	if len(b) > 64<<10 {
		s.toolError(id, "local TAP result is too large")
		return
	}
	text := string(b)
	if !detail {
		var object map[string]any
		if err := json.Unmarshal(b, &object); err != nil {
			s.toolError(id, "local TAP result could not be formatted")
			return
		}
		text = readableToolOutput(name, object)
	}
	if len(text) > 64<<10 {
		s.toolError(id, "local TAP result is too large; request detail=true")
		return
	}
	s.reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
}

func inlineJSON(value any) string {
	b, _ := json.Marshal(value) // Values have already passed JSON encoding.
	return string(b)
}

func lineValue(value any) string {
	if text, ok := value.(string); ok {
		if strings.ContainsAny(text, "\n\r\t\x1b") {
			return inlineJSON(text)
		}
		return text
	}
	return inlineJSON(value)
}

func outputLine(b *strings.Builder, label string, value any) {
	if value == nil || value == "" {
		return
	}
	fmt.Fprintf(b, "%s: %s\n", label, lineValue(value))
}

func readableToolOutput(name string, value map[string]any) string {
	var b strings.Builder
	switch name {
	case "tap_search":
		hits, _ := value["matches"].([]any)
		if len(hits) == 0 {
			b.WriteString("No matching TAP primitive.\n")
			outputLine(&b, "Note", value["note"])
		} else {
			fmt.Fprintf(&b, "%d TAP primitive(s)\n", len(hits))
			for _, hit := range hits {
				row := hit.(map[string]any)
				fmt.Fprintf(&b, "\n%s\n", lineValue(row["ref"]))
				outputLine(&b, "Digest", row["digest"])
				outputLine(&b, "Description", row["description"])
			}
			b.WriteString("\nNext: tap_load with the exact ref and digest.\n")
		}
	case "tap_load":
		fmt.Fprintf(&b, "Loaded %s\n", lineValue(value["ref"]))
		outputLine(&b, "Description", value["description"])
		outputLine(&b, "Digest", value["digest"])
		if intf, ok := value["interface"].(map[string]any); ok {
			if schema, ok := intf["inputSchema"].(map[string]any); ok && len(schema) != 0 {
				b.WriteString("\nInputs\n")
				writeInputSchema(&b, schema)
			}
		}
		outputLine(&b, "Args", value["args"])
		writeDeclarations(&b, value)
		tools, _ := value["tools"].([]any)
		if preview, ok := value["connection_preview"].(map[string]any); ok && len(tools) != 0 {
			b.WriteString("\nConnections\n")
			if preview["status"] != "available" {
				outputLine(&b, "Inventory", strings.ReplaceAll(lineValue(preview["status"]), "_", " "))
			}
			rows, _ := preview["connections"].([]any)
			for _, item := range rows {
				row := item.(map[string]any)
				fmt.Fprintf(&b, "  %s: %s", lineValue(row["alias"]), lineValue(row["status"]))
				if row["status"] == "resolved" {
					fmt.Fprintf(&b, " → %s/%s; base effect %s", lineValue(row["server"]), lineValue(row["tool"]), lineValue(row["base_effect"]))
					if row["base_approval_required"] == true {
						b.WriteString("; approval required")
					}
				}
				if row["optional"] == true {
					b.WriteString("; optional")
				}
				if row["requires_runtime_check"] == true {
					b.WriteString("; runtime check required")
				}
				if operation, ok := row["operation"].(string); ok && operation != "" {
					fmt.Fprintf(&b, "; operation %s", lineValue(operation))
				}
				b.WriteByte('\n')
			}
			if preview["truncated"] == true {
				b.WriteString("Preview truncated; some bindings omitted.\n")
			}
			b.WriteString("Preview only; execution rechecks connections and per-call effects.\n")
		}
		b.WriteString("\nNext: tap_run with the exact ref and digest above.\nFull declarations and output schema: tap_load with detail=true.\n")
	case "tap_status", "tap_evidence":
		fmt.Fprintf(&b, "Run %s — %s\n", lineValue(value["run_id"]), lineValue(value["state"]))
		outputLine(&b, "Outcome", value["outcome"])
		outputLine(&b, "Package digest", value["package_digest"])
		outputLine(&b, "Started", value["started"])
		if name == "tap_evidence" {
			outputLine(&b, "Primitive", value["ref"])
			outputLine(&b, "Permissions", value["permissions_status"])
			if manifest, ok := value["manifest"].(map[string]any); ok {
				outputLine(&b, "Manifest", manifest["status"])
				outputLine(&b, "Manifest digest", manifest["digest"])
				outputLine(&b, "Manifest content", manifest["content_status"])
				outputLine(&b, "Manifest reason", manifest["reason"])
				if yaml, ok := manifest["yaml"].(string); ok {
					b.WriteString("\nSaved manifest\n" + yaml + "\n")
				}
			}
			if permissions, ok := value["declared_permissions"].(map[string]any); ok {
				writeDeclarations(&b, permissions)
			}
			if events, ok := value["events"].([]any); ok && len(events) != 0 {
				b.WriteString("\nEvents (journaled request metadata)\n")
				for _, event := range events {
					fmt.Fprintf(&b, "  %s\n", inlineJSON(event))
				}
			}
			if value["truncated"] == true {
				b.WriteString("Evidence truncated; events omitted or journal inspection bounded.\n")
			}
			b.WriteString("Local journal evidence; arguments and results excluded.\n")
		}
	default:
		return inlineJSON(value)
	}
	return strings.TrimSpace(b.String())
}

// Keep every input constraint, including unknown JSON Schema keywords and
// nested definitions. Only the presentation changes; false and null are data.
func writeInputSchema(b *strings.Builder, schema map[string]any) {
	properties, _ := schema["properties"].(map[string]any)
	required := schemaStrings(schema["required"])
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		label := lineValue(key)
		for _, field := range required {
			if key == field {
				label += " (required)"
				break
			}
		}
		fmt.Fprintf(b, "  %s: %s\n", label, inlineJSON(properties[key]))
	}
	constraints := make(map[string]any)
	for key, value := range schema {
		if key != "properties" {
			constraints[key] = value
		}
	}
	if len(constraints) != 0 {
		outputLine(b, "Constraints", constraints)
	}
}

func writeDeclarations(b *strings.Builder, value map[string]any) {
	for _, section := range []struct{ key, label string }{{"tools", "Tools"}, {"commands", "Commands"}, {"files", "Files"}, {"fetch", "Network"}} {
		items, _ := value[section.key].([]any)
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n%s\n", section.label)
		for _, item := range items {
			if section.key == "tools" {
				row := item.(map[string]any)
				fmt.Fprintf(b, "  %s [%s] %s", lineValue(row["alias"]), lineValue(row["effect"]), lineValue(row["capability"]))
				if row["optional"] == true {
					b.WriteString("; optional")
				}
				if pin, ok := row["pin"].(map[string]any); ok {
					fmt.Fprintf(b, "; pin %s", inlineJSON(pin))
				}
				b.WriteByte('\n')
				continue
			}
			fmt.Fprintf(b, "  %s\n", inlineJSON(item))
		}
	}
}
