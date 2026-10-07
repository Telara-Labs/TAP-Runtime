package main

import (
	"encoding/json"
	"fmt"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/Telara-Labs/TAP-Runtime/journal"
)

// runEvidence deliberately excludes the header's package path and arguments,
// request bodies and replies. Declarations are intent; begin/end events describe
// attempts and outcomes recorded by this local host, not correctness of results.
func runEvidence(root string, snap journal.Snapshot, includeManifest bool) (map[string]any, error) {
	manifest := map[string]any{"status": "unavailable", "reason": "legacy journal has no saved manifest"}
	evidence := map[string]any{
		"run_id": snap.Header.RunID, "package_digest": snap.Header.PackageDigest,
		"started": snap.Header.Started, "state": snap.State, "outcome": snap.Outcome,
		"events": snap.Events, "truncated": snap.Truncated,
		"evidence_version": snap.Header.EvidenceVersion, "manifest": manifest,
		"permissions_status": "unavailable", "trace_scope": "journaled requests only",
	}
	raw, err := journal.ReadManifest(root, snap.Header)
	if err != nil {
		return nil, err
	}
	if raw != nil {
		m, err := mf.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("saved manifest cannot be read: %w", err)
		}
		manifest["status"], manifest["digest"], manifest["bytes"] = "available", snap.Header.ManifestDigest, len(raw)
		delete(manifest, "reason")
		evidence["ref"] = fmt.Sprintf("%s/%s@%s", m.Metadata.Publisher, m.Metadata.Name, m.Metadata.Version)
		permissions := map[string]any{"tools": m.Tools, "commands": m.Commands, "files": m.Files, "fetch": m.Fetch}
		if encoded, err := json.Marshal(permissions); err == nil && len(encoded) <= 16<<10 {
			evidence["declared_permissions"], evidence["permissions_status"] = permissions, "available"
		} else {
			evidence["permissions_status"] = "omitted_size_limit"
		}
		manifest["content_status"] = "not_requested"
		if includeManifest {
			// Budget encoded JSON, not source bytes: escaping can multiply size.
			if encoded, err := json.Marshal(string(raw)); err == nil && len(encoded) <= 16<<10 {
				manifest["yaml"], manifest["content_status"] = string(raw), "included"
			} else {
				manifest["content_status"] = "omitted_size_limit"
			}
		}
	}
	// Keep useful identity even when many long events exceed the MCP budget.
	// Omit whole events rather than silently shortening their identity.
	events := snap.Events
	for {
		encoded, err := json.Marshal(evidence)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= 60<<10 {
			return evidence, nil
		}
		if len(events) == 0 {
			return nil, fmt.Errorf("run evidence metadata exceeds output limit")
		}
		events = events[:len(events)-1]
		evidence["events"], evidence["truncated"] = events, true
	}
}
