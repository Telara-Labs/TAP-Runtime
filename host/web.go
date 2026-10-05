package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

//go:embed webassets/worker.html
var webWorkerPage string

//go:embed webassets/tapweb.js
var webRunner string

// webCommand is `host web build`: it writes one web page that runs the given
// primitives inside claude.ai, as an Artifact. The page carries the JavaScript
// interpreter and the guest SDK the runner uses, and answers a primitive's
// tool calls with the viewer's own claude.ai connectors. A chat starts a
// primitive by adding a job to the page's database and reads the answer from
// the same place (TENG-3056).
//
//	host web build --out tap-worker.html pkg/recent-mail-web [more packages]
//
// It also prints the capabilities the page must be published with: an
// Artifact may call only the connector tools it declares.
func webCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "build" {
		fmt.Fprintln(stderr, "usage: host web build --out FILE <package-dir>...")
		return 2
	}
	fs := flag.NewFlagSet("web build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "tap-worker.html", "the page to write")
	interpDir := fs.String("interpreters", "", "interpreter store; default is the user cache directory")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "name at least one primitive package")
		return 2
	}
	store, err := storeDir(*interpDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	page, caps, err := buildWebPage(store, fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := os.WriteFile(*out, page, 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	b, _ := json.MarshalIndent(caps, "", "  ")
	fmt.Fprintf(stdout, "wrote %s (%d bytes)\n\nPublish it as a claude.ai Artifact with these capabilities:\n%s\n", *out, len(page), b)
	return 0
}

// webPrimitive is one primitive as the page holds it.
type webPrimitive struct {
	Version     string                `json:"version"`
	Description string                `json:"description,omitempty"`
	Bindings    map[string]webBinding `json:"bindings"`
	Program     string                `json:"program"`
}

type webBinding struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

// buildWebPage returns the page and the capabilities it must be published
// with. A primitive the page could not run honestly is refused here, with the
// reason, rather than failing when somebody runs it.
func buildWebPage(store string, packages []string) ([]byte, map[string]any, error) {
	prims := map[string]webPrimitive{}
	tools := map[string]map[string]bool{} // connector to its tools
	for _, dir := range packages {
		m, err := mf.Load(dir)
		if err != nil {
			return nil, nil, err
		}
		if problems := m.RunProblems(); len(problems) > 0 {
			return nil, nil, fmt.Errorf("%s: primitive.yaml cannot be run:\n  - %s", dir, strings.Join(problems, "\n  - "))
		}
		name := m.Metadata.Name
		refuse := func(f string, a ...any) error {
			return fmt.Errorf("%s cannot run in a web page: %s", name, fmt.Sprintf(f, a...))
		}
		if _, dup := prims[name]; dup {
			return nil, nil, fmt.Errorf("two packages are named %s", name)
		}
		ext := filepath.Ext(m.Execution.Entrypoint)
		if ext != ".js" && ext != ".ts" {
			return nil, nil, refuse("its entrypoint is %s, and the page runs JavaScript and TypeScript only", m.Execution.Entrypoint)
		}
		switch {
		case len(m.Commands) > 0:
			return nil, nil, refuse("it declares host programs, and a web page has no machine")
		case len(m.Files) > 0:
			return nil, nil, refuse("it declares files, and a web page has no machine")
		case len(m.Fetch) > 0:
			return nil, nil, refuse("it declares web requests, which an Artifact is not allowed to make")
		}
		raw, err := os.ReadFile(filepath.Join(dir, m.Execution.Entrypoint))
		if err != nil {
			return nil, nil, err
		}
		program := string(raw)
		if ext == ".ts" {
			if program, err = stripTypes(program, m.Execution.Entrypoint); err != nil {
				return nil, nil, err
			}
		}
		p := webPrimitive{Version: m.Metadata.Version, Description: m.Metadata.Description, Bindings: map[string]webBinding{}, Program: program}
		for _, t := range m.Tools {
			// The page has no approval step yet, so it makes no changes.
			if t.Effect != "read" {
				return nil, nil, refuse("tool %q is declared %s, and the page cannot ask for approval yet", t.Alias, t.Effect)
			}
			// An Artifact declares its connector tools when it is published,
			// before any viewer's tools can be seen, so a tool is named, not
			// found.
			if t.Pin == nil || t.Pin.Server == "" || t.Pin.Tool == "" {
				if t.Optional {
					continue
				}
				return nil, nil, refuse("tool %q must name its connector, as pin: {server: Gmail, tool: search_threads}", t.Alias)
			}
			// Claude Code calls a claude.ai connector "claude.ai Gmail"; in
			// claude.ai itself it is "Gmail".
			server := strings.TrimPrefix(t.Pin.Server, "claude.ai ")
			p.Bindings[t.Alias] = webBinding{Server: server, Tool: t.Pin.Tool}
			if tools[server] == nil {
				tools[server] = map[string]bool{}
			}
			tools[server][t.Pin.Tool] = true
		}
		prims[name] = p
	}

	wasm, in, sum, err := obtain(store, "main.js")
	if err != nil {
		return nil, nil, err
	}
	if in.SHA256 == "" || sum != in.SHA256 {
		return nil, nil, fmt.Errorf("the JavaScript interpreter is not the pinned one")
	}
	primJSON, _ := json.MarshalIndent(prims, "", "  ")
	// A closing script tag inside the page's own script would end it early.
	safe := func(s string) string { return strings.ReplaceAll(s, "</", `<\/`) }
	page := strings.NewReplacer(
		"@QJS_B64@", base64.StdEncoding.EncodeToString(wasm),
		"@QJS_SHA256@", sum,
		"@PRELUDE@", safe(quote(jsPrelude)),
		"@TAPWEB@", safe(strings.Replace(webRunner, "export async function runPrimitive", "async function runPrimitive", 1)),
		"@PRIMITIVES@", safe(string(primJSON)),
		"@VERSION@", version,
	).Replace(webWorkerPage)

	var servers []any
	var names []string
	for s := range tools {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		var ts []string
		for t := range tools[s] {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		servers = append(servers, map[string]any{"server": s, "tools": ts})
	}
	caps := map[string]any{"db": map[string]any{}, "user": map[string]any{}}
	if len(servers) > 0 {
		caps["mcp"] = map[string]any{"servers": servers}
	}
	return []byte(page), caps, nil
}
