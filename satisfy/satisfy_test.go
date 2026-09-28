package satisfy

import (
	"strings"
	"testing"
)

func obj(props map[string]any, req ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		r := make([]any, len(req))
		for i, s := range req {
			r[i] = s
		}
		m["required"] = r
	}
	return m
}

func typ(t string) map[string]any { return map[string]any{"type": t} }

// The golden and negative cases of the rule in doc 34 section 11.3.
func TestArguments(t *testing.T) {
	threads := obj(map[string]any{"query": typ("string"), "pageSize": typ("integer"), "pageToken": typ("string")}, "query")
	cases := []struct {
		name     string
		contract map[string]any
		tool     map[string]any
		want     string // a fragment of the problem, or "" for satisfies
	}{
		{"identical", threads, threads, ""},
		{"connector accepts more than the contract sends", threads,
			obj(map[string]any{"query": typ("string"), "pageSize": typ("integer"), "pageToken": typ("string"), "view": typ("string")}, "query"), ""},
		{"contract declares an argument the connector does not accept", threads,
			obj(map[string]any{"query": typ("string"), "max_results": typ("integer")}, "query"), `sends "pageSize"`},
		{"connector requires an argument the contract does not supply", threads,
			obj(map[string]any{"query": typ("string"), "pageSize": typ("integer"), "pageToken": typ("string"), "account": typ("string")}, "query", "account"),
			`requires "account"`},
		{"connector requires less than the contract does", threads,
			obj(map[string]any{"query": typ("string"), "pageSize": typ("integer"), "pageToken": typ("string")}), ""},
		{"an integer is accepted where a number is taken",
			obj(map[string]any{"n": typ("integer")}), obj(map[string]any{"n": typ("number")}), ""},
		{"a number is not accepted where an integer is taken",
			obj(map[string]any{"n": typ("number")}), obj(map[string]any{"n": typ("integer")}), `sends "n" as number`},
		{"a string is not accepted where an integer is taken",
			obj(map[string]any{"n": typ("string")}), obj(map[string]any{"n": typ("integer")}), `sends "n" as string`},
		{"a nullable connector type",
			obj(map[string]any{"q": typ("string")}), obj(map[string]any{"q": map[string]any{"type": []any{"string", "null"}}}), ""},
		{"a property that names no type accepts anything",
			obj(map[string]any{"q": typ("string")}), obj(map[string]any{"q": map[string]any{"description": "a query"}}), ""},
		{"an open connector accepts what it does not list",
			threads, map[string]any{"type": "object", "additionalProperties": true}, ""},
		{"a connector with an empty schema accepts nothing we can be sure of",
			threads, map[string]any{"type": "object"}, "does not accept"},
		{"no schema at all", threads, nil, "gives no input schema"},
		{"a contract that sends nothing satisfies a connector that requires nothing",
			obj(map[string]any{}), obj(map[string]any{"q": typ("string")}), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(Arguments(c.contract, c.tool), "; ")
			if c.want == "" && got != "" {
				t.Fatalf("does not satisfy: %s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("want a problem mentioning %q, got %q", c.want, got)
			}
		})
	}
}

func TestResult(t *testing.T) {
	contract := obj(map[string]any{"threads": map[string]any{"type": "array"}, "nextPageToken": typ("string")}, "threads")
	for name, c := range map[string]struct{ answer, want string }{
		"conforms":              {`{"threads":[],"nextPageToken":"x"}`, ""},
		"extra fields are fine": {`{"threads":[],"resultCountEstimate":3}`, ""},
		"a required field is missing, as when the connector answers a different question": {`{"emails":[]}`, "threads"},
		"a field of the wrong type": {`{"threads":"none"}`, "threads"},
		"not JSON":                  {`Action completed.`, "not JSON"},
	} {
		got := Result(contract, c.answer)
		if c.want == "" && got != "" {
			t.Errorf("%s: %s", name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q in %q", name, c.want, got)
		}
	}
	if got := Result(nil, `anything`); got != "" {
		t.Errorf("no contract, and still: %s", got)
	}
}

func TestDigestIgnoresKeyOrder(t *testing.T) {
	a := map[string]any{"type": "object", "properties": map[string]any{"a": typ("string"), "b": typ("integer")}}
	b := map[string]any{"properties": map[string]any{"b": typ("integer"), "a": typ("string")}, "type": "object"}
	if Digest(a) != Digest(b) {
		t.Fatal("the digest depends on key order")
	}
	b["properties"].(map[string]any)["b"] = typ("number")
	if Digest(a) == Digest(b) {
		t.Fatal("a changed schema has the same digest")
	}
}
