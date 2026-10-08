package author_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/author"
)

// Exercise the real history reader and private brief writer on synthetic
// session data, not the person's history or a mocked authoring pipeline.
func TestBriefWriterRemovesOpaqueCredentialsAndEntirePEMBodies(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "synthetic")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	pem := "-----BEGIN " + "RSA PRIVATE KEY-----\nSYNTHETIC-PRIVATE-BODY\n-----END RSA PRIVATE KEY-----"
	lines := []any{
		map[string]any{"type": "user", "message": map[string]any{"content": "Inspect the synthetic example\n" + pem}},
		map[string]any{"type": "assistant", "message": map[string]any{"id": "msg1", "content": []any{map[string]any{"type": "tool_use", "id": "call1", "name": "mcp__synthetic__inspect", "input": map[string]any{
			"password": "synthetic-password", "token": "x", "api_key": "synthetic-api-key", "issue_key": "ABC-12", "a_token": "${SYNTHETIC_TOKEN}",
			"config": map[string]any{"env": map[string]any{"DB_PASSWORD": "synthetic-env-password", "API_TOKEN": "short", "REGION": "west", "PATH": "/ordinary/bin", "REFRESH_TOKEN": "${SYNTHETIC_TOKEN}"}, "limit": 5},
		}}}}},
		map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call1", "content": pem}}}},
		map[string]any{"type": "assistant", "message": map[string]any{"id": "msg2", "content": []any{map[string]any{"type": "tool_use", "id": "call2", "name": "Bash", "input": map[string]any{"command": "export DB_PASSWORD=abc; API_TOKEN=q REGION=west PATH=/ordinary/bin tool status"}}}}},
	}
	var fixture strings.Builder
	for _, line := range lines {
		raw, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Write(raw)
		fixture.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(project, "synthetic.jsonl"), []byte(fixture.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := author.FindSession("claude-code", "synthetic", home)
	if err != nil {
		t.Fatal(err)
	}
	brief, err := author.NewBrief(session, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "brief")
	if _, err := brief.Write(out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"brief.json", "BRIEF.md"} {
		raw, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, secret := range []string{"synthetic-password", "synthetic-api-key", "synthetic-env-password", "SYNTHETIC-PRIVATE-BODY", "DB_PASSWORD=abc", "API_TOKEN=q", "short"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s retains synthetic credential %q", name, secret)
			}
		}
		for _, normal := range []string{"ABC-12", "west", "/ordinary/bin", "tool status", "${SYNTHETIC_TOKEN}"} {
			if !strings.Contains(text, normal) {
				t.Errorf("%s removed ordinary context %q", name, normal)
			}
		}
	}
	if brief.Evidence.Steps[0].Args["token"] == "x" {
		t.Fatal("a one-character opaque credential reached the brief")
	}
	if len(brief.Evidence.Steps) != 2 || !strings.Contains(brief.Evidence.Steps[0].Output, "redacted") {
		t.Fatalf("calls/result evidence changed beyond redaction: %+v", brief.Evidence.Steps)
	}
}
