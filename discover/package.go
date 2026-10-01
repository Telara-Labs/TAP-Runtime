package discover

import (
	"sort"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Package returns the draft as a gzip tar, the form a registry package and
// `telara tap pull` use, and its digest. Entries are sorted and carry a fixed
// time, so the same draft always has the same digest.
func PackageDraft(d *model.Draft) ([]byte, string, error) {
	files, err := DraftArtifacts(d)
	if err != nil {
		return nil, "", err
	}
	return packFiles(files, func(n string) bool { return n == "main.sh" })
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
