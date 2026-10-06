package history

import (
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// Each reader that reports progress reads the same sessions as Read, counts
// up from 0 to its total in order, and ends at total of total.
func TestProgressReadersCountEverySessionFile(t *testing.T) {
	for name, r := range map[string]trace.ProgressReader{
		"claude-code": ClaudeCode{Dir: "testdata/claude"},
		"codex":       Codex{Dir: "testdata/codex"},
		"gemini-cli":  GeminiCLI{Dir: "testdata/gemini-cli/tmp"},
	} {
		plain, err := r.Read(time.Time{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var seen [][2]int
		got, err := r.ReadProgress(time.Time{}, func(done, total int) { seen = append(seen, [2]int{done, total}) })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != len(plain) {
			t.Errorf("%s: ReadProgress found %d sessions, Read found %d", name, len(got), len(plain))
		}
		if len(seen) == 0 {
			t.Errorf("%s: no progress reported", name)
			continue
		}
		last := seen[len(seen)-1]
		if last[0] != last[1] || last[1] == 0 {
			t.Errorf("%s: progress ended at %d/%d", name, last[0], last[1])
		}
		for i := 1; i < len(seen); i++ {
			if seen[i][0] < seen[i-1][0] || seen[i][1] != last[1] {
				t.Errorf("%s: progress went %v then %v", name, seen[i-1], seen[i])
			}
		}
	}
}

// A cutoff after every file leaves nothing to read, and says so.
func TestProgressReaderSkipsOldFiles(t *testing.T) {
	var last [2]int
	ss, err := ClaudeCode{Dir: "testdata/claude"}.ReadProgress(time.Now().Add(24*time.Hour), func(d, n int) { last = [2]int{d, n} })
	if err != nil || len(ss) != 0 || last != [2]int{0, 0} {
		t.Fatalf("future cutoff: %d sessions, progress %v, err %v", len(ss), last, err)
	}
}
