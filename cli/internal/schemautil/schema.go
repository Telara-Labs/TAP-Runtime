// Package schemautil wraps github.com/santhosh-tekuri/jsonschema/v5 with the
// conveniences tap needs: compiling either an inline (YAML-parsed) schema
// object or a $ref'd JSON file, validating arbitrary Go values (normalizing
// through a JSON round-trip so numeric/string kinds match what the JSON
// Schema draft expects), and walking a schema document for its embedded
// `description` strings (03 §3.1: per-field descriptions are scanned by the
// same rules as the manifest description).
package schemautil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// LoadSchemaDoc loads a JSON schema document either from a file (if ref !=
// "") or from an inline map (already parsed from YAML/JSON). It returns the
// raw generic document (for description-walking) alongside the compiled
// schema (for validation).
func LoadSchemaDoc(ref string, inline map[string]interface{}) (raw map[string]interface{}, schema *jsonschema.Schema, err error) {
	var data []byte
	var id string
	switch {
	case ref != "":
		data, err = os.ReadFile(ref)
		if err != nil {
			return nil, nil, fmt.Errorf("reading schema %s: %w", ref, err)
		}
		id = ref
	case inline != nil:
		data, err = json.Marshal(inline)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling inline schema: %w", err)
		}
		id = "inline.json"
	default:
		return nil, nil, fmt.Errorf("no schema provided (neither $ref nor inline)")
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("%s: not valid JSON: %w", id, err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	if err := compiler.AddResource(id, bytes.NewReader(data)); err != nil {
		return raw, nil, fmt.Errorf("%s: %w", id, err)
	}
	schema, err = compiler.Compile(id)
	if err != nil {
		return raw, nil, fmt.Errorf("%s: invalid JSON Schema: %w", id, err)
	}
	return raw, schema, nil
}

// Normalize round-trips v through JSON encode/decode so Go values produced
// by YAML parsing (e.g. int vs float64, map[string]interface{}) match what
// the jsonschema library expects for a JSON instance.
func Normalize(v interface{}) (interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Validate normalizes v and validates it against schema.
func Validate(schema *jsonschema.Schema, v interface{}) error {
	norm, err := Normalize(v)
	if err != nil {
		return err
	}
	return schema.Validate(norm)
}

// WalkDescriptions calls fn(path, description) for every "description"
// string found anywhere in a schema document (03 §3.1: "Per-field
// description strings inside the I/O schemas follow the same rules and are
// scanned identically").
func WalkDescriptions(doc interface{}, path string, fn func(path, desc string)) {
	switch v := doc.(type) {
	case map[string]interface{}:
		if d, ok := v["description"].(string); ok {
			fn(path, d)
		}
		for k, sub := range v {
			if k == "description" {
				continue
			}
			WalkDescriptions(sub, path+"."+k, fn)
		}
	case []interface{}:
		for i, sub := range v {
			WalkDescriptions(sub, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}
