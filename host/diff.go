package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

type diffIdentity struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

type packageChange struct {
	Path          string `json:"path"`
	Before        any    `json:"before"`
	After         any    `json:"after"`
	BeforePresent bool   `json:"before_present"`
	AfterPresent  bool   `json:"after_present"`
}

type packageDiff struct {
	Before            diffIdentity    `json:"before"`
	After             diffIdentity    `json:"after"`
	Status            string          `json:"status"`
	Scope             string          `json:"scope"`
	SameVersionChange bool            `json:"same_version_change"`
	Changes           []packageChange `json:"changes"`
	AuthorityWidening []string        `json:"authority_widening"`
	Review            []string        `json:"review"`
}

const diffCaveat = "This compares the manifest and entrypoint only. It does not prove behavioral compatibility, inspect dependencies, or approve an upgrade."

// diffCommand never executes either package, downloads an interpreter, or
// changes trust. A changed digest requires review, even for a patch version.
func diffCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the comparison as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: tap diff [--json] <old-package-dir> <new-package-dir>")
		return 2
	}
	r, err := comparePackages(fs.Arg(0), fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, "tap diff:", err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	} else {
		fmt.Fprintf(stdout, "%s -> %s\nold digest: %s\nnew digest: %s\n%s\n", r.Before.Ref, r.After.Ref, r.Before.Digest, r.After.Digest, r.Status)
		for _, c := range r.Changes {
			before, _ := json.Marshal(c.Before)
			after, _ := json.Marshal(c.After)
			if !c.BeforePresent {
				before = []byte("<absent>")
			}
			if !c.AfterPresent {
				after = []byte("<absent>")
			}
			fmt.Fprintf(stdout, "  %s: %s -> %s\n", c.Path, before, after)
		}
		for _, reason := range r.AuthorityWidening {
			fmt.Fprintln(stdout, "  authority:", reason)
		}
		for _, reason := range r.Review {
			fmt.Fprintln(stdout, "  review:", reason)
		}
		fmt.Fprintln(stdout, r.Scope)
	}
	if r.Status != "identical" {
		return 1
	}
	return 0
}

type diffPackage struct {
	identity diffIdentity
	manifest *mf.Manifest
	code     []byte
}

// Read validated paths before reading the entrypoint; do not follow package
// member symlinks. Hash exactly the bytes used for this comparison.
func readDiffPackage(dir string) (diffPackage, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return diffPackage{}, err
	}
	st, err := os.Lstat(abs)
	if err != nil || !st.IsDir() {
		return diffPackage{}, fmt.Errorf("%s is not a package directory", dir)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return diffPackage{}, err
	}
	read := func(name string) ([]byte, error) {
		path := filepath.Join(abs, filepath.FromSlash(name))
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path || !withinPath(abs, resolved) {
			return nil, fmt.Errorf("%s is missing, symlinked, or outside the package", name)
		}
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", name)
		}
		return os.ReadFile(path)
	}
	raw, err := read("primitive.yaml")
	if err != nil {
		return diffPackage{}, err
	}
	m, err := mf.Parse(raw)
	if err != nil {
		return diffPackage{}, err
	}
	if p := m.RunProblems(); len(p) > 0 {
		return diffPackage{}, fmt.Errorf("invalid manifest: %s", strings.Join(p, "; "))
	}
	if m.Metadata.Publisher == "" || m.Metadata.Version == "" || strings.ContainsAny(m.Metadata.Publisher+m.Metadata.Version, "/@ \t\n\r") {
		return diffPackage{}, fmt.Errorf("manifest needs an exact publisher/name@version identity")
	}
	code, err := read(m.Execution.Entrypoint)
	if err != nil {
		return diffPackage{}, err
	}
	sum := sha256.Sum256(append(append([]byte{}, raw...), code...))
	return diffPackage{diffIdentity{m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version, hex.EncodeToString(sum[:])}, m, code}, nil
}

func comparePackages(oldDir, newDir string) (*packageDiff, error) {
	old, err := readDiffPackage(oldDir)
	if err != nil {
		return nil, fmt.Errorf("old package: %w", err)
	}
	next, err := readDiffPackage(newDir)
	if err != nil {
		return nil, fmt.Errorf("new package: %w", err)
	}
	if old.manifest.Metadata.Publisher != next.manifest.Metadata.Publisher || old.manifest.Metadata.Name != next.manifest.Metadata.Name {
		return nil, fmt.Errorf("compare versions of the same publisher/name, not %s and %s", old.identity.Ref, next.identity.Ref)
	}
	r := &packageDiff{Before: old.identity, After: next.identity, Status: "identical", Scope: diffCaveat, Changes: []packageChange{}, AuthorityWidening: []string{}, Review: []string{}}
	if old.identity.Digest == next.identity.Digest {
		return r, nil
	}
	r.Status = "review_required"
	r.SameVersionChange = old.identity.Ref == next.identity.Ref
	if r.SameVersionChange {
		r.Review = append(r.Review, "The same version names different bytes. Publish changed content under a new version; keep the reviewed digest pinned.")
	}
	// JSON maps normalize YAML map order, while arrays retain their order.
	toObject := func(m *mf.Manifest) (any, error) {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		var value any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		err = dec.Decode(&value)
		return value, err
	}
	a, err := toObject(old.manifest)
	if err != nil {
		return nil, fmt.Errorf("old manifest cannot be compared: %w", err)
	}
	b, err := toObject(next.manifest)
	if err != nil {
		return nil, fmt.Errorf("new manifest cannot be compared: %w", err)
	}
	manifestChanges("", a, b, &r.Changes)
	if !reflect.DeepEqual(old.code, next.code) {
		codeHash := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
		r.Changes = append(r.Changes, packageChange{"entrypoint.sha256", codeHash(old.code), codeHash(next.code), true, true})
		r.Review = append(r.Review, "Entrypoint code changed. Review the source diff and rerun the caller's acceptance tests.")
	}
	if !reflect.DeepEqual(old.manifest.Interface, next.manifest.Interface) {
		r.Review = append(r.Review, "Input or output schema changed; existing callers may break. Review the contract diff and migration before upgrading.")
	}
	for _, field := range []struct {
		name      string
		old, next any
	}{
		{"tools", old.manifest.Tools, next.manifest.Tools},
		{"capabilities", old.manifest.Capabilities, next.manifest.Capabilities},
		{"execution", old.manifest.Execution, next.manifest.Execution},
		{"requires", old.manifest.Requires, next.manifest.Requires},
		{"commands", old.manifest.Commands, next.manifest.Commands},
		{"files", old.manifest.Files, next.manifest.Files},
		{"fetch", old.manifest.Fetch, next.manifest.Fetch},
	} {
		if !reflect.DeepEqual(field.old, field.next) {
			r.Review = append(r.Review, field.name+" changed; review binding, runtime, and caller assumptions.")
		}
	}
	r.AuthorityWidening = append(r.AuthorityWidening, mf.Widening(old.manifest, next.manifest)...)
	if len(r.Changes) == 0 {
		r.Review = append(r.Review, "Manifest bytes changed without a parsed field change (for example formatting). The execution digest and trust identity still changed.")
	}
	r.Review = append(r.Review, "The changed digest requires fresh review; a version label is not a compatibility guarantee.")
	return r, nil
}

// Report exact before/after values, not just a count or a changelog assertion.
func manifestChanges(path string, old, next any, changes *[]packageChange) {
	if reflect.DeepEqual(old, next) {
		return
	}
	a, aok := old.(map[string]any)
	b, bok := next.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range a {
			keys[k] = true
		}
		for k := range b {
			keys[k] = true
		}
		ordered := make([]string, 0, len(keys))
		for k := range keys {
			ordered = append(ordered, k)
		}
		sort.Strings(ordered)
		for _, k := range ordered {
			p := k
			if path != "" {
				p = path + "." + k
			}
			before, had := a[k]
			after, has := b[k]
			if had != has {
				*changes = append(*changes, packageChange{p, before, after, had, has})
			} else {
				manifestChanges(p, before, after, changes)
			}
		}
		return
	}
	*changes = append(*changes, packageChange{path, old, next, true, true})
}
