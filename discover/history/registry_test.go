package history

import (
	"sort"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/client"
)

// The registry's History flag and the reader map are two lists of one fact;
// this keeps them equal.
func TestEveryHistoryClientHasAReaderAndViceVersa(t *testing.T) {
	want := client.IDs(client.HasHistory)
	var got []string
	for id := range Readers {
		got = append(got, id)
		if _, ok := client.Lookup(id); !ok {
			t.Errorf("reader %q has no registry entry", id)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("readers %v, registry clients with history %v", got, want)
	}
	for id, r := range Readers {
		if c := r(t.TempDir()).Client(); c != id {
			t.Errorf("reader under %q reports client %q", id, c)
		}
	}
}

func TestDefaultReadersFollowTheRegistry(t *testing.T) {
	home := t.TempDir()
	rs, err := DefaultReaders(nil, home)
	if err != nil || len(rs) != 0 {
		t.Fatalf("empty home: %d readers, %v", len(rs), err)
	}
	rs, err = DefaultReaders([]string{"claude"}, home)
	if err != nil || len(rs) != 1 || rs[0].Client() != "claude-code" {
		t.Fatalf("alias: %v, %v", rs, err)
	}
	if _, err := DefaultReaders([]string{"windsurf"}, home); err == nil || strings.Contains(err.Error(), "unknown") {
		t.Fatalf("windsurf is known but has no reader yet: %v", err)
	}
}
