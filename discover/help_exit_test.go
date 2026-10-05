package discover

import (
	"bytes"
	"strings"
	"testing"
)

// Asking for help is not a failure: usage prints and the exit code is 0,
// while a genuinely unknown flag still exits 2.
func TestDiscoverHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"save", "--help"}, {"brief", "--help"}, {"validate", "--help"}} {
		var out, errb bytes.Buffer
		if code := Command(args, strings.NewReader(""), &out, &errb); code != 0 {
			t.Errorf("%v: exit %d, want 0\n%s%s", args, code, out.String(), errb.String())
		}
		if !strings.Contains(out.String()+errb.String(), "Usage") {
			t.Errorf("%v: no usage printed", args)
		}
	}
	var out, errb bytes.Buffer
	if code := Command([]string{"--no-such-flag"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
}
