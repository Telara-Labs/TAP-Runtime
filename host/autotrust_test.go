package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

func TestReadsOnly(t *testing.T) {
	for _, c := range []struct {
		m    mf.Manifest
		want bool
	}{
		{mf.Manifest{Fetch: []mf.Fetch{{Origin: "https://gitlab.com"}}}, true},
		{mf.Manifest{Fetch: []mf.Fetch{{Origin: "https://gitlab.com", Methods: []string{"GET", "HEAD"}}}}, true},
		{mf.Manifest{Fetch: []mf.Fetch{{Origin: "https://gitlab.com", Methods: []string{"POST"}}}}, false},
		{mf.Manifest{Files: []mf.File{{Path: "in", Access: "read"}}}, true},
		{mf.Manifest{Files: []mf.File{{Path: "out", Access: "write"}}}, false},
		{mf.Manifest{Tools: []mf.Tool{{Alias: "a", Effect: "read"}, {Alias: "b", Effect: "write"}}}, false},
		{mf.Manifest{Commands: []mf.Command{{Command: "git", Effect: "read"}}}, false},
	} {
		if got := readsOnly(&c.m); got != c.want {
			t.Errorf("%+v: readsOnly %v, want %v", c.m, got, c.want)
		}
	}
}

func writeHomeFile(t *testing.T, home, rel, body string) {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEachAgentsOwnWebPermissionIsRead(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		client, file, body string
		want               bool
	}{
		{"opencode", ".config/opencode/opencode.json", `{"mcp":{}}`, true},
		{"opencode", ".config/opencode/opencode.json", `{"permission":{"webfetch":"ask"}}`, false},
		{"kilo", ".config/kilo/kilo.json", `{"permission":{"webfetch":"allow"}}`, true},
		{"crush", ".config/crush/crush.json", `{"permissions":{"allowed_tools":["fetch"]}}`, true},
		{"crush", ".config/crush/crush.json", `{}`, false},
		{"goose-cli", ".config/goose/config.yaml", "GOOSE_MODE: auto\n", true},
		{"goose-cli", ".config/goose/config.yaml", "GOOSE_MODE: approve\n", false},
	}
	for _, c := range cases {
		writeHomeFile(t, home, c.file, c.body)
		if got, why := agentReadsWebUnasked(c.client, home); got != c.want {
			t.Errorf("%s %s: %v (%s), want %v", c.client, c.body, got, why, c.want)
		}
	}
	for _, unknown := range []string{"test-client", "gemini-cli-mcp-client"} {
		if got, _ := agentReadsWebUnasked(unknown, home); got {
			t.Errorf("%s allowed web reads", unknown)
		}
	}
}

// OpenCode cannot show a prompt, and its default config lets its model fetch
// unasked: a reads-only primitive runs there without tap trust. Its config
// set to ask, or a primitive that posts, is refused as before.
func TestAReadsOnlyPrimitiveRunsInAnAgentThatCannotPrompt(t *testing.T) {
	inDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHomeFile(t, home, ".config/opencode/opencode.json", `{"mcp":{}}`)
	old := testClientName
	testClientName = "opencode"
	t.Cleanup(func() { testClientName = old })
	c := startServer(t, false, nil)
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Add(1); fmt.Fprint(w, "read-ok") }))
	defer srv.Close()
	reads := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: reads, version: 1.0.0}\nexecution: {entrypoint: main.sh}\nfetch:\n  - {origin: "+srv.URL+"}\n", "tap fetch "+srv.URL+"/probe\n")
	if out := c.run(reads); !strings.Contains(out, "read-ok") || seen.Load() != 1 {
		t.Fatalf("reads-only primitive did not run: %s", out)
	}
	posts := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: posts, version: 1.0.0}\nexecution: {entrypoint: main.sh}\nfetch:\n  - {origin: "+srv.URL+", methods: [GET, POST]}\n", "tap fetch "+srv.URL+"/probe\n")
	c.run(posts)
	if seen.Load() != 1 {
		t.Fatal("a primitive that may post ran without anyone agreeing")
	}
	writeHomeFile(t, home, ".config/opencode/opencode.json", `{"permission":{"webfetch":"ask"}}`)
	c.run(reads)
	if seen.Load() != 1 {
		t.Fatal("ran although the agent asks before web reads")
	}
}
