package primitive

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Rubric names how the detail scores are calculated. Each claim scores how
// consistently the runs that show it support it: every run's observation
// gets a quality, and the claim's score is their mean; a primitive's score is
// its weakest area. It describes the evidence for refinement. It is not a
// probability that the chain is a primitive or that a program will work, so
// the menu never shows it as one: it shows values traced and open questions,
// and the run count separately.
const Rubric = "run consistency v3"

// Observation quality: how strongly one run supports a binding.
const (
	qExplicit = 1.0 // a structured field of the producer's result
	qInferred = 0.8 // a whole output line, or its first field
	qInput    = 1.0 // the caller supplies it (typed in the request, or no source)
	qConflict = 0.0 // taken from a different step than in most runs
)

// Claim is one scored statement about a primitive's flow.
type Claim struct {
	ID        string `json:"id"`
	Dimension string `json:"dimension"`
	Subject   string `json:"subject"`
	Score     int    `json:"score"`
	// Support is the runs behind the score ("38/40").
	Support string `json:"support"`
	Reason  string `json:"reason"`
}

// Dimension is the weakest of its claims' consistency; it is not applicable
// when the flow has no claim of that kind.
type Dimension struct {
	Name       string `json:"name"`
	Applicable bool   `json:"applicable"`
	Score      int    `json:"score"`
}

// Confidence is a primitive's calculated support and what needs review.
type Confidence struct {
	Rubric     string      `json:"rubric"`
	Overall    int         `json:"overall"`
	Dimensions []Dimension `json:"dimensions"`
	Claims     []Claim     `json:"claims"`
	// Notes are facts that do not change the score yet: long gaps, whose
	// cutoff is not validated.
	Notes []string `json:"notes,omitempty"`
	// NeedsReview are claims that are unresolved, weakest first.
	NeedsReview []string `json:"needsReview,omitempty"`
	// Readiness is needs_decision when a claim is unresolved (a value from
	// different steps, a source only mentioned in text, a selection with no
	// consistent rule), otherwise candidate. Execution validation is
	// separate and not run here.
	Readiness string `json:"readiness"`
	// Requirements are capabilities the flow would need that the TAP
	// runtime does not offer (it has no bounded reasoning step).
	Requirements []string `json:"requirements,omitempty"`
}

var dimensionOrder = []string{"bindings", "transformations", "control", "completion"}

func pct(x float64) int { return int(math.Round(100 * x)) }

// score calculates a primitive's confidence from its counted runs.
func score(p Primitive) Confidence {
	c := Confidence{Rubric: Rubric, Readiness: "candidate"}
	add := func(dim, subject string, s float64, k, n int, reason string) {
		c.Claims = append(c.Claims, Claim{ID: fmt.Sprintf("C%02d", len(c.Claims)+1), Dimension: dim, Subject: subject,
			Score: pct(s), Support: fmt.Sprintf("%d/%d", k, n), Reason: reason})
	}
	unresolved := func(text string) {
		c.Readiness = "needs_decision"
		c.NeedsReview = append(c.NeedsReview, text)
	}
	var runs []Execution
	for _, ex := range p.Executions {
		if ex.Overlaps == "" {
			runs = append(runs, ex)
		}
	}
	n := len(runs)

	// Bindings: one observation per run for each argument.
	type key struct {
		step int
		arg  string
	}
	obs := map[key][]Observed{}
	var order []key
	for _, ex := range runs {
		seen := map[key]bool{}
		for _, o := range ex.Observed {
			k := key{o.Step, o.Arg}
			if seen[k] {
				continue
			}
			seen[k] = true
			if obs[k] == nil {
				order = append(order, k)
			}
			obs[k] = append(obs[k], o)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].step != order[j].step {
			return order[i].step < order[j].step
		}
		return order[i].arg < order[j].arg
	})
	for _, k := range order {
		os := obs[k]
		producers := map[int]int{}
		for _, o := range os {
			if o.Source == "step" && o.Label != Ambiguous {
				producers[o.From]++
			}
		}
		modal, modalN := 0, 0
		for f, m := range producers {
			if m > modalN || m == modalN && f < modal {
				modal, modalN = f, m
			}
		}
		sum := 0.0
		var explicit, inferred, typed, input, ambiguous, conflict int
		for _, o := range os {
			switch {
			case o.Source == "step" && o.Label != Ambiguous && o.From != modal:
				sum += qConflict
				conflict++
			case o.Source == "step" && o.Label == Explicit:
				sum += qExplicit
				explicit++
			case o.Source == "step" && o.Label == Inferred:
				sum += qInferred
				inferred++
			case o.Label == Ambiguous:
				// No proven source: by rule a caller input. It also appeared in
				// an earlier output's text, which refinement may turn into a
				// binding.
				sum += qInput
				ambiguous++
			case o.Label == Inferred: // given in the request
				sum += qInput
				typed++
			default:
				sum += qInput
				input++
			}
		}
		var parts []string
		for _, x := range []struct {
			n    int
			what string
		}{{explicit, fmt.Sprintf("a structured field of step %d", modal)}, {inferred, fmt.Sprintf("an output line of step %d", modal)},
			{typed, "typed in the request"}, {input, "a caller input"}, {ambiguous, "a caller input that also appeared in earlier output text"}, {conflict, "taken from another step"}} {
			if x.n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", x.n, x.what))
			}
		}
		subject := fmt.Sprintf("step %d %s", k.step, k.arg)
		add("bindings", subject, sum/float64(len(os)), len(os)-conflict, len(os), strings.Join(parts, ", "))
		if conflict > 0 {
			unresolved(fmt.Sprintf("%s: taken from different steps in %d of %d runs", subject, conflict, len(os)))
		}
	}

	// Selections: which list item a step took, run by run.
	type sel struct {
		step int
		arg  string
		from int
	}
	picks := map[sel]map[string]int{}
	var selOrder []sel
	for _, ex := range runs {
		for _, o := range ex.Observed {
			m := listIndex.FindStringSubmatch(o.Selector)
			if o.Source != "step" || o.Label != Explicit || m == nil {
				continue
			}
			s := sel{o.Step, o.Arg, o.From}
			if picks[s] == nil {
				picks[s] = map[string]int{}
				selOrder = append(selOrder, s)
			}
			picks[s][m[1]]++
		}
	}
	loop := map[int]bool{}
	for _, l := range p.Loops {
		loop[l] = true
	}
	for _, s := range selOrder {
		if loop[s.step] {
			continue // a loop takes every item; nothing is selected
		}
		total, best, bestN := 0, "", 0
		for idx, m := range picks[s] {
			total += m
			if m > bestN || m == bestN && idx < best {
				best, bestN = idx, m
			}
		}
		subject := fmt.Sprintf("step %d %s from step %d's list", s.step, s.arg, s.from)
		reason := fmt.Sprintf("item [%s] in %d of %d runs", best, bestN, total)
		if bestN == total {
			reason += "; a fixed position, codeable without judgment"
		} else {
			unresolved(fmt.Sprintf("%s: different items taken (%s); the rule is not known", subject, reason))
			c.Requirements = append(c.Requirements, "The TAP runtime has no bounded reasoning step: the rule choosing "+subject+" must become code or a caller input")
		}
		add("transformations", subject, float64(bestN)/float64(total), bestN, total, reason)
	}

	// Control: loops and && inside a call.
	for _, l := range p.Loops {
		k := 0
		for _, ex := range runs {
			items := 0
			for _, cl := range ex.Calls {
				if cl.Step == l {
					items++
				}
			}
			if items > 1 {
				k++
			}
		}
		// Every repetition took a different item of the same result (that is
		// how loops are found); runs that took one item fit the same loop.
		add("control", fmt.Sprintf("step %d loop", l), 1, k, n,
			fmt.Sprintf("repeated over different items of one earlier result in %d of %d runs; the others took one item", k, n))
	}
	for _, ce := range p.ControlEdges {
		add("control", ce, 1, n, n, "&& in the recorded command")
	}

	// Completion: every call in the run recorded as succeeding.
	ok := 0
	for _, ex := range runs {
		all := true
		for _, cl := range ex.Calls {
			all = all && cl.OK
		}
		if all {
			ok++
		}
	}
	add("completion", "every call succeeded", float64(ok)/math.Max(1, float64(n)), ok, n, "runs with every call recorded as succeeding")

	// Long gaps are a note: the cutoff is a proposal, not a validated rule.
	long, untimed := 0, 0
	for _, ex := range runs {
		switch {
		case ex.MaxGapSeconds < 0:
			untimed++
		case ex.MaxGapSeconds >= 20*60:
			long++
		}
	}
	if long > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("%d of %d runs have 20 minutes or more between two calls (a long command, a wait, or separate work); not scored until the cutoff is validated", long, n))
	}
	if untimed > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("%d of %d runs have no recorded call times", untimed, n))
	}

	c.Overall = 100
	for _, name := range dimensionOrder {
		d := Dimension{Name: name, Score: 100}
		for _, cl := range c.Claims {
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
	// A long gap is a point to check, listed with the open questions.
	if long > 0 {
		c.NeedsReview = append(c.NeedsReview, fmt.Sprintf("%d of %d runs: 20 minutes or more between two calls; check it is one operation", long, n))
	}
	sort.Strings(c.NeedsReview)
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
	return fmt.Sprintf("weakest area %d/100 (%s) · %s · %s", c.Overall, c.Rubric, strings.Join(parts, " | "), c.Readiness)
}
