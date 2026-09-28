package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestInstallArgv(t *testing.T) {
	got, err := installArgv("claude", "user", "tap", "/opt/tap-runtime")
	if err != nil || strings.Join(got, " ") != "claude mcp add --scope user tap -- /opt/tap-runtime serve" {
		t.Fatalf("%v %v", got, err)
	}
	got, err = installArgv("codex", "user", "tap", "/opt/tap-runtime")
	if err != nil || strings.Join(got, " ") != "codex mcp add tap -- /opt/tap-runtime serve" {
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
