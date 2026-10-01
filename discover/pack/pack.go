// Package pack writes a draft as a TAP package and installs it the way an agent client finds a skill.
package pack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Artifacts returns the draft's files, or ErrBlocked. Everything that writes
// or sends a draft goes through it.
func DraftArtifacts(d *model.Draft) (map[string][]byte, error) {
	if len(d.Blocked) > 0 {
		return nil, fmt.Errorf("%w: %s", model.ErrBlocked, strings.Join(d.Blocked, "; "))
	}
	return d.Files, nil
}

// SavedMarker is the file that marks a skills folder as a saved primitive.
const SavedMarker = ".tap-primitive.json"

// Bounds on unpacking, so a package cannot fill the disk.
const (
	MaxSavedFiles    = 2000
	MaxSavedUnpacked = 64 << 20
)

// SkillsDir is where a client looks for skills: "claude-code" or "codex",
// globally (under home) or for the project in cwd.
func SkillsDir(client string, project bool, home, cwd string) (string, error) {
	base := home
	if project {
		base = cwd
	}
	switch client {
	case "claude-code":
		return filepath.Join(base, ".claude", "skills"), nil
	case "codex":
		if project {
			return filepath.Join(base, ".codex", "skills"), nil
		}
		return filepath.Join(home, ".codex", "skills"), nil
	}
	return "", fmt.Errorf("unknown client %q (want claude-code or codex)", client)
}

type Marker struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	// Validation is "not_run": saving is not validating. A validation
	// result names the exact digest it passed for.
	Validation string `json:"validation"`
	// Origin, Receipts and Cases are set for a package a host agent
	// authored (save.go); a saved draft leaves them out.
	Origin   string `json:"origin,omitempty"`
	Receipts string `json:"receipts,omitempty"`
	Cases    int    `json:"cases,omitempty"`
}

// ErrNotSaved reports a folder of the draft's name that is not a saved
// primitive; it is left alone.
type ErrNotSaved struct{ Path string }

func (e *ErrNotSaved) Error() string {
	return e.Path + " exists and is not a saved primitive; it was not replaced"
}

var SkillName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Save installs the draft into root (a skills directory) and returns the
// folder. unchanged is true when the same draft was already there.
func SaveDraft(d *model.Draft, root string) (path string, unchanged bool, err error) {
	if !SkillName.MatchString(d.Name) {
		return "", false, fmt.Errorf("%q is not a usable folder name", d.Name)
	}
	pkg, digest, err := PackageDraft(d)
	if err != nil {
		return "", false, err
	}
	m := Marker{Name: d.Publisher + "/" + d.Name, Digest: digest, Validation: model.ValidationNotRun}
	return Install(root, d.Name, pkg, m, SavedSkillMD(d, filepath.Join(root, d.Name)))
}

// Install unpacks pkg into root/name with its SKILL.md and marker. A folder
// already holding the same marker is left as it is; one holding another
// saved primitive is replaced whole; any other folder is never touched.
func Install(root, name string, pkg []byte, m Marker, skillMD string) (path string, unchanged bool, err error) {
	dest := filepath.Join(root, name)
	overwrite := false
	if _, err := os.Stat(dest); err == nil {
		var have Marker
		b, rerr := os.ReadFile(filepath.Join(dest, SavedMarker))
		if rerr != nil || json.Unmarshal(b, &have) != nil {
			return "", false, &ErrNotSaved{Path: dest}
		}
		if have == m {
			return dest, true, nil
		}
		overwrite = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", false, err
	}
	stage, err := os.MkdirTemp(root, "."+name+".saving-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(stage)
	if err := Unpack(pkg, stage); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(stage, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		return "", false, err
	}
	marker, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, SavedMarker), append(marker, '\n'), 0o644); err != nil {
		return "", false, err
	}
	// Swap in whole: a reader sees the old folder or the new one, never half.
	if overwrite {
		old := stage + ".old"
		if err := os.Rename(dest, old); err != nil {
			return "", false, err
		}
		if err := os.Rename(stage, dest); err != nil {
			_ = os.Rename(old, dest)
			return "", false, err
		}
		_ = os.RemoveAll(old)
		return dest, false, nil
	}
	return dest, false, os.Rename(stage, dest)
}

func SavedSkillMD(d *model.Draft, dir string) string {
	desc := d.Name
	if b := d.Files["README.md"]; len(b) > 0 {
		if parts := strings.SplitN(string(b), "\n\n", 3); len(parts) > 1 {
			desc = strings.Join(strings.Fields(parts[1]), " ")
		}
	}
	descJSON, _ := json.Marshal(desc)
	return fmt.Sprintf(`---
name: %s
description: %s
---

# %s

A primitive drafted by `+"`tap discover`"+` from work that recurred in your sessions.
Read main.sh before running it.

Run it with the TAP runner's `+"`tap_run`"+` tool, giving this folder as the package:

    package: %s

Pass the arguments README.md lists, in order. The runner asks you to approve
any change it would make.

If no `+"`tap_run`"+` tool is available, connect the runner: `+"`tap install --client <client>`"+`.
`, d.Name, descJSON, d.Name, dir)
}

// Unpack writes a gzip tar into dest, refusing entries outside it, links and
// anything past the size bounds.
func Unpack(pkg []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(pkg))
	if err != nil {
		return fmt.Errorf("package is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files int
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read package: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("package entry %q is outside the package", h.Name)
		}
		target := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			total += h.Size
			if files > MaxSavedFiles || total > MaxSavedUnpacked {
				return fmt.Errorf("package unpacks past %d files or %d bytes", MaxSavedFiles, MaxSavedUnpacked)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("package entry %q is not a regular file or directory", h.Name)
		}
	}
	if files == 0 {
		return errors.New("package is empty")
	}
	return nil
}

// Package returns the draft as a gzip tar, the form a registry package and
// `telara tap pull` use, and its digest. Entries are sorted and carry a fixed
// time, so the same draft always has the same digest.
func PackageDraft(d *model.Draft) ([]byte, string, error) {
	files, err := DraftArtifacts(d)
	if err != nil {
		return nil, "", err
	}
	return PackFiles(files, func(n string) bool { return n == "main.sh" })
}

// PatternRoutines lists the pattern-level routines worth reviewing, one per task, most tokens
// saved over the recorded history first.
//
// One task shows up as many overlapping patterns (a five-step window slid
// along a longer automation, the same checks with and without a git status).
// Routines that occur in mostly the same sessions are treated as one task,
// whatever their steps: they are grouped when they share at least half of
// their sessions (Jaccard), and the group is represented by its longest,
// most specific routine. Routines with no step a primitive can replay are
// left out: there is nothing to draft.
func ReportPatternRoutines(r *model.Report) []int {
	var cands []int
	for i, c := range r.Candidates {
		if !c.Qualified || c.Family != i {
			continue
		}
		for _, s := range c.Steps {
			if trace.Replayable(s.Label) {
				cands = append(cands, i)
				break
			}
		}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		return r.Candidates[cands[a]].SavedTotal.Total() > r.Candidates[cands[b]].SavedTotal.Total()
	})
	type group struct {
		lead, best int
		saved      float64
	}
	var groups []group
	better := func(a, b model.Candidate) bool {
		if len(a.Steps) != len(b.Steps) {
			return len(a.Steps) > len(b.Steps)
		}
		if a.Specificity != b.Specificity {
			return a.Specificity > b.Specificity
		}
		return a.SavedTotal.Total() > b.SavedTotal.Total()
	}
	for _, i := range cands {
		c := r.Candidates[i]
		joined := false
		for g := range groups {
			if model.JaccardInts(c.SessionSet, r.Candidates[groups[g].lead].SessionSet) >= 0.5 {
				if better(c, r.Candidates[groups[g].best]) {
					groups[g].best = i
				}
				joined = true
				break
			}
		}
		if !joined {
			groups = append(groups, group{lead: i, best: i, saved: c.SavedTotal.Total()})
		}
	}
	out := make([]int, len(groups))
	for g := range groups {
		out[g] = groups[g].best
	}
	return out
}

func PackFiles(files map[string][]byte, executable func(string) bool) ([]byte, string, error) {
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	gz.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		body := files[n]
		mode := int64(0o644)
		if executable(n) {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(body)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:]), nil
}
