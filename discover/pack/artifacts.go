package pack

import (
	"fmt"
	"github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"
)

// Artifacts returns the draft's files, or ErrBlocked. Everything that writes
// or sends a draft goes through it.
func DraftArtifacts(d *model.Draft) (map[string][]byte, error) {
	if len(d.Blocked) > 0 {
		return nil, fmt.Errorf("%w: %s", model.ErrBlocked, strings.Join(d.Blocked, "; "))
	}
	files := make(map[string][]byte, len(d.Files)+1)
	for k, v := range d.Files {
		files[k] = v
	}
	if _, ok := files["CHANGELOG.md"]; !ok {
		m, err := manifest.Parse(files["primitive.yaml"])
		if err != nil {
			return nil, err
		}
		files["CHANGELOG.md"] = InitialChangelog(m.Metadata.Version)
	}
	return files, nil
}

func InitialChangelog(version string) []byte {
	return []byte("# Changelog\n\n## " + version + "\n\n- Initial generated primitive and declared input/output contract.\n- Validation: not run; review and test before use.\n")
}
