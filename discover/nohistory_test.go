package discover

import (
	"bytes"
	"strings"
	"testing"
)

// With nothing to read, discover says what it looked for and what to do,
// instead of printing an empty report.
func TestDiscoverWithNoHistorySaysWhatToDo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var out, errb bytes.Buffer
	if code := Command([]string{"--all"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if s := out.String(); !strings.Contains(s, "No agent history found on this machine.") || !strings.Contains(s, "claude-code") || strings.Contains(s, "Summary") {
		t.Errorf("no agents detected:\n%s", s)
	}

	out.Reset()
	if code := Command([]string{"--all", "--client", "codex", "--days", "7"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if s := out.String(); !strings.Contains(s, "No sessions from the last 7 days in the history of: codex.") || !strings.Contains(s, "drop --days") {
		t.Errorf("one agent named, nothing in it:\n%s", s)
	}
}

func TestWriteNoHistoryWithoutDays(t *testing.T) {
	var out bytes.Buffer
	writeNoHistory(&out, []string{"claude-code", "codex"}, 0)
	s := out.String()
	if !strings.HasPrefix(s, "No sessions in the history of: claude-code, codex.") || strings.Contains(s, "--days") {
		t.Errorf("got:\n%s", s)
	}
}
