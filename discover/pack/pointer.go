package pack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gitlab.com/telara-labs/tap-runtime/discover/client"
)

// A primitive is saved once, to the TAP collection, and each chosen agent
// gets a pointer: a skills folder holding only a SKILL.md that names the
// primitive and says to run it with tap_run (D2, TENG-3109). The TAP MCP
// server finds the primitive in the collection from every agent; the
// pointer lets an agent notice it without searching first. A pointer holds
// no package and no SavedMarker, so the catalog never lists it twice.

// CollectionDir is the TAP collection, the first root the runner's catalog
// scans: <user config dir>/tap/primitives.
func CollectionDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "tap", "primitives"), nil
}

// PointerKey is the SKILL.md front-matter field that marks a pointer as
// TAP's. A folder without it is never touched.
const PointerKey = "tap-pointer"

// Pointer write outcomes.
const (
	PointerWritten   = "written"
	PointerUnchanged = "unchanged"
	PointerSkipped   = "skipped"
)

// PointerResult is one report line: the pointer for one agent.
type PointerResult struct {
	Client, Path, Mode, Reason string
}

// Target is an agent to point at the primitive, and whether it was picked by
// name (an agent picked by name gets a pointer even where the primitive
// cannot run yet).
type Target struct {
	Client   client.Client
	Explicit bool
}

// Targets resolves --save-client: "detected" (the default, or empty) points
// every installed agent where the primitive can run (it has a skills folder
// and a bridge); "all" and named agents are explicit picks; "none" points
// nobody.
func Targets(list, home string) ([]Target, error) {
	list = strings.TrimSpace(list)
	if list == "none" {
		return nil, nil
	}
	var out []Target
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		explicit := name != "" && name != "detected"
		runnable := func(c client.Client) bool { return client.HasSkills(c) && (explicit || c.Bridge) }
		cs, err := client.Resolve(name, home, client.CapSkills, runnable)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			out = append(out, Target{Client: c, Explicit: explicit})
		}
	}
	seen := map[string]bool{}
	dedup := out[:0]
	for _, t := range out {
		if !seen[t.Client.ID] {
			seen[t.Client.ID] = true
			dedup = append(dedup, t)
		}
	}
	return dedup, nil
}

// Identity is what a pointer names: the primitive's exact reference and its
// run digest, as tap_search returns them and tap_run checks them.
type Identity struct {
	Ref, Digest, Name, Description string
}

// ReadIdentity reads a saved package's identity from its primitive.yaml.
func ReadIdentity(pkgDir string) (Identity, error) {
	b, err := os.ReadFile(filepath.Join(pkgDir, "primitive.yaml"))
	if err != nil {
		return Identity{}, err
	}
	var m struct {
		Metadata struct {
			Name, Publisher, Version, Description string
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return Identity{}, err
	}
	md := m.Metadata
	if md.Name == "" || md.Publisher == "" || md.Version == "" {
		return Identity{}, fmt.Errorf("%s: primitive.yaml lacks publisher, name or version", pkgDir)
	}
	digest, err := RunDigest(pkgDir)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Ref: md.Publisher + "/" + md.Name + "@" + md.Version, Digest: digest, Name: md.Name, Description: md.Description}, nil
}

// RunDigest is the digest the runner lists a package under and checks
// before running it: sha256 over primitive.yaml followed by the entrypoint
// program, hex. It is not the SavedMarker digest (the archive's). The rule
// is the runner's (host/trust.go packageDigest); the host test
// TestSavedOncePointedEverywhereListedOnceAndRunnable fails if they part.
func RunDigest(pkgDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(pkgDir, "primitive.yaml"))
	if err != nil {
		return "", err
	}
	var m struct {
		Execution struct {
			Entrypoint string `yaml:"entrypoint"`
		} `yaml:"execution"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	if m.Execution.Entrypoint == "" {
		return "", fmt.Errorf("%s: primitive.yaml names no entrypoint", pkgDir)
	}
	script, err := os.ReadFile(filepath.Join(pkgDir, filepath.Clean(filepath.FromSlash(m.Execution.Entrypoint))))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(append([]byte{}, raw...), script...))
	return hex.EncodeToString(sum[:]), nil
}

// PointerSkillMD is the pointer's whole content. runsIn lists the agents
// sharing this folder where the primitive can run; empty means none can yet.
func PointerSkillMD(id Identity, runsIn []string) string {
	desc := id.Description
	if desc == "" {
		desc = id.Name
	}
	var where string
	if len(runsIn) == 0 {
		where = "This agent cannot run TAP primitives yet: the runner cannot make tool calls through it. Run it from an agent that can, such as Claude Code or Codex.\n"
	} else {
		where = "Runs here through the TAP runner (" + strings.Join(runsIn, ", ") + "). If no `tap_run` tool is available, connect the runner: `tap install --client <agent>`.\n"
	}
	descJSON, _ := json.Marshal(desc)
	return fmt.Sprintf(`---
name: %s
description: %s
%s: %s
---

# %s

A TAP primitive saved by `+"`tap discover`"+`. This folder only points at it; the
package lives in your TAP collection.

Run it with the TAP runner's `+"`tap_run`"+` tool:

    ref:    %s
    digest: %s

`+"`tap_load`"+` with the same ref shows its inputs and effects; the runner asks
you to approve any change it would make.

%s`, id.Name, descJSON, PointerKey, id.Ref, id.Name, id.Ref, id.Digest, where)
}

// IsPointer reports whether dir is a TAP pointer folder, and for which ref.
func IsPointer(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || !bytes.HasPrefix(b, []byte("---\n")) {
		return "", false
	}
	front, _, ok := bytes.Cut(b[4:], []byte("\n---"))
	if !ok {
		return "", false
	}
	for _, line := range strings.Split(string(front), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == PointerKey {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// WritePointers writes the pointer to the primitive saved at pkgDir into
// each target's skills folder (global, or the project's when project).
// Targets sharing one folder get one pointer and a report line each. A
// folder of the same name that is neither a pointer nor a saved primitive is
// left alone and reported as skipped; the others are still written.
func WritePointers(pkgDir string, targets []Target, project bool, home, projectDir string) ([]PointerResult, error) {
	id, err := ReadIdentity(pkgDir)
	if err != nil {
		return nil, err
	}
	type folder struct {
		path    string
		clients []Target
	}
	var folders []*folder
	byPath := map[string]*folder{}
	var out []PointerResult
	for _, t := range targets {
		dir, err := t.Client.SkillsDir(project, home, projectDir)
		if err != nil {
			out = append(out, PointerResult{Client: t.Client.ID, Mode: PointerSkipped, Reason: err.Error()})
			continue
		}
		p := filepath.Join(dir, id.Name)
		f := byPath[p]
		if f == nil {
			f = &folder{path: p}
			byPath[p] = f
			folders = append(folders, f)
		}
		f.clients = append(f.clients, t)
	}
	for _, f := range folders {
		var runsIn []string
		for _, t := range f.clients {
			if t.Client.Bridge {
				runsIn = append(runsIn, t.Client.Name)
			}
		}
		sort.Strings(runsIn)
		mode, reason, err := writePointer(f.path, PointerSkillMD(id, runsIn))
		if err != nil {
			return out, err
		}
		for i, t := range f.clients {
			r := PointerResult{Client: t.Client.ID, Path: f.path, Mode: mode, Reason: reason}
			if i > 0 && mode != PointerSkipped {
				r.Mode, r.Reason = PointerUnchanged, "shares "+f.clients[0].Client.ID+"'s folder"
			}
			if mode != PointerSkipped && !t.Client.Bridge {
				r.Reason = strings.TrimPrefix(r.Reason+"; the primitive cannot run in "+t.Client.Name+" yet", "; ")
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// writePointer puts content at dir/SKILL.md. It replaces only a pointer or a
// primitive saved there before collections existed (its package is now in
// the collection).
func writePointer(dir, content string) (mode, reason string, err error) {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return PointerSkipped, dir + " exists and is not a TAP folder; it was not replaced", nil
		}
		_, pointer := IsPointer(dir)
		_, merr := ReadMarker(dir)
		if !pointer && merr != nil {
			return PointerSkipped, dir + " exists and is not a TAP folder; it was not replaced", nil
		}
		if pointer {
			if b, err := os.ReadFile(filepath.Join(dir, "SKILL.md")); err == nil && string(b) == content {
				if entries, _ := os.ReadDir(dir); len(entries) == 1 {
					return PointerUnchanged, "", nil
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".pointer-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(stage)
	if err := os.WriteFile(filepath.Join(stage, "SKILL.md"), []byte(content), 0o644); err != nil {
		return "", "", err
	}
	if err := swapIn(stage, dir); err != nil {
		return "", "", err
	}
	return PointerWritten, "", nil
}

// swapIn replaces dest with stage whole, so a reader sees one or the other.
func swapIn(stage, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		old := stage + ".old"
		if err := os.Rename(dest, old); err != nil {
			return err
		}
		if err := os.Rename(stage, dest); err != nil {
			_ = os.Rename(old, dest)
			return err
		}
		return os.RemoveAll(old)
	}
	return os.Rename(stage, dest)
}

// MigrateResult is one primitive moved out of a skills folder.
type MigrateResult struct {
	From, To, Mode, Reason string
}

// skillsFolders lists every skills folder an agent reads, global and for the
// project in cwd, with the agents that read it. The project .codex/skills
// folder, where saves went before agents shared .agents/skills, is included
// so its primitives are migrated too.
func skillsFolders(home, cwd string) ([]string, map[string][]client.Client) {
	var dirs []string
	readers := map[string][]client.Client{}
	add := func(dir string, c client.Client) {
		if _, ok := readers[dir]; !ok {
			dirs = append(dirs, dir)
		}
		readers[dir] = append(readers[dir], c)
	}
	for _, c := range client.All() {
		for _, project := range []bool{false, true} {
			if project && cwd == "" {
				continue
			}
			if dir, err := c.SkillsDir(project, home, cwd); err == nil {
				add(dir, c)
			}
		}
	}
	if codex, ok := client.Lookup("codex"); ok && cwd != "" {
		add(filepath.Join(cwd, ".codex", "skills"), codex)
	}
	return dirs, readers
}

// MigrateSaved moves every primitive saved as a full package in an agent's
// skills folder (before collections) into the collection, and leaves a
// pointer in its place, so the agents reading that folder still see it. A
// collection entry of the same name holding another package wins: the old
// folder is left untouched and reported.
func MigrateSaved(home, cwd, collection string) ([]MigrateResult, error) {
	var out []MigrateResult
	dirs, readers := skillsFolders(home, cwd)
	for _, root := range dirs {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		var runsIn []string
		for _, c := range readers[root] {
			if c.Bridge {
				runsIn = append(runsIn, c.Name)
			}
		}
		sort.Strings(runsIn)
		for _, e := range entries {
			from := filepath.Join(root, e.Name())
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			mk, err := ReadMarker(from)
			if err != nil {
				continue // not a saved primitive: a pointer, or someone else's skill
			}
			id, err := ReadIdentity(from)
			if err != nil {
				out = append(out, MigrateResult{From: from, Mode: PointerSkipped, Reason: err.Error()})
				continue
			}
			to := filepath.Join(collection, e.Name())
			if have, err := ReadMarker(to); err == nil {
				if have != mk {
					out = append(out, MigrateResult{From: from, To: to, Mode: PointerSkipped, Reason: "the collection already holds a different " + e.Name()})
					continue
				}
			} else if _, err := os.Lstat(to); err == nil {
				out = append(out, MigrateResult{From: from, To: to, Mode: PointerSkipped, Reason: to + " exists and is not a saved primitive"})
				continue
			} else {
				if err := os.MkdirAll(collection, 0o755); err != nil {
					return out, err
				}
				if err := copyTree(from, to); err != nil {
					return out, err
				}
			}
			if _, _, err := writePointer(from, PointerSkillMD(id, runsIn)); err != nil {
				return out, err
			}
			out = append(out, MigrateResult{From: from, To: to, Mode: "moved"})
		}
	}
	return out, nil
}

// copyTree copies a package folder (regular files and folders only) into a
// staging folder beside dest, then renames it into place.
func copyTree(src, dest string) error {
	stage, err := os.MkdirTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".moving-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(stage, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, info.Mode().Perm())
		}
		return fmt.Errorf("%s is not a regular file", p)
	})
	if err != nil {
		return err
	}
	return os.Rename(stage, dest)
}

// Destination is where a save goes: the package into Collection, and a
// pointer into each target agent's skills folder (the project's when
// Project).
type Destination struct {
	Collection       string
	Targets          []Target
	Project          bool
	Home, ProjectDir string
}

// NewDestination resolves --save-client (see Targets) against the TAP
// collection of this user.
func NewDestination(saveClients string, project bool, home, projectDir string) (Destination, error) {
	coll, err := CollectionDir()
	if err != nil {
		return Destination{}, err
	}
	ts, err := Targets(saveClients, home)
	if err != nil {
		return Destination{}, err
	}
	return Destination{Collection: coll, Targets: ts, Project: project, Home: home, ProjectDir: projectDir}, nil
}

// Point writes the pointers to the package saved at pkgDir.
func (d Destination) Point(pkgDir string) ([]PointerResult, error) {
	if len(d.Targets) == 0 {
		return nil, nil
	}
	return WritePointers(pkgDir, d.Targets, d.Project, d.Home, d.ProjectDir)
}

// FormatPointers is the report of a save's pointers, one line each.
func FormatPointers(rs []PointerResult) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "  %s: %s", r.Client, r.Mode)
		if r.Path != "" {
			fmt.Fprintf(&b, " %s", r.Path)
		}
		if r.Reason != "" {
			fmt.Fprintf(&b, " (%s)", r.Reason)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
