package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// tap_run takes a package path from the model. A model
// that has been steered can write a primitive anywhere and ask for it to be
// run. So the first time this machine is asked to run a particular package,
// the person sees what it declares and says yes or no, or trusts it through
// the owner CLI before using a client that cannot ask. The answer is kept by
// the package's digest: edit the manifest or the program and it is asked about again.

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
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	At           string   `json:"at"`
	FetchOrigins []string `json:"fetch_origins,omitempty"`
	// FetchGrants are read fetches the person approved in a prompt for this
	// digest, kept so a later run of the same version does not ask again
	// ("send GET requests to https://gitlab.com"). A new version asks anew.
	FetchGrants []string `json:"fetch_grants,omitempty"`
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

func (t *trustStore) add(digest, name, path string, origins ...string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path == "" {
		return fmt.Errorf("this machine has no user config directory to keep the answer in")
	}
	all := t.load()
	all[digest] = trusted{Name: name, Path: path, At: time.Now().UTC().Format(time.RFC3339), FetchOrigins: origins}
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
// nobody to ask (ask is nil), local-reach packages require prior owner trust.
func admitPackage(store *trustStore, ask Truster, dir string) (refusal string) {
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
	if ask == nil || !ask(m.Metadata.Name, m.Metadata.Publisher, m.Metadata.Version, abs, digest[:12], declares(m)) {
		return fmt.Sprintf("the person did not agree to run %s from %s; in a client that cannot show this question, review the package and run: tap trust %q", m.Metadata.Name, abs, abs)
	}
	if err := store.add(digest, m.Metadata.Name, abs); err != nil {
		logf("trust      could not keep the answer for %s: %v", m.Metadata.Name, err)
	}
	return ""
}

func newTrustStore() *trustStore { return &trustStore{path: defaultTrustPath()} }

// trustCommand is `tap trust`: agree, ahead of time, to run a package, so a
// client that cannot show the first-run question (a headless `claude -p`, a
// script) can still run it. Found testing: with no window to answer it, the
// question was declined and the primitive could not run at all.
func trustCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tap trust", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var origins stringList
	fs.Var(&origins, "fetch-origin", "also permit requests to this exact declared origin for this package digest; repeat for each origin")
	list := fs.Bool("list", false, "list trusted packages and fetch grants")
	forget := fs.String("forget", "", "forget a trusted digest prefix, including its fetch grants")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	args = fs.Args()
	if *list || *forget != "" {
		if len(args) != 0 || len(origins) != 0 || (*list && *forget != "") {
			fmt.Fprintln(stderr, "tap trust: --list and --forget cannot be combined with a package or fetch grants")
			return 2
		}
		if *list {
			args = []string{"--list"}
		} else {
			args = []string{"--forget", *forget}
		}
	}
	store := newTrustStore()
	switch {
	case len(args) == 1 && args[0] == "--list":
		all := store.load()
		if len(all) == 0 {
			fmt.Fprintln(stdout, "no packages are trusted")
		}
		var digests []string
		for d := range all {
			digests = append(digests, d)
		}
		sort.Strings(digests)
		for _, d := range digests {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", d[:12], all[d].Name, all[d].Path)
			for _, origin := range all[d].FetchOrigins {
				fmt.Fprintln(stdout, "  fetch:", origin)
			}
			for _, kind := range all[d].FetchGrants {
				fmt.Fprintln(stdout, "  approved:", kind)
			}
		}
		return 0
	case len(args) == 2 && args[0] == "--forget":
		store.mu.Lock()
		all := store.load()
		removed := 0
		for d := range all {
			if strings.HasPrefix(d, args[1]) {
				delete(all, d)
				removed++
			}
		}
		if removed > 0 {
			b, _ := json.MarshalIndent(all, "", "  ")
			if err := os.WriteFile(store.path, append(b, '\n'), 0o600); err != nil {
				store.mu.Unlock()
				fmt.Fprintln(stderr, "tap trust:", err)
				return 1
			}
		}
		store.mu.Unlock()
		fmt.Fprintf(stdout, "forgot %d package(s)\n", removed)
		return 0
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		digest, m, err := packageDigest(args[0])
		if err != nil {
			fmt.Fprintln(stderr, "tap trust:", err)
			return 1
		}
		abs, _ := filepath.Abs(args[0])
		for _, origin := range origins {
			if err := validateFetchGrant(m, origin); err != nil {
				fmt.Fprintln(stderr, "tap trust:", err)
				return 1
			}
		}
		fmt.Fprintf(stdout, "%s v%s by %s (digest %s)\n%s\n", m.Metadata.Name, m.Metadata.Version, m.Metadata.Publisher, digest[:12], declares(m))
		if err := store.add(digest, m.Metadata.Name, abs, origins...); err != nil {
			fmt.Fprintln(stderr, "tap trust:", err)
			return 1
		}
		fmt.Fprintln(stdout, "trusted: this package, as it is now, may run through a client that cannot ask. Edit it and it is asked about again.")
		for _, origin := range origins {
			fmt.Fprintf(stdout, "fetch approved for this digest: %s (declared methods only; URLs, headers and bodies can send data there)\n", origin)
		}
		return 0
	}
	fmt.Fprintln(stderr, "usage: tap trust [--fetch-origin ORIGIN] PACKAGE-DIR\n       tap trust --list\n       tap trust --forget DIGEST-PREFIX")
	return 2
}

func validateFetchGrant(m *mf.Manifest, origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(u.Host, "* \t\r\n") {
		return fmt.Errorf("fetch grant must be an exact http(s) origin without credentials, a path or wildcard: %q", origin)
	}
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		if fetchAllowed(m.Fetch, method, u) {
			if p := declarationProblems(m); len(p) > 0 {
				return fmt.Errorf("invalid declaration: %s", strings.Join(p, "; "))
			}
			return nil
		}
	}
	return fmt.Errorf("fetch origin %q is not declared by this package", origin)
}

// addFetchGrant keeps a read fetch the person approved for digest.
func (t *trustStore) addFetchGrant(digest, kind string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path == "" {
		return fmt.Errorf("this machine has no user config directory to keep the answer in")
	}
	all := t.load()
	e := all[digest]
	for _, k := range e.FetchGrants {
		if k == kind {
			return nil
		}
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	e.FetchGrants = append(e.FetchGrants, kind)
	all[digest] = e
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(t.path, append(b, '\n'), 0o600)
}

func (t *trustStore) fetchGrants(digest string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.load()[digest].FetchGrants...)
}

func (t *trustStore) fetchOrigins(digest string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.load()[digest].FetchOrigins...)
}

// takeConfigDir removes a leading --config-dir DIR from args and points the
// runner's configuration at DIR, as `tap serve --config-dir` does, so `tap
// trust` and `tap bind` can write to the configuration a server reads.
func takeConfigDir(args []string) []string {
	if len(args) >= 2 && args[0] == "--config-dir" {
		dir := args[1]
		userConfigDir = func() (string, error) { return dir, nil }
		return args[2:]
	}
	return args
}
