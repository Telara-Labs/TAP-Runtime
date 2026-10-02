package primitive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Family is a group of observed result-linked chains. It becomes a reusable
// procedure only when an executable API is established. Follow-ups can share
// the same head call; their counts and token opportunity are not independent
// predictions of savings. The head can come from alternative sources.
type Family struct {
	ID string `json:"id"`
	// Fingerprint names the family across runs, whatever its members: the
	// head command and whether it reads or may write. Decisions attach here.
	Fingerprint string `json:"fingerprint"`
	// Status says why it is shown (new, new since your last decision,
	// re-evaluated by new rules); Earlier is that last decision.
	Status  string `json:"status,omitempty"`
	Earlier string `json:"earlier,omitempty"`
	// Head is the most common first step; Sources are the alternatives.
	Head    string   `json:"head"`
	Sources []string `json:"sources,omitempty"`
	// FollowUps are the chains that act on the head's result.
	FollowUps []FollowUp `json:"followUps"`
	// Members are the primitives (exact chains, with their evidence) this
	// family groups.
	Members []string `json:"members"`
	// Inputs are the caller's values across members ("step:key"), with
	// argument names that carried the same value joined as aliases.
	Inputs         []string `json:"inputs,omitempty"`
	Effect         string   `json:"effect"`
	SessionCount   int      `json:"sessionCount"`
	ExecutionCount int      `json:"executionCount"`
	// SavedTokens and Saved are the historical model-turn cost after the
	// first call. They estimate an opportunity, not realized savings unless
	// the complete program contract is executable and later reused.
	SavedTokens float64     `json:"savedTokens"`
	Saved       trace.Usage `json:"saved"`
	// Confidence is the members' scores weighted by their runs; Weakest is
	// the lowest member score. NeedsDecision counts members with unresolved
	// claims.
	Confidence int `json:"confidence"`
	Weakest    int `json:"weakest"`
	// RelationshipConfidence is the weakest admitted follow-up's evidence
	// that it belongs to this head. Excluded paths retain their own scores.
	RelationshipConfidence int `json:"relationshipConfidence"`
	// APIConfidence is the weakest admitted path's existing confidence,
	// relationship, and call-shape support. It is evidence, not success probability.
	APIConfidence int `json:"apiConfidence"`
	// Values are the distinct arguments across the family's chains; Traced
	// are those whose source is known in every run (an earlier step's
	// result, or the caller). OpenQuestions are the points to resolve.
	// TurnsSaved counts historical follow-up calls. For an unresolved family
	// these are an opportunity estimate, not turns an installed program saves.
	TurnsSaved    int `json:"turnsSaved"`
	Values        int `json:"values"`
	Traced        int `json:"traced"`
	OpenQuestions int `json:"openQuestions"`
	// ReadToDecide counts runs whose output the agent then used to decide
	// its next call: evidence the output was read, so replaying the chain
	// would not remove those turns.
	ReadToDecide int `json:"readToDecide"`
	// Questions are the open questions in plain words, each naming its step.
	Questions     []string `json:"questions,omitempty"`
	NeedsDecision int      `json:"needsDecision"`
	Readiness     string   `json:"readiness"`
	// APIMode is a pre-review compilation verdict. A shared first call is
	// evidence of a pattern, not by itself an executable primitive API.
	APIMode    string   `json:"apiMode,omitempty"`
	APIChoices []string `json:"apiChoices,omitempty"`
	APIInputs  []string `json:"apiInputs,omitempty"`
	APIReason  string   `json:"apiReason,omitempty"`
}

// FollowUp is one chain after the head.
type FollowUp struct {
	Steps           []string `json:"steps"`
	Runs            int      `json:"runs"`
	Optional        bool     `json:"optional"`
	Members         []string `json:"members,omitempty"`
	PotentialTurns  int      `json:"potentialTurns,omitempty"`
	PotentialTokens float64  `json:"potentialTokens,omitempty"`
	// RelationshipScore counts executions with an explicit same-request
	// binding to the head's structured result, out of all usable executions.
	RelationshipScore   int    `json:"relationshipScore"`
	RelationshipSupport string `json:"relationshipSupport,omitempty"`
	ShapeScore          int    `json:"shapeScore"`
	ShapeSupport        string `json:"shapeSupport,omitempty"`
	Confidence          int    `json:"confidence"`
	APIMode             string `json:"apiMode,omitempty"`
	APIReason           string `json:"apiReason,omitempty"`
}

func execKey(ex Execution) string {
	var ids []string
	for _, c := range ex.Calls {
		ids = append(ids, ex.Session+"/"+strconv.Itoa(c.Index))
	}
	return strings.Join(ids, ",")
}

// dropFragments removes a primitive when every one of its runs is part of a
// run of a longer primitive: it is a piece of that chain, not a procedure of
// its own.
func dropFragments(ps []Primitive) ([]Primitive, int) {
	owner := map[string][]int{} // call -> primitives whose runs contain it
	for pi, p := range ps {
		for _, ex := range p.Executions {
			for _, c := range ex.Calls {
				k := ex.Session + "/" + strconv.Itoa(c.Index)
				owner[k] = append(owner[k], pi)
			}
		}
	}
	runCalls := func(p Primitive) []map[string]bool {
		var out []map[string]bool
		for _, ex := range p.Executions {
			m := map[string]bool{}
			for _, c := range ex.Calls {
				m[ex.Session+"/"+strconv.Itoa(c.Index)] = true
			}
			out = append(out, m)
		}
		return out
	}
	runs := make([][]map[string]bool, len(ps))
	for i := range ps {
		runs[i] = runCalls(ps[i])
	}
	var kept []Primitive
	dropped := 0
	for pi, p := range ps {
		inside := len(p.Executions) > 0
		for ri, ex := range p.Executions {
			if len(ex.Calls) == 0 {
				inside = false
				break
			}
			first := ex.Session + "/" + strconv.Itoa(ex.Calls[0].Index)
			found := false
			for _, qi := range owner[first] {
				if qi == pi || len(ps[qi].Steps) <= len(p.Steps) {
					continue
				}
				for _, big := range runs[qi] {
					all := len(big) > len(runs[pi][ri])
					for k := range runs[pi][ri] {
						all = all && big[k]
					}
					if all {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				inside = false
				break
			}
		}
		if inside {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	return kept, dropped
}

// families groups primitives into proposed procedures:
//   - chains that start with the same command are one procedure with
//     optional follow-ups (every chain's steps are linked by its results);
//   - procedures whose heads only read and that run the same follow-ups on
//     the value they produce are one procedure with alternative sources;
//   - a single call left on its own is the tool, not a procedure.
func families(ps []Primitive) []Family {
	type fam struct {
		heads   map[string]int
		members []int
		tails   map[string]int
		read    bool
	}
	byHead := map[string]*fam{}
	var order []string
	var solo []*fam
	for i, p := range ps {
		if len(p.Steps) < 2 {
			solo = append(solo, &fam{heads: map[string]int{headKey(p.Steps[0]): p.ExecutionCount}, members: []int{i}, tails: map[string]int{tail(p): p.ExecutionCount}, read: headReads(p)})
			continue
		}
		h := headKey(p.Steps[0])
		f := byHead[h]
		if f == nil {
			f = &fam{heads: map[string]int{h: 0}, tails: map[string]int{}, read: true}
			byHead[h] = f
			order = append(order, h)
		}
		f.heads[h] += p.ExecutionCount
		f.members = append(f.members, i)
		f.tails[tail(p)] += p.ExecutionCount
		f.read = f.read && headReads(p)
	}
	// Merge read-only heads that run a shared follow-up on what they return.
	parent := map[string]string{}
	var find func(string) string
	find = func(h string) string {
		if parent[h] == "" || parent[h] == h {
			return h
		}
		r := find(parent[h])
		parent[h] = r
		return r
	}
	for i, a := range order {
		for _, b := range order[i+1:] {
			fa, fb := byHead[a], byHead[b]
			if !fa.read || !fb.read {
				continue
			}
			for t := range fa.tails {
				if fb.tails[t] > 0 {
					parent[find(b)] = find(a)
					break
				}
			}
		}
	}
	merged := map[string]*fam{}
	var roots []string
	for _, h := range order {
		r := find(h)
		m := merged[r]
		if m == nil {
			m = &fam{heads: map[string]int{}, tails: map[string]int{}}
			merged[r] = m
			roots = append(roots, r)
		}
		for k, v := range byHead[h].heads {
			m.heads[k] += v
		}
		for k, v := range byHead[h].tails {
			m.tails[k] += v
		}
		m.members = append(m.members, byHead[h].members...)
	}
	var all []*fam
	for _, r := range roots {
		all = append(all, merged[r])
	}
	all = append(all, solo...)

	var out []Family
	for _, f := range all {
		var members []Primitive
		for _, i := range f.members {
			members = append(members, ps[i])
		}
		if len(members) == 1 && len(members[0].Steps) == 1 {
			continue // one call on its own is the tool
		}
		out = append(out, buildFamily(f.heads, f.tails, members))
	}
	sort.SliceStable(out, func(i, j int) bool { return inputEquivalent(out[i].Saved) > inputEquivalent(out[j].Saved) })
	return out
}

// tail is what follows the head, each step by its command: pipeline stages
// after a command stay in the member chains, not in the family's identity.
func tail(p Primitive) string {
	var ks []string
	for _, s := range p.Steps[1:] {
		ks = append(ks, headKey(s))
	}
	return strings.Join(ks, " > ")
}

// headKey is a step's command: a shell pipeline's first command, or the tool.
func headKey(op string) string {
	if strings.HasPrefix(op, "sh:") {
		return strings.Split(op, "+")[0]
	}
	return op
}

func headReads(p Primitive) bool {
	return len(p.StepEffects) > 0 && p.StepEffects[0] == "read"
}

func buildFamily(heads, tails map[string]int, members []Primitive) Family {
	f := Family{Effect: "read", Readiness: "candidate", Weakest: 100}
	weighted, weight := 0.0, 0
	best := -1
	var hs []string
	for h, n := range heads {
		hs = append(hs, h)
		if n > best || n == best && h < f.Head {
			f.Head, best = h, n
		}
	}
	sort.Strings(hs)
	for _, h := range hs {
		if h != f.Head {
			f.Sources = append(f.Sources, h)
		}
	}
	// A run is identified by its head call: follow-ups on the same created
	// or found value are one execution of the family.
	runs := map[string]bool{}
	sessions := map[string]bool{}
	traced := map[string]bool{} // op|arg -> traced in every chain
	questions := map[string]bool{}
	readable := map[string]bool{}
	inputs := map[string]map[string]bool{} // step:op -> arg keys
	tailMembers := map[string][]string{}
	tailTurns := map[string]int{}
	tailTokens := map[string]float64{}
	for _, p := range members {
		t := tail(p)
		tailMembers[t] = append(tailMembers[t], p.ID)
		tailTokens[t] += inputEquivalent(p.Saved)
		f.Members = append(f.Members, p.ID)
		f.SavedTokens += p.SavedTokens
		f.Saved = f.Saved.Add(p.Saved)
		if rankEffect(p.Effect) > rankEffect(f.Effect) {
			f.Effect = p.Effect
		}
		if p.Confidence.Overall < f.Weakest {
			f.Weakest = p.Confidence.Overall
		}
		weighted += float64(p.Confidence.Overall * p.ExecutionCount)
		weight += p.ExecutionCount
		if p.Confidence.Readiness == "needs_decision" {
			f.NeedsDecision++
			f.Readiness = "needs_decision"
		}
		for _, cl := range p.Confidence.Claims {
			if cl.Dimension != "bindings" {
				continue
			}
			var step int
			var arg string
			if _, err := fmt.Sscanf(cl.Subject, "step %d %s", &step, &arg); err != nil || step < 1 || step > len(p.Steps) {
				continue
			}
			k := headKey(p.Steps[step-1]) + "|" + normKey(arg)
			var got, of int
			fmt.Sscanf(cl.Support, "%d/%d", &got, &of)
			ok, seen := traced[k]
			traced[k] = (ok || !seen) && got == of
		}
		for _, q := range p.Confidence.NeedsReview {
			questions[p.ID+" "+q] = true
			readable[readableQuestion(p, q)] = true
		}
		for _, ex := range p.Executions {
			if ex.Overlaps != "" || len(ex.Calls) == 0 {
				continue
			}
			if !runs[ex.Session+"/"+strconv.Itoa(ex.Calls[0].Index)] && ex.ThenDecided {
				f.ReadToDecide++
			}
			runs[ex.Session+"/"+strconv.Itoa(ex.Calls[0].Index)] = true
			f.TurnsSaved += len(ex.Calls) - 1
			tailTurns[t] += len(ex.Calls) - 1
			sessions[ex.Session] = true
		}
		for _, in := range p.Inputs {
			op := headKey(p.Steps[in.Step-1])
			if inputs[op] == nil {
				inputs[op] = map[string]bool{}
			}
			inputs[op][in.Key] = true
		}
	}
	f.ExecutionCount, f.SessionCount = len(runs), len(sessions)
	headEffect := "read"
	for _, p := range members {
		if len(p.StepEffects) > 0 && rankEffect(p.StepEffects[0]) > rankEffect(headEffect) {
			headEffect = p.StepEffects[0]
		}
	}
	class := "read"
	if headEffect != "read" {
		class = "may-write"
	}
	f.Fingerprint = headKey(f.Head) + "|" + class
	f.Values, f.OpenQuestions = len(traced), len(questions)
	for q := range readable {
		f.Questions = append(f.Questions, q)
	}
	sort.Strings(f.Questions)
	for _, ok := range traced {
		if ok {
			f.Traced++
		}
	}
	if weight > 0 {
		f.Confidence = int(math.Round(weighted / float64(weight)))
	}
	total := 0
	for _, n := range tails {
		total += n
	}
	for t, n := range tails {
		if t == "" {
			continue
		}
		f.FollowUps = append(f.FollowUps, FollowUp{Steps: strings.Split(t, " > "), Runs: n, Optional: n < total,
			Members: tailMembers[t], PotentialTurns: tailTurns[t], PotentialTokens: tailTokens[t]})
	}
	sort.Slice(f.FollowUps, func(i, j int) bool { return f.FollowUps[i].Runs > f.FollowUps[j].Runs })
	var ops []string
	for op := range inputs {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		var keys []string
		for k := range inputs[op] {
			keys = append(keys, k)
		}
		f.Inputs = append(f.Inputs, short(op)+": "+strings.Join(aliases(keys), ", "))
	}
	sort.Strings(f.Members)
	sum := sha256.Sum256([]byte(strings.Join(f.Members, ",")))
	f.ID = "pf_" + hex.EncodeToString(sum[:6])
	return f
}

// aliases joins argument names that differ only in case or separators
// (issue_key / issueKey), so one value is not listed as two inputs.
func aliases(keys []string) []string {
	by := map[string][]string{}
	var order []string
	for _, k := range keys {
		n := normKey(k)
		if by[n] == nil {
			order = append(order, n)
		}
		by[n] = append(by[n], k)
	}
	sort.Strings(order)
	var out []string
	for _, n := range order {
		ks := by[n]
		sort.Strings(ks)
		out = append(out, strings.Join(ks, "|"))
	}
	return out
}

// Exploration reports a family whose output the agent mostly read to decide
// what to do next: replaying it would not remove those turns.
func (f Family) Exploration() bool { return 2*f.ReadToDecide > f.ExecutionCount }

// readableQuestion names the step a question is about ("step 2 issue_key:
// ..." becomes "jira add comment, issue_key: ...").
func readableQuestion(p Primitive, q string) string {
	var n int
	var rest string
	if _, err := fmt.Sscanf(q, "step %d", &n); err != nil || n < 1 || n > len(p.Steps) {
		return q
	}
	_, rest, _ = strings.Cut(q, " ")
	_, rest, _ = strings.Cut(rest, " ")
	return display(p.Steps[n-1]) + ", " + rest
}
