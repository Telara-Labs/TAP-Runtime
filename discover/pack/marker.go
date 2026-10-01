package pack

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
