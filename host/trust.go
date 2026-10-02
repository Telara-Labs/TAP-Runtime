package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mf "gitlab.com/telara-labs/tap-runtime/contract/manifest"
)

// tap_run takes a package path from the model (TENG-3103, threat G6). A model
// that has been steered can write a primitive anywhere and ask for it to be
// run. So the first time this machine is asked to run a particular package,
// through a client that can ask, the person sees what it declares and says
// yes or no. The answer is kept by the package's digest: edit the manifest or
// the program and it is asked about again.

// userConfigDir is where the runner keeps what a person has chosen. A test
// replaces it, so no test writes into a real user's configuration.
var userConfigDir = os.UserConfigDir

// packageDigest is the digest of a package as a run records it: its manifest
// and its entrypoint.
func packageDigest(dir string) (digest string, m *mf.Manifest, err error) {
	return mf.RunDigest(dir)
}

// needsPackageTrust is true when the program can act outside the mediated MCP
// tool broker. A tool-only primitive has no local file, command, or fetch
// authority; each dispatched call is checked against its declared and
// server-annotated effect in callTool, where effectful calls need approval.
// Asking for a second, package-wide approval before that per-call gate is
// redundant and can fail on MCP clients that do not surface trust elicitations.
func needsPackageTrust(m *mf.Manifest) bool {
	return len(m.Tools) == 0 || len(m.Files) > 0 || len(m.Commands) > 0 || len(m.Fetch) > 0
}

// declares says in a few lines what a package may touch.
func declares(m *mf.Manifest) string {
	var lines []string
	for _, f := range m.Files {
		lines = append(lines, fmt.Sprintf("files: %s (%s)", f.Path, f.Access))
	}
	for _, f := range m.Fetch {
		methods := "GET"
		if len(f.Methods) > 0 {
			methods = strings.Join(f.Methods, ", ")
		}
		lines = append(lines, fmt.Sprintf("web: %s (%s)", f.Origin, methods))
	}
	for _, c := range m.Commands {
		lines = append(lines, fmt.Sprintf("program: %s %s (%s)", c.Command, strings.Join(c.Args, " "), c.Effect))
	}
	for _, t := range m.Tools {
		lines = append(lines, fmt.Sprintf("tool: %s (%s, %s)", t.Capability, t.Alias, t.Effect))
	}
	if len(lines) == 0 {
		return "it declares nothing: no files, web, programs or tools"
	}
	return strings.Join(lines, "\n")
}

// trustStore keeps the digests of the packages a person has agreed to run.
type trustStore struct {
	path string
	mu   sync.Mutex
}

func defaultTrustPath() string {
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "tap", "trusted.json")
}

type trusted struct {
	Name string `json:"name"`
	Path string `json:"path"`
	At   string `json:"at"`
}

func (t *trustStore) load() map[string]trusted {
	all := map[string]trusted{}
	if t.path == "" {
		return all
	}
	if b, err := os.ReadFile(t.path); err == nil {
		json.Unmarshal(b, &all)
	}
	return all
}

func (t *trustStore) has(digest string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.load()[digest]
	return ok
}

func (t *trustStore) add(digest, name, path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path == "" {
		return fmt.Errorf("this machine has no user config directory to keep the answer in")
	}
	all := t.load()
	all[digest] = trusted{Name: name, Path: path, At: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(t.path, append(b, '\n'), 0o600)
}

// Truster asks the person whether a package may run. ok is false for a no,
// and for no answer.
type Truster func(name, publisher, version, path, digest, declared string) bool

// admitPackage decides whether a package named by a client may be run. A
// package whose digest is kept runs. Otherwise the person is asked; with
// nobody to ask (ask is nil) it runs as before, and a change in it is refused
// anyway because no one can approve one.
func admitPackage(store *trustStore, ask Truster, dir string) (refusal string) {
	if ask == nil {
		return ""
	}
	digest, m, err := packageDigest(dir)
	if err != nil {
		return "" // Run reports a package that cannot be read
	}
	if !needsPackageTrust(m) {
		return ""
	}
	if store.has(digest) {
		return ""
	}
	abs, _ := filepath.Abs(dir)
	if !ask(m.Metadata.Name, m.Metadata.Publisher, m.Metadata.Version, abs, digest[:12], declares(m)) {
		return fmt.Sprintf("the person did not agree to run %s from %s", m.Metadata.Name, abs)
	}
	if err := store.add(digest, m.Metadata.Name, abs); err != nil {
		logf("trust      could not keep the answer for %s: %v", m.Metadata.Name, err)
	}
	return ""
}

func newTrustStore() *trustStore { return &trustStore{path: defaultTrustPath()} }
