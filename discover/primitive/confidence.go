package primitive

import (
	"fmt"
	"sort"
	"strings"
)

// Rubric names the scoring rules below. Scores are an ordinal evidence
// rubric, not probabilities: 0 missing or contradicted, 25 proposed, 50
// inferred, 75 corroborated, 100 explicitly established (or required by the
// user's own rule). Repetition alone never raises a claim to 100.
const Rubric = "evidence rubric v1"

// Claim is one scored statement about a primitive's flow.
type Claim struct {
	ID        string `json:"id"`
	Dimension string `json:"dimension"`
	Subject   string `json:"subject"`
	Score     int    `json:"score"`
	// Basis is observed, user_required or proposed.
	Basis  string `json:"basis"`
	Reason string `json:"reason"`
}

// Dimension is the minimum score of its claims; it is not applicable when
// the flow has no claim of that kind.
type Dimension struct {
	Name       string `json:"name"`
	Applicable bool   `json:"applicable"`
	Score      int    `json:"score"`
}

// Confidence is a primitive's scored evidence and what needs review.
type Confidence struct {
	Rubric     string      `json:"rubric"`
	Overall    int         `json:"overall"`
	Dimensions []Dimension `json:"dimensions"`
	Claims     []Claim     `json:"claims"`
	// NeedsReview are the required claims scored below 50, weakest first.
	NeedsReview []string `json:"needsReview,omitempty"`
	// Readiness is needs_decision when any required claim is unresolved,
	// otherwise candidate. Execution validation is separate and not run here.
	Readiness string `json:"readiness"`
	// Requirements are execution capabilities the flow would need that the
	// TAP runtime does not offer (it has no bounded reasoning step).
	Requirements []string `json:"requirements,omitempty"`
}

var dimensionOrder = []string{"bindings", "transformations", "boundaries", "control", "completion"}

// score applies the rubric to a primitive. Each dimension is the minimum of
// its claims and the overall score is the minimum applicable dimension, so
// one weak claim is never averaged away.
func score(p Primitive) Confidence {
	var claims []Claim
	add := func(dim, subject string, s int, basis, reason string) {
		claims = append(claims, Claim{ID: fmt.Sprintf("C%02d", len(claims)+1), Dimension: dim, Subject: subject, Score: s, Basis: basis, Reason: reason})
	}
	observed := map[string]int{}
	for _, ex := range p.Executions {
		seen := map[string]bool{}
		for _, o := range ex.Observed {
			k := fmt.Sprintf("%d:%s", o.Step, o.Arg)
			if !seen[k] {
				seen[k] = true
				observed[k]++
			}
		}
	}
	for _, b := range p.Bindings {
		subject := fmt.Sprintf("step %d %s", b.Step, b.Arg)
		n := observed[fmt.Sprintf("%d:%s", b.Step, b.Arg)]
		switch {
		case len(b.Contradicting) > 0:
			add("bindings", subject, 25, "observed", fmt.Sprintf("%d execution(s) contradict the majority source", len(b.Contradicting)))
		case b.Source == "step" && b.Label == Explicit && n >= 2:
			add("bindings", subject, 75, "observed", fmt.Sprintf("structured result field %s in %d executions", b.Selector, n))
		case b.Source == "step" && b.Label == Explicit:
			add("bindings", subject, 50, "observed", "structured result field in a single execution")
		case b.Source == "step" && b.Label == Inferred:
			add("bindings", subject, 50, "observed", "taken by "+b.Selector)
		case b.Label == Ambiguous:
			add("bindings", subject, 25, "proposed", "source ambiguous: "+strings.Join(b.Reasons, "; "))
		case b.Source == "input" && b.Label == Inferred:
			add("bindings", subject, 75, "observed", "given in the user's request")
		case b.Source == "input":
			add("bindings", subject, 100, "user_required", "no source in the execution: a declared invocation input")
		}
	}
	for _, u := range p.Unresolved {
		if strings.Contains(u, "selection rule") {
			add("transformations", u, 25, "proposed", "one item taken from a returned list; the rule that picks it is not known")
		}
	}
	timed, long := true, 0
	for _, ex := range p.Executions {
		if ex.MaxGapSeconds < 0 {
			timed = false
		}
		if ex.MaxGapSeconds >= 20*60 {
			long++
		}
	}
	switch {
	case !timed:
		add("boundaries", "execution timing", 0, "observed", "some executions have no recorded call times")
	case long > 0:
		add("boundaries", "execution boundary", 25, "proposed", fmt.Sprintf("%d execution(s) have a gap of 20 minutes or more; continuity unverified", long))
	default:
		add("boundaries", "execution boundary", 50, "observed", "each execution lies in one request with short gaps; the gap cutoff itself is not validated")
	}
	for _, l := range p.Loops {
		add("control", fmt.Sprintf("step %d loop", l), 50, "observed", "each repetition takes a different item from the same earlier result")
	}
	for _, c := range p.ControlEdges {
		add("control", c, 100, "observed", "&& in the recorded command")
	}
	ok := true
	for _, ex := range p.Executions {
		for _, c := range ex.Calls {
			if !c.OK {
				ok = false
			}
		}
	}
	if ok {
		add("completion", "every call succeeded", 75, "observed", fmt.Sprintf("all calls in %d executions recorded as succeeding", len(p.Executions)))
	} else {
		add("completion", "every call succeeded", 50, "observed", "some calls have no recorded outcome")
	}

	c := Confidence{Rubric: Rubric, Claims: claims, Overall: 100, Readiness: "candidate"}
	for _, name := range dimensionOrder {
		d := Dimension{Name: name, Score: 100}
		for _, cl := range claims {
			if cl.Dimension == name {
				d.Applicable = true
				if cl.Score < d.Score {
					d.Score = cl.Score
				}
			}
		}
		if !d.Applicable {
			d.Score = 0
		} else if d.Score < c.Overall {
			c.Overall = d.Score
		}
		c.Dimensions = append(c.Dimensions, d)
	}
	weak := append([]Claim(nil), claims...)
	sort.SliceStable(weak, func(i, j int) bool { return weak[i].Score < weak[j].Score })
	for _, cl := range weak {
		if cl.Score < 50 {
			c.NeedsReview = append(c.NeedsReview, fmt.Sprintf("%s %s (%d): %s", cl.ID, cl.Subject, cl.Score, cl.Reason))
			c.Readiness = "needs_decision"
		}
	}
	for _, cl := range claims {
		if cl.Dimension == "transformations" {
			c.Requirements = append(c.Requirements, "The TAP runtime has no bounded reasoning step: the selection rule must become deterministic code or a caller input ("+cl.Subject+")")
		}
	}
	return c
}

// Summary renders the confidence line shown with a primitive.
func (c Confidence) Summary() string {
	var parts []string
	for _, d := range c.Dimensions {
		if d.Applicable {
			parts = append(parts, fmt.Sprintf("%s %d", d.Name, d.Score))
		}
	}
	return fmt.Sprintf("%d/100 (heuristic; %s) · %s · %s", c.Overall, c.Rubric, strings.Join(parts, " | "), c.Readiness)
}
