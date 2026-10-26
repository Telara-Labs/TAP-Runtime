package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCorrelationSiblingsRemainExact(t *testing.T) {
	for _, id := range []string{"req\u200d1", "req\u00a01", "req\ufe0f1", "req\u202f1"} {
		for _, credential := range []string{"syntheticOpaqueValue42", "ghp_" + strings.Repeat("A", 36)} {
			for _, escaped := range []bool{false, true} {
				input := `{"jsonrpc":"2.0","id":` + encodeValue(id) + `,"result":{"label":` + encodeValue(id) + `,"password":` + encodeValue(credential) + `}}`
				if escaped {
					input = strings.NewReplacer("\u200d", `\u200d`, "\u00a0", `\u00a0`, "\ufe0f", `\ufe0f`, "\u202f", `\u202f`).Replace(input)
				}
				for _, inspect := range []func(string) string{Redact, func(s string) string { return Field(nil, s) }} {
					safe := inspect(input)
					var got struct {
						ID     string `json:"id"`
						Result struct {
							Label string `json:"label"`
						} `json:"result"`
					}
					if err := json.Unmarshal([]byte(safe), &got); err != nil {
						t.Fatal(err, safe)
					}
					if got.ID != id || got.Result.Label != id {
						t.Fatalf("siblings changed %q / %q, expected %q", got.ID, got.Result.Label, id)
					}
					if strings.Contains(safe, credential) {
						t.Fatal("credential survived")
					}
				}
			}
		}
	}
}

func TestCorrelationDetectionViewsKeepOriginalKeys(t *testing.T) {
	for _, name := range []string{"pro\u200dperties", "properties"} {
		input := map[string]any{"id": "req\u200d1", name: map[string]any{"password": map[string]any{"type": "string", "descrip\u200dtion": "ordinary schema prose", "de\u00a0fault": "not an annotation", "default": "syntheticOpaqueValue42"}}}
		raw, _ := json.Marshal(input)
		safe := Field(nil, string(raw))
		if strings.Contains(safe, "syntheticOpaqueValue42") {
			t.Fatal("credential survived")
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(safe), &got); err != nil {
			t.Fatal(err, safe)
		}
		if got["id"] != "req\u200d1" {
			t.Fatal("correlation changed")
		}
		schema, ok := got[name].(map[string]any)
		if !ok {
			t.Fatal("original schema key changed", safe)
		}
		property := schema["password"].(map[string]any)
		if property["descrip\u200dtion"] != "ordinary schema prose" {
			t.Fatal("schema detection view lost", safe)
		}
	}
	// Both distinct original keys must survive even when their detection views coincide.
	input := `{"id":"req‍1","a‍b":"first","ab":"second","result":{"password":"syntheticOpaqueValue42"}}`
	var got map[string]any
	if err := json.Unmarshal([]byte(Field(nil, input)), &got); err != nil {
		t.Fatal(err)
	}
	if got["a‍b"] != "first" || got["ab"] != "second" {
		t.Fatal("distinct keys merged", got)
	}
}

func TestCorrelationFloorProjectsOnlyCredentialSpans(t *testing.T) {
	for _, id := range []string{"req\u200d1", "req\u00a01", "req\ufe0f1"} {
		for _, token := range []string{"ghp_" + strings.Repeat("A", 36), "gh\u200dp_" + strings.Repeat("A", 36)} {
			raw := `{"id":` + encodeValue(id) + `,"data":` + encodeValue(token) + `,"label":` + encodeValue(id) + `}`
			// Simulate a future parser leaving a frozen-floor match behind. The floor
			// itself must remove it without emitting normalized sibling values.
			safe := gatewayFloor(raw, inspection{text: raw}).text
			var got map[string]string
			if err := json.Unmarshal([]byte(safe), &got); err != nil {
				t.Fatal(err, safe)
			}
			if got["id"] != id || got["label"] != id {
				t.Fatal("floor changed clean siblings", safe)
			}
			if strings.Contains(Normalize(safe), Normalize(token)) {
				t.Fatal("floor retained credential", safe)
			}
		}
	}
}
