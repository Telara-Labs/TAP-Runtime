package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

// catalogEntry is a primitive available to this local TAP installation.
type catalogEntry struct {
	Ref, Digest, Description, Path, Source string
	Manifest                               *mf.Manifest
}

// localCatalog scans only known primitive roots. Each root's immediate child
// directories are package candidates; the user's home is never recursively
// traversed.
func localCatalog(extraRoots ...string) ([]catalogEntry, error) {
	var roots []struct{ path, source string }
	if cfg, err := userConfigDir(); err == nil && cfg != "" {
		roots = append(roots, struct{ path, source string }{filepath.Join(cfg, "tap", "primitives"), "tap"})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots,
			struct{ path, source string }{filepath.Join(home, ".claude", "skills"), "claude"},
			struct{ path, source string }{filepath.Join(home, ".codex", "skills"), "codex"},
		)
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		roots = append(roots,
			struct{ path, source string }{filepath.Join(cwd, ".claude", "skills"), "claude"},
			struct{ path, source string }{filepath.Join(cwd, ".codex", "skills"), "codex"},
		)
	}
	for _, path := range extraRoots {
		if path != "" {
			roots = append(roots, struct{ path, source string }{path, "catalog-root"})
		}
	}
	entries := make([]catalogEntry, 0)
	for _, root := range roots {
		items, err := os.ReadDir(root.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read local primitive root %s: %w", root.path, err)
		}
		for _, item := range items {
			if !item.IsDir() || strings.HasPrefix(item.Name(), ".") {
				continue
			}
			p := filepath.Join(root.path, item.Name())
			// Skills roots are opt-in: only folders with TAP's save marker count.
			if root.source == "claude" || root.source == "codex" {
				if _, err := os.Lstat(filepath.Join(p, ".tap-primitive.json")); os.IsNotExist(err) {
					continue
				}
			}
			entry, err := readCatalogEntry(p, root.source)
			if err != nil {
				continue
			} // malformed, stale, symlinked, or escaping entries are unavailable
			if (root.source == "claude" || root.source == "codex") && !validSavedMarker(p, entry.Ref) {
				continue
			}
			entries = append(entries, entry)
		}
		if root.source == "tap" || root.source == "catalog-root" {
			entries = append(entries, retainedCatalog(root.path, root.source)...)
		}
	}
	return dedupeCatalog(entries), nil
}

// Saved version history has exactly two levels under the known collection.
// Temporary stages and unrelated hidden directories are never catalog roots.
func retainedCatalog(root, source string) []catalogEntry {
	history := filepath.Join(root, ".versions")
	st, err := os.Lstat(history)
	if err != nil || !st.IsDir() {
		return nil
	}
	identities, err := os.ReadDir(history)
	if err != nil {
		return nil
	}
	var out []catalogEntry
	for _, identity := range identities {
		if !identity.IsDir() || strings.HasPrefix(identity.Name(), ".") {
			continue
		}
		group := filepath.Join(history, identity.Name())
		versions, err := os.ReadDir(group)
		if err != nil {
			continue
		}
		for _, version := range versions {
			if !version.IsDir() || strings.HasPrefix(version.Name(), ".") {
				continue
			}
			dir := filepath.Join(group, version.Name())
			entry, err := readCatalogEntry(dir, source)
			if err != nil || !validSavedMarker(dir, entry.Ref) {
				continue
			}
			md := entry.Manifest.Metadata
			if group != pack.VersionHistoryDir(root, md.Publisher, md.Name) || version.Name() != md.Version {
				continue
			}
			out = append(out, entry)
		}
	}
	return out
}

func readCatalogEntry(dir, source string) (catalogEntry, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return catalogEntry{}, err
	}
	rootInfo, err := os.Lstat(abs)
	if err != nil || !rootInfo.IsDir() {
		return catalogEntry{}, fmt.Errorf("not a package directory or is a symlink")
	}
	// Resolve aliases in ancestors (notably macOS /var -> /private/var), then
	// compare package members against this canonical root.
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return catalogEntry{}, err
	}
	manifestPath := filepath.Join(abs, "primitive.yaml")
	if st, err := os.Lstat(manifestPath); err != nil || st.Mode()&os.ModeSymlink != 0 {
		return catalogEntry{}, fmt.Errorf("manifest is missing or a symlink")
	}
	manifestResolved, err := filepath.EvalSymlinks(manifestPath)
	if err != nil || !withinPath(abs, manifestResolved) {
		return catalogEntry{}, fmt.Errorf("manifest is a symlink or escapes package")
	}
	digest, m, err := packageDigest(abs)
	if err != nil {
		return catalogEntry{}, err
	}
	if len(m.RunProblems()) != 0 {
		return catalogEntry{}, fmt.Errorf("invalid manifest: %s", strings.Join(m.RunProblems(), "; "))
	}
	if m.Runtime() != mf.RuntimeWasm {
		return catalogEntry{}, fmt.Errorf("this runner cannot execute %s", m.Runtime())
	}
	entryPath := filepath.Join(abs, filepath.Clean(filepath.FromSlash(m.Execution.Entrypoint)))
	if st, err := os.Lstat(entryPath); err != nil || st.Mode()&os.ModeSymlink != 0 {
		return catalogEntry{}, fmt.Errorf("entrypoint is missing or a symlink")
	}
	entryResolved, err := filepath.EvalSymlinks(entryPath)
	if err != nil || entryResolved != entryPath || !withinPath(abs, entryResolved) {
		return catalogEntry{}, fmt.Errorf("entrypoint is a symlink or escapes package")
	}
	if st, err := os.Stat(entryResolved); err != nil || !st.Mode().IsRegular() {
		return catalogEntry{}, fmt.Errorf("invalid entrypoint")
	}
	if m.Metadata.Publisher == "" || m.Metadata.Version == "" ||
		strings.ContainsAny(m.Metadata.Publisher, "/@ \t\n\r") ||
		strings.ContainsAny(m.Metadata.Version, "/@ \t\n\r") {
		return catalogEntry{}, fmt.Errorf("primitive needs an exact publisher/name@version identity")
	}
	ref := m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version
	return catalogEntry{Ref: ref, Digest: digest, Description: m.Metadata.Description, Path: abs, Source: source, Manifest: m}, nil
}

func withinPath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func dedupeCatalog(in []catalogEntry) []catalogEntry {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Ref != in[j].Ref {
			return in[i].Ref < in[j].Ref
		}
		if in[i].Digest != in[j].Digest {
			return in[i].Digest < in[j].Digest
		}
		return in[i].Path < in[j].Path
	})
	out := make([]catalogEntry, 0, len(in))
	pairs := map[string]bool{}
	for _, e := range in {
		key := e.Ref + "\x00" + e.Digest
		if pairs[key] {
			continue
		}
		pairs[key] = true
		out = append(out, e)
	}
	return out
}

// searchCatalog finds the primitives a few words describe. An agent searches
// in its own words ("release readiness check commit after tag"), so an entry
// matches when its reference or description holds the query as written (and
// then only such entries are returned), or else shares at least half of the
// query's words (wordSet: values such as versions
// left out, words cut to five letters). Entries sharing more words rank first.
func searchCatalog(entries []catalogEntry, query string, limit int) []catalogEntry {
	query = strings.ToLower(strings.TrimSpace(query))
	if limit <= 0 {
		return []catalogEntry{}
	}
	q := wordSet(query)
	type scored struct {
		e     catalogEntry
		score int
	}
	var matches []scored
	for _, e := range entries {
		text := strings.ToLower(e.Ref + " " + e.Description)
		score := 0
		if query == "" || strings.Contains(text, query) {
			score = len(q) + 1
		} else {
			words := wordSet(text)
			for w := range q {
				if words[w] {
					score++
				}
			}
			if len(q) == 0 || 2*score < len(q) {
				continue
			}
		}
		matches = append(matches, scored{e, score})
	}
	// A query found as written is a precise lookup ("probe-24"): only those
	// entries are returned, not every entry sharing its words.
	if query != "" {
		var exact []scored
		for _, m := range matches {
			if m.score == len(q)+1 {
				exact = append(exact, m)
			}
		}
		if len(exact) > 0 {
			matches = exact
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].e.Ref < matches[j].e.Ref
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]catalogEntry, len(matches))
	for i, m := range matches {
		out[i] = m.e
	}
	return out
}

func resolveCatalog(entries []catalogEntry, ref, digest string) (catalogEntry, error) {
	var found []catalogEntry
	for _, e := range entries {
		if ref != "" && e.Ref != ref {
			continue
		}
		if digest != "" && !strings.EqualFold(e.Digest, digest) {
			continue
		}
		found = append(found, e)
	}
	switch len(found) {
	case 0:
		for _, e := range entries {
			if ref != "" && e.Ref == ref && digest != "" {
				return catalogEntry{}, fmt.Errorf("local primitive digest is no longer installed; inspect %s with tap_load before choosing its current digest; refresh TAP-owned pointers with tap discover migrate-saved", ref)
			}
		}
		return catalogEntry{}, fmt.Errorf("local primitive not found")
	case 1:
		return found[0], nil
	default:
		return catalogEntry{}, fmt.Errorf("local primitive reference is ambiguous; include its digest")
	}
}

// savedMarker is decoded only to ensure a saved skill's metadata is valid.
type savedMarker struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

func validSavedMarker(path, ref string) bool {
	markerPath := filepath.Join(path, ".tap-primitive.json")
	info, err := os.Lstat(markerPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(markerPath)
	if err != nil {
		return false
	}
	var marker savedMarker
	name, _, ok := strings.Cut(ref, "@")
	return ok && json.Unmarshal(b, &marker) == nil && marker.Name == name && marker.Digest != ""
}
