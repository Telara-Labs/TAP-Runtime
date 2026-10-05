package discover

import (
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/author"
	"github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/history"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

// Every call site that takes a client name accepts every registered client:
// it works, or it says the client lacks that capability. "unknown client"
// would mean a call site keeps its own list again (TENG-3108).
func TestEveryCallSiteAcceptsEveryRegisteredClient(t *testing.T) {
	home := t.TempDir()
	for _, c := range client.All() {
		for _, name := range append([]string{c.ID}, c.Aliases...) {
			for site, err := range map[string]error{
				"history.DefaultReaders": func() error { _, err := history.DefaultReaders([]string{name}, home); return err }(),
				"pack.SkillsDir":         func() error { _, err := pack.SkillsDir(name, false, home, home); return err }(),
				"author.FindSession":     func() error { _, err := author.FindSession(name, "none", home); return err }(),
			} {
				if err != nil && strings.Contains(err.Error(), "unknown client") {
					t.Errorf("%s(%q): %v", site, name, err)
				}
			}
		}
	}
}
