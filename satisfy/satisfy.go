// Package satisfy decides whether a tool does what a capability's contract
// asks, where the client gives the tool's input schema.
//
// The rule is doc 34 section 11.3, and it is normative:
//
//	Arguments, checked at admission: every argument the contract declares
//	must be accepted by the connector, and every argument the connector
//	marks required must be supplied by the contract. Extras on either side
//	are allowed.
//
//	Results cannot be checked at admission, so they are validated at run
//	time against the contract.
//
// Ruling 30 (doc 34 section 13.16) settled what that rule left unsaid about
// types and about a connector that describes nothing.
package satisfy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Digest identifies a schema whatever order its keys are written in. It is
// the `schema` of a binding (34 section 11.2): when it changes, the
// connector changed shape under the package.
func Digest(schema map[string]any) string {
	b, _ := json.Marshal(schema)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func properties(schema map[string]any) map[string]any {
	p, _ := schema["properties"].(map[string]any)
	return p
}

func required(schema map[string]any) []string {
	var out []string
	switch r := schema["required"].(type) {
	case []any:
		for _, x := range r {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, r...)
	}
	sort.Strings(out)
	return out
}

// types returns the JSON types a property allows, or nil when it does not
// say. "type" may be one name or a list of names.
func types(prop any) []string {
	m, _ := prop.(map[string]any)
	switch t := m["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// accepts reports whether a connector that allows the types in have will
// take a value of every type in want.
//
// Ruling 30: an integer is a number, so a contract that sends an integer is
// accepted where a number is. The reverse is not: a connector wanting an
// integer is not satisfied by a contract that may send 1.5. "null" in the
// connector's list is ignored. A side that names no type accepts anything.
func accepts(have, want []string) bool {
	if len(have) == 0 || len(want) == 0 {
		return true
	}
	allowed := map[string]bool{}
	for _, t := range have {
		allowed[t] = true
	}
	for _, t := range want {
		if t == "null" {
			continue
		}
		if !allowed[t] && !(t == "integer" && allowed["number"]) {
			return false
		}
	}
	return true
}

// Arguments applies the admission half of the rule. It returns what stops
// the tool from satisfying the contract; empty means it satisfies.
func Arguments(contract, tool map[string]any) []string {
	var p []string
	cp, tp := properties(contract), properties(tool)
	// Ruling 30: an empty tool schema accepts anything. A connector that
	// describes no arguments has said nothing a contract can be held
	// against, so the tool binds on its name (ruling 22) and its arguments
	// are not checked.
	if len(tp) == 0 && len(required(tool)) == 0 {
		// Unless it says, in so many words, that it takes no arguments.
		if allows, said := tool["additionalProperties"].(bool); !said || allows {
			return nil
		}
	}
	open := false
	if ap, ok := tool["additionalProperties"].(bool); ok && ap {
		open = true
	}
	var names []string
	for n := range cp {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tprop, ok := tp[n]
		if !ok {
			if !open {
				p = append(p, fmt.Sprintf("the contract sends %q, which the connector does not accept", n))
			}
			continue
		}
		if !accepts(types(tprop), types(cp[n])) {
			p = append(p, fmt.Sprintf("the contract sends %q as %s, and the connector takes %s",
				n, strings.Join(types(cp[n]), " or "), strings.Join(types(tprop), " or ")))
		}
	}
	for _, n := range required(tool) {
		if _, ok := cp[n]; !ok {
			p = append(p, fmt.Sprintf("the connector requires %q, which the contract does not supply", n))
		}
	}
	return p
}

// Result applies the run-time half: the tool's answer is checked against the
// contract's result schema. It returns "" when the answer conforms.
func Result(contract map[string]any, answer string) string {
	if len(contract) == 0 {
		return ""
	}
	raw, _ := json.Marshal(contract)
	sc, err := jsonschema.CompileString("contract-result.json", string(raw))
	if err != nil {
		return "the contract's result schema does not compile: " + err.Error()
	}
	var v any
	if err := json.Unmarshal([]byte(answer), &v); err != nil {
		return "the answer is not JSON"
	}
	if err := sc.Validate(v); err != nil {
		if ve, ok := err.(*jsonschema.ValidationError); ok {
			for len(ve.Causes) > 0 {
				ve = ve.Causes[0]
			}
			at := strings.TrimPrefix(ve.InstanceLocation, "/")
			if at == "" {
				at = "the answer"
			}
			return at + ": " + ve.Message
		}
		return err.Error()
	}
	return ""
}
