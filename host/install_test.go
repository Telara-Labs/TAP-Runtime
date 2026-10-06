package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallArgv(t *testing.T) {
	got, err := installArgv("claude", "user", "tap", "/opt/tap/tap", nil)
	if err != nil || strings.Join(got, " ") != "claude mcp add --scope user tap -- /opt/tap/tap serve" {
		t.Fatalf("%v %v", got, err)
	}
	got, err = installArgv("codex", "user", "tap", "/opt/tap/tap", nil)
	if err != nil || strings.Join(got, " ") != "codex mcp add tap -- /opt/tap/tap serve" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][2]string{{"", "user"}, {"cursor", "user"}, {"claude", "everywhere"}} {
		if _, err := installArgv(bad[0], bad[1], "tap", "/x", nil); err == nil {
			t.Errorf("client %q scope %q was accepted", bad[0], bad[1])
		}
	}
}

func TestInstallPrintChangesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := installCommand([]string{"--client", "claude", "--print"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "claude mcp add --scope user tap -- ") || !strings.HasSuffix(strings.TrimSpace(out.String()), " serve") {
		t.Fatalf("printed %q", out.String())
	}
}

func TestInstallReplacesAnEarlierRegistration(t *testing.T) {
	if got := strings.Join(removeArgv("claude", "user", "tap"), " "); got != "claude mcp remove --scope user tap" {
		t.Fatalf("claude remove = %q", got)
	}
	if got := strings.Join(removeArgv("codex", "user", "tap"), " "); got != "codex mcp remove tap" {
		t.Fatalf("codex remove = %q", got)
	}
	if removeArgv("gemini", "user", "tap") != nil {
		t.Fatal("gemini's settings entry is rewritten in place; there is nothing to remove")
	}
}

// --env passes the OpenTelemetry variables through each client's own
// configuration, and nothing else.
func TestInstallPassesOnlyOTelVariables(t *testing.T) {
	var env envFlags
	if err := env.Set("OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example"); err != nil {
		t.Fatal(err)
	}
	if err := env.Set("OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer k"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"PATH=/x", "AWS_SECRET=1", "OTEL_", "=v", "noequals"} {
		if err := (&envFlags{}).Set(bad); err == nil && bad != "OTEL_" {
			t.Errorf("--env %s was accepted", bad)
		}
	}
	got, err := installArgv("claude", "user", "tap", "/opt/tap", env)
	if err != nil {
		t.Fatal(err)
	}
	want := "claude mcp add --scope user -e OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example -e OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer k tap -- /opt/tap serve"
	if strings.Join(got, " ") != want {
		t.Errorf("claude:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	got, _ = installArgv("codex", "user", "tap", "/opt/tap", env)
	if !strings.Contains(strings.Join(got, " "), "--env OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example") {
		t.Errorf("codex: %v", got)
	}
	if printed := strings.Join(maskEnvArgs(got), " "); strings.Contains(printed, "Bearer") {
		t.Errorf("--print must not show a header value: %s", printed)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := addGemini(path, "tap", "/opt/tap", env); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example"`) {
		t.Errorf("gemini settings carry no env: %s", raw)
	}
}
