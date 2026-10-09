package main

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
)

var liveReadableClients = flag.Bool("live-readable-clients", false, "check readable inspection through installed Claude/Codex MCP control channels; no model turns or connector calls")

func TestLiveReadableClientOutput(t *testing.T) {
	if !*liveReadableClients {
		t.Skip("pass -live-readable-clients to check installed MCP clients")
	}
	claude, err := bridge.ClaudeExecutable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	catalog := filepath.Join(root, "catalog")
	pkg := filepath.Join(catalog, "readable-output-fixture")
	if err := os.MkdirAll(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"primitive.yaml": inputManifest, "main.sh": "echo never-executed\n"} {
		if err := os.WriteFile(filepath.Join(pkg, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := readCatalogEntry(pkg, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{"claude", "codex"} {
		t.Run(client, func(t *testing.T) {
			work := filepath.Join(root, client)
			if err := os.MkdirAll(work, 0700); err != nil {
				t.Fatal(err)
			}
			log, err := os.Create(filepath.Join(work, "transcript.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			args := []string{"serve", "--catalog-root", catalog, "--config-dir", filepath.Join(work, "config"), "--runs", filepath.Join(work, "runs"), "--journal", filepath.Join(work, "journal.jsonl")}
			var caller *acceptanceClient
			if client == "claude" {
				caller, err = startClaudeAcceptance(t, claude, bin, args, work, log, "tap_readable", "", "")
			} else {
				caller, err = startCodexAcceptance(t, bin, args, work, log, "tap_readable", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer caller.Close()
			search, err := caller.Call("tap_search", map[string]any{"query": "example.test/release-check"})
			if err != nil || !strings.Contains(search, entry.Ref) || !strings.Contains(search, entry.Digest) {
				t.Fatalf("search: %v %s", err, search)
			}
			identity := map[string]any{"ref": entry.Ref, "digest": entry.Digest}
			load, err := caller.Call("tap_load", identity)
			if err != nil || !strings.Contains(load, "candidate (required)") || !strings.Contains(load, "args[0]") || strings.Contains(load, `\"`) {
				t.Fatalf("load: %v %s", err, load)
			}
			full, err := caller.Call("tap_load", map[string]any{"ref": entry.Ref, "digest": entry.Digest, "detail": true})
			var object map[string]any
			if err != nil || json.Unmarshal([]byte(full), &object) != nil || object["digest"] != entry.Digest || object["interface"] == nil {
				t.Fatalf("JSON compatibility: %v %s", err, full)
			}
			t.Logf("%s MCP front door preserved readable search/load and opt-in JSON; no model turn, primitive execution or connector call", client)
			t.Log(load)
		})
	}
}
