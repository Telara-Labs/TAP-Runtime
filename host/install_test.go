package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestInstallArgv(t *testing.T) {
	got, err := installArgv("claude", "user", "tap", "/opt/tap/tap")
	if err != nil || strings.Join(got, " ") != "claude mcp add --scope user tap -- /opt/tap/tap serve" {
		t.Fatalf("%v %v", got, err)
	}
	got, err = installArgv("codex", "user", "tap", "/opt/tap/tap")
	if err != nil || strings.Join(got, " ") != "codex mcp add tap -- /opt/tap/tap serve" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][2]string{{"", "user"}, {"cursor", "user"}, {"claude", "everywhere"}} {
		if _, err := installArgv(bad[0], bad[1], "tap", "/x"); err == nil {
			t.Errorf("client %q scope %q was accepted", bad[0], bad[1])
		}
	}
}

func TestInstallPrintChangesNothing(t *testing.T) {
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
