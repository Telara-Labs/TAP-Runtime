package redact

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestSecretShapeNamesTheShape(t *testing.T) {
	for in, want := range map[string]string{
		fakeBearer:                       "bearer token",
		fakeGitlab:                       "GitLab token",
		fakeJWT:                          "JWT",
		fakeKey:                          "AWS access key",
		"-----BEGIN EC PRIVATE KEY-----": "private key",
		"ghp_" + strings.Repeat("a", 36): "GitHub token",
		"plain text with no credential":  "",
		"issue_key=TENG-1":               "",
	} {
		if got := SecretShape(in); got != want {
			t.Errorf("SecretShape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSensitiveSlot(t *testing.T) {
	cases := []struct {
		label string
		slot  trace.Slot
		want  bool
	}{
		// The name says it carries a credential.
		{"mcp:login", trace.Slot{Key: "password", Type: trace.SlotText, Value: "x"}, true},
		{"mcp:call", trace.Slot{Key: "api_key", Type: trace.SlotText, Value: "x"}, true},
		{"sh:tool", trace.Slot{Key: "--access-token=", Type: trace.SlotText, Value: "x"}, true},
		{"sh:tool", trace.Slot{Key: "--client-secret#2", Type: trace.SlotText, Value: "x"}, true},
		// Names that only look like it.
		{"mcp:jira", trace.Slot{Key: "issue_key", Type: trace.SlotText, Value: "TENG-1"}, false},
		{"mcp:llm", trace.Slot{Key: "max_output_tokens", Type: trace.SlotNumber, Value: "4000"}, false},
		// -u is a user:password only for programs where it means a user.
		{"sh:curl", trace.Slot{Key: "-u=", Type: trace.SlotText, Value: "admin:hunter2"}, true},
		{"sh:wget", trace.Slot{Key: "--user=", Type: trace.SlotText, Value: "admin:hunter2"}, true},
		{"sh:curl", trace.Slot{Key: "-u=", Type: trace.SlotText, Value: "admin"}, false},
		{"sh:sort", trace.Slot{Key: "-u=", Type: trace.SlotText, Value: "a:b"}, false},
		// A credential by shape, whatever the name.
		{"mcp:call", trace.Slot{Key: "header", Type: trace.SlotText, Value: fakeBearer}, true},
		// The subcommand and bare flags are never values.
		{"sh:git", trace.Slot{Key: "password", Type: trace.SlotText, Value: "x", Sub: true}, false},
		{"sh:tool", trace.Slot{Key: "--password", Type: trace.SlotFlag}, false},
	}
	for _, c := range cases {
		if got := SensitiveSlot(c.label, c.slot); got != c.want {
			t.Errorf("SensitiveSlot(%q, %+v) = %v, want %v", c.label, c.slot, got, c.want)
		}
	}
}

func TestScanArtifactsNamesLinesNotValues(t *testing.T) {
	got := ScanArtifacts(map[string][]byte{
		"b/main.py":   []byte("import os\nTOKEN = '" + fakeGitlab + "'\n"),
		"a/README.md": []byte("# Title\n\nNothing here.\n"),
		"a/run.sh":    []byte("curl -H 'Authorization: " + fakeBearer + "' https://x\n"),
	})
	want := []string{"a/run.sh line 1: bearer token", "b/main.py line 2: GitLab token"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanArtifacts = %q, want %q", got, want)
	}
	for _, f := range got {
		if strings.Contains(f, fakeGitlab) || strings.Contains(f, fakeBearer) {
			t.Errorf("a finding repeats the credential: %q", f)
		}
	}
	if got := ScanArtifacts(nil); len(got) != 0 {
		t.Errorf("ScanArtifacts(nil) = %q", got)
	}
}
