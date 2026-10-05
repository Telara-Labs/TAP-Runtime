package pack

import (
	"fmt"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"
)

// Artifacts returns the draft's files, or ErrBlocked. Everything that writes
// or sends a draft goes through it.
func DraftArtifacts(d *model.Draft) (map[string][]byte, error) {
	if len(d.Blocked) > 0 {
		return nil, fmt.Errorf("%w: %s", model.ErrBlocked, strings.Join(d.Blocked, "; "))
	}
	return d.Files, nil
}
