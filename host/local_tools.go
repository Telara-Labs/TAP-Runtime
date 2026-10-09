package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/journal"
)

var searchTool = localTool("tap_search", "Call this first, at the start of every request to look something up, check something or do something, even one that names a specific commit, ticket or file, or that one step might answer. Pass a few words describing the kind of task, without its specific values. It returns a saved primitive that does the task in one step, or says whether the person has asked for this kind of task before; only this tool can tell you that. The user does not need to mention TAP. Read-only and instant; it runs nothing.", map[string]any{
	"query":  map[string]any{"type": "string", "description": "Words in the primitive reference or description."},
	"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
	"detail": detailArgument,
}, nil, true)

var loadTool = localTool("tap_load", "Load an exact local TAP primitive's declarations and a bounded preview of possible host connections and write gates. Runs no tools; execution rechecks bindings and dynamic effects.", map[string]any{
	"ref": map[string]any{"type": "string"}, "digest": map[string]any{"type": "string"},
	"detail": detailArgument,
}, []string{"ref", "digest"}, true)

var statusTool = localTool("tap_status", "Read the current state of a local TAP run without resuming it.", map[string]any{
	"run_id": map[string]any{"type": "string"},
	"detail": detailArgument,
}, []string{"run_id"}, true)

var evidenceTool = localTool("tap_evidence", "Read bounded run provenance and journaled request metadata. Exact saved manifest is opt-in; runtime arguments and results are excluded.", map[string]any{
	"run_id":           map[string]any{"type": "string"},
	"limit":            map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
	"include_manifest": map[string]any{"type": "boolean", "description": "Include exact saved YAML if it fits. May contain sensitive package defaults or examples."},
	"detail":           detailArgument,
}, []string{"run_id"}, true)

var detailArgument = map[string]any{"type": "boolean", "description": "Return complete JSON instead of compact readable text. Use for programmatic inspection; default false."}

func localTool(name, description string, properties map[string]any, required []string, readOnly bool) map[string]any {
	// JSON Schema's required is an array; null makes Claude Code reject the
	// whole tool list ("tools.0.inputSchema.required: expected array").
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"name": name, "description": description,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
		"annotations": map[string]any{"readOnlyHint": readOnly, "destructiveHint": false, "openWorldHint": false},
	}
}

func (s *server) catalog() ([]catalogEntry, error) {
	if s.catalogRoot != "" {
		return localCatalogIn(s.proc.Wd(), s.catalogRoot)
	}
	return localCatalogIn(s.proc.Wd())
}

func (s *server) resolveRunPackage(ref, digest, legacyPath string) (string, error) {
	if legacyPath != "" {
		if s.allowPackagePath && ref == "" && digest == "" {
			return legacyPath, nil
		}
		return "", fmt.Errorf("tap_run accepts an installed ref and digest, not a package path")
	}
	if ref == "" || digest == "" {
		return "", fmt.Errorf("tap_run needs an exact ref and digest from tap_search")
	}
	entries, err := s.catalog()
	if err != nil {
		return "", err
	}
	entry, err := resolveCatalog(entries, ref, digest)
	if err != nil {
		return "", err
	}
	return entry.Path, nil
}

func (s *server) toolError(id *json.RawMessage, message string) {
	s.reply(id, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": message}}})
}

// withoutClientScheduling drops wait_for_previous, which Gemini CLI (0.63)
// adds to every tool's schema and then sends to the server with the call
// (packages/core/src/tools/tools.ts). It only orders the client's own calls.
// Read strictly, it made tap_save and tap_run refuse Gemini's calls.
func withoutClientScheduling(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	v, ok := m["wait_for_previous"]
	if !ok {
		return raw
	}
	var b bool
	if json.Unmarshal(v, &b) != nil {
		return raw
	}
	delete(m, "wait_for_previous")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func readToolArgs(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	raw = withoutClientScheduling(raw)
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid arguments: trailing JSON")
	}
	return nil
}

func (s *server) runRoot() (string, error) {
	if s.runsDir != "" {
		return s.runsDir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tap-runtime", "runs"), nil
}

func (s *server) handleReadTool(id *json.RawMessage, name string, raw json.RawMessage) {
	switch name {
	case "tap_search":
		var a struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Detail bool   `json:"detail"`
		}
		if err := readToolArgs(raw, &a); err != nil {
			s.toolError(id, err.Error())
			return
		}
		if len(a.Query) > 256 || a.Limit < 0 || a.Limit > 20 {
			s.toolError(id, "query must be at most 256 bytes and limit 1 through 20")
			return
		}
		if a.Limit == 0 {
			a.Limit = 20
		}
		entries, err := s.catalog()
		if err != nil {
			s.toolError(id, err.Error())
			return
		}
		type hit struct {
			Ref         string `json:"ref"`
			Digest      string `json:"digest"`
			Description string `json:"description,omitempty"`
			Source      string `json:"source"`
		}
		hits := make([]hit, 0)
		for _, e := range searchCatalog(entries, a.Query, a.Limit) {
			desc := e.Description
			if len(desc) > 1024 {
				desc = desc[:1024]
			}
			hits = append(hits, hit{e.Ref, e.Digest, desc, e.Source})
		}
		reply := map[string]any{"matches": hits}
		if len(hits) == 0 {
			s.mu.Lock()
			h := s.history
			s.mu.Unlock()
			reply["note"] = noMatchNote(a.Query, h)
		}
		s.toolOutput(id, name, reply, a.Detail)
	case "tap_load":
		var a struct {
			Ref    string `json:"ref"`
			Digest string `json:"digest"`
			Detail bool   `json:"detail"`
		}
		if err := readToolArgs(raw, &a); err != nil || a.Ref == "" || a.Digest == "" {
			s.toolError(id, "tap_load needs ref and digest")
			return
		}
		entries, err := s.catalog()
		if err != nil {
			s.toolError(id, err.Error())
			return
		}
		e, err := resolveCatalog(entries, a.Ref, a.Digest)
		if err != nil {
			s.toolError(id, err.Error())
			return
		}
		m := e.Manifest
		loaded := map[string]any{"ref": e.Ref, "digest": e.Digest, "description": e.Description,
			"interface": m.Interface, "capabilities": m.Capabilities, "tools": m.Tools,
			"commands": m.Commands, "files": m.Files, "fetch": m.Fetch,
			"connection_preview": s.previewConnections(m)}
		if checkRunArgs(m, nil) != "" {
			loaded["args"] = "one element: the input object encoded as a JSON string in args[0]"
		}
		s.toolOutput(id, name, loaded, a.Detail)
	case "tap_status", "tap_evidence":
		var a struct {
			RunID           string `json:"run_id"`
			Limit           int    `json:"limit"`
			IncludeManifest bool   `json:"include_manifest"`
			Detail          bool   `json:"detail"`
		}
		if err := readToolArgs(raw, &a); err != nil || a.RunID == "" {
			s.toolError(id, name+" needs run_id")
			return
		}
		if name == "tap_status" && (a.Limit != 0 || a.IncludeManifest) {
			s.toolError(id, "tap_status does not accept limit or include_manifest")
			return
		}
		if name == "tap_evidence" && (a.Limit < 0 || a.Limit > 100) {
			s.toolError(id, "limit must be 1 through 100")
			return
		}
		root, err := s.runRoot()
		if err != nil {
			s.toolError(id, err.Error())
			return
		}
		limit := a.Limit
		if name == "tap_status" {
			limit = 1
		}
		snap, err := journal.Inspect(root, a.RunID, limit)
		if err != nil {
			s.toolError(id, err.Error())
			return
		}
		if name == "tap_status" {
			s.toolOutput(id, name, map[string]any{"run_id": snap.Header.RunID, "package_digest": snap.Header.PackageDigest,
				"started": snap.Header.Started, "state": snap.State, "outcome": snap.Outcome}, a.Detail)
		} else {
			evidence, err := runEvidence(root, snap, a.IncludeManifest)
			if err != nil {
				s.toolError(id, err.Error())
				return
			}
			s.toolOutput(id, name, evidence, a.Detail)
		}
	default:
		if strings.HasPrefix(name, "tap_") {
			s.fail(id, -32601, "unknown TAP tool")
		} else {
			s.fail(id, -32601, "tool not found")
		}
	}
}
