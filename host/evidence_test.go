package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/journal"
)

func evidenceSnapshot(t *testing.T, raw string) (string, journal.Snapshot) {
	t.Helper()
	root := t.TempDir()
	j, err := journal.CreateWithManifest(root, journal.Header{RunID: "run-evidence-bounded", PackageDigest: "package-digest", Started: time.Now()}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err := journal.Inspect(root, j.Header.RunID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return root, snap
}

const evidenceManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: evidence, version: 1.0.0}
execution: {entrypoint: main.sh}
interface:
  inputSchema:
    type: object
    properties:
      credential: {type: string, default: sensitive-package-default}
tools: [{alias: lookup, capability: records.lookup, effect: read}]
files: [{path: '*.csv', access: read}]
fetch: [{origin: 'https://example.test', methods: [GET]}]
`

func TestRunEvidencePermissionsAndOptInManifest(t *testing.T) {
	root, snap := evidenceSnapshot(t, evidenceManifest)
	got, err := runEvidence(root, snap, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "sensitive-package-default") {
		t.Fatal("default evidence disclosed manifest values")
	}
	permissions := got["declared_permissions"].(map[string]any)
	if got["package_digest"] != "package-digest" || got["permissions_status"] != "available" || permissions["tools"] == nil || permissions["files"] == nil || permissions["fetch"] == nil {
		t.Fatalf("missing permissions %#v", got)
	}
	got, err = runEvidence(root, snap, true)
	if err != nil || got["manifest"].(map[string]any)["yaml"] != evidenceManifest {
		t.Fatalf("exact opt-in snapshot %#v %v", got, err)
	}
}

func TestRunEvidenceBoundsAndLegacy(t *testing.T) {
	// Escaped content consumes the JSON budget faster than its source bytes.
	large := strings.Replace(evidenceManifest, "access: read", "access: read", 1) + "# " + strings.Repeat("<", 20000) + "\n"
	root, snap := evidenceSnapshot(t, large)
	for i := 0; i < 100; i++ {
		snap.Events = append(snap.Events, journal.Event{Phase: "begin", ID: "request", Call: &journal.CallIdentity{Server: strings.Repeat("s", 1024), Tool: strings.Repeat("t", 1024)}})
	}
	got, err := runEvidence(root, snap, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	if len(encoded) > 60<<10 || got["truncated"] != true || got["manifest"].(map[string]any)["content_status"] != "omitted_size_limit" || got["package_digest"] != "package-digest" {
		t.Fatalf("unbounded or implicit truncation %d %#v", len(encoded), got)
	}
	// A large declaration list is explicitly omitted, not shown as empty.
	large = strings.Replace(evidenceManifest, "'*.csv'", "'"+strings.Repeat("x", 20000)+"'", 1)
	root, snap = evidenceSnapshot(t, large)
	got, err = runEvidence(root, snap, false)
	if err != nil || got["permissions_status"] != "omitted_size_limit" || got["declared_permissions"] != nil {
		t.Fatalf("oversized permissions hidden %#v %v", got, err)
	}
	legacy := journal.Snapshot{Header: journal.Header{RunID: "run-legacy-evidence", PackageDigest: "old-digest"}, Events: []journal.Event{}}
	got, err = runEvidence(t.TempDir(), legacy, true)
	if err != nil || got["permissions_status"] != "unavailable" || got["manifest"].(map[string]any)["status"] != "unavailable" || got["package_digest"] != "old-digest" {
		t.Fatalf("legacy context fabricated %#v %v", got, err)
	}
}
