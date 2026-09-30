package discover

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

// Package returns the draft as a gzip tar, the form a registry package and
// `telara tap pull` use, and its digest. Entries are sorted and carry a fixed
// time, so the same draft always has the same digest.
func (d *Draft) Package() ([]byte, string, error) {
	files, err := d.Artifacts()
	if err != nil {
		return nil, "", err
	}
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
		if n == "main.sh" {
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
func (r *Report) PatternRoutines() []int {
	var cands []int
	for i, c := range r.Candidates {
		if !c.Qualified || c.Family != i {
			continue
		}
		for _, s := range c.Steps {
			if replayable(s.Label) {
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
	better := func(a, b Candidate) bool {
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
			if jaccardInts(c.sessionSet, r.Candidates[groups[g].lead].sessionSet) >= 0.5 {
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
