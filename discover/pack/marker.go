package pack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Marker struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	// Validation is "not_run": saving is not validating. A validation
	// result names the exact digest it passed for.
	Validation string `json:"validation"`
	// Origin, Receipts and Cases are set for a package a host agent
	// authored (save.go); a saved draft leaves them out.
	Origin   string `json:"origin,omitempty"`
	Receipts string `json:"receipts,omitempty"`
	Cases    int    `json:"cases,omitempty"`
}

// ReadMarker reads the SavedMarker of a saved primitive's folder. A folder
// without a valid one is not a saved primitive.
func ReadMarker(dir string) (Marker, error) {
	var m Marker
	b, err := os.ReadFile(filepath.Join(dir, SavedMarker))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Name == "" || m.Digest == "" {
		return m, fmt.Errorf("%s: incomplete marker", dir)
	}
	return m, nil
}
