package routine

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/redact"
	"gopkg.in/yaml.v3"
)

func TestCredentialInputSchemaKeepsMetadataAndPassesFrozenScanner(t *testing.T) {
	for _, name := range []string{"api_key", "vault_write_token", "password", "refresh_token"} {
		source := map[string]any{"properties": map[string]any{name: draftInputProperty{Type: "string", Description: "ordinary schema description"}}}
		raw, err := yaml.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte("type: string")) || !bytes.Contains(raw, []byte("description: ordinary schema description")) {
			t.Fatalf("metadata lost %s", raw)
		}
		if redact.SecretShape(string(raw)) != "" || len(redact.ScanArtifacts(map[string][]byte{"primitive.yaml": raw})) != 0 {
			t.Fatalf("generated metadata matches frozen credential expression %s", raw)
		}
		var got, want map[string]any
		if err := yaml.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		ordinary, _ := yaml.Marshal(map[string]any{"properties": map[string]any{name: map[string]any{"type": "string", "description": "ordinary schema description"}}})
		if err := yaml.Unmarshal(ordinary, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("schema semantics changed")
		}
		dirty := append(append([]byte(nil), raw...), []byte("password: syntheticOpaqueValue42\n")...)
		if redact.SecretShape(string(dirty)) == "" {
			t.Fatal("metadata formatting hid an actual credential")
		}
	}
}
