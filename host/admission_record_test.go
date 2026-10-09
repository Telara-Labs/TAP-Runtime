package main

import (
	"context"
	"strings"
	"testing"
	"time"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	runlog "github.com/Telara-Labs/TAP-Runtime/journal"
)

const admissionRecordManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: admission-record, version: 1.0.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: gmail.threads.search, effect: read}
`

func TestAdmissionRefusalHasTerminalStatusAndEvidence(t *testing.T) {
	for _, reason := range []string{"cancelled choice", "missing connector", "denied tool"} {
		t.Run(reason, func(t *testing.T) {
			c := startServer(t, true, accept)
			pkg := writePackage(t, admissionRecordManifest, "echo must-not-run\n")
			b := gmail()
			o := opts(t, pkg, c.runs)
			switch reason {
			case "cancelled choice":
				b = squatted()
				o.Choose = func(Pick) (string, bool) { return "", false }
			case "missing connector":
				b.inv = nil
			case "denied tool":
				b.deny["claude.ai Gmail/search_threads"] = true
			}
			o.Bridge = b
			if _, err := Run(context.Background(), o); err == nil {
				t.Fatal("admission unexpectedly succeeded")
			}
			if len(b.calls) != 0 {
				t.Fatalf("refused admission dispatched %v", b.calls)
			}
			id := onlyRun(t, c.runs)
			snap, err := runlog.Inspect(c.runs, id, 10)
			if err != nil {
				t.Fatal(err)
			}
			if snap.State != runlog.InspectFinished || snap.Outcome != "refused" || len(snap.Events) != 1 || snap.Events[0].Phase != "finish" {
				t.Fatalf("a known admission refusal looks interrupted: %+v", snap)
			}
			status := toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_status", "arguments": map[string]any{"run_id": id}}))
			if status["state"] != "finished" || status["outcome"] != "refused" {
				t.Fatalf("status = %#v", status)
			}
			evidence := toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_evidence", "arguments": map[string]any{"run_id": id}}))
			events, _ := evidence["events"].([]any)
			if evidence["state"] != "finished" || len(events) != 1 || events[0].(map[string]any)["outcome"] != "refused" {
				t.Fatalf("evidence = %#v", evidence)
			}
			o.Resume = id
			if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "finished") {
				t.Fatalf("a terminal refusal was resumed: %v", err)
			}
		})
	}
}

func TestFailedResumeAdmissionPreservesUnknownWrite(t *testing.T) {
	for _, reason := range []string{"cancelled choice", "missing connector", "denied tool"} {
		t.Run(reason, func(t *testing.T) {
			c := startServer(t, true, accept)
			pkg := writePackage(t, admissionRecordManifest, "echo must-not-run\n")
			digest, _, err := mf.RunDigest(pkg)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			id := runlog.NewRunID(now)
			j, err := runlog.Create(c.runs, runlog.Header{RunID: id, Package: pkg, PackageDigest: digest, Started: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := j.Begin("prior-write", "call", "prior-request-digest", "write", now); err != nil {
				j.Close()
				t.Fatal(err)
			}
			j.Close()
			b := gmail()
			o := opts(t, pkg, c.runs)
			o.Resume = id
			switch reason {
			case "cancelled choice":
				b = squatted()
				o.Choose = func(Pick) (string, bool) { return "", false }
			case "missing connector":
				b.inv = nil
			case "denied tool":
				b.deny["claude.ai Gmail/search_threads"] = true
			}
			o.Bridge = b
			if _, err := Run(context.Background(), o); err == nil {
				t.Fatal("resume admission unexpectedly succeeded")
			}
			if len(b.calls) != 0 {
				t.Fatalf("failed resume dispatched %v", b.calls)
			}
			snap, err := runlog.Inspect(c.runs, id, 10)
			if err != nil {
				t.Fatal(err)
			}
			if snap.State != runlog.InspectInterrupted || snap.Outcome != "" || len(snap.Events) != 1 || snap.Events[0].Phase != "begin" || snap.Events[0].Effect != "write" {
				t.Fatalf("failed admission concealed the unknown write: %+v", snap)
			}
			j, err = runlog.Open(c.runs, id)
			if err != nil {
				t.Fatalf("failed admission blocked later recovery: %v", err)
			}
			defer j.Close()
			state, _, err := j.Lookup("prior-write", "prior-request-digest")
			if err != nil || state != runlog.Interrupted || j.Effect("prior-write") != "write" {
				t.Fatalf("unknown write was changed: state=%v effect=%q err=%v", state, j.Effect("prior-write"), err)
			}
		})
	}
}
