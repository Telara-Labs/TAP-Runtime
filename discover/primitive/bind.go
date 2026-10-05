package primitive

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// Evidence levels for a binding.
const (
	Explicit  = "explicit"  // a structured field of the producer's result
	Inferred  = "inferred"  // a whole output line or its first field; or given in the request
	Ambiguous = "ambiguous" // the value only appears inside text, or several unrelated producers
	Missing   = "missing"   // no source in this execution: a caller input
)

// flows reports a value type that can carry a result into a later call:
// identifiers, URLs, numbers and paths. Words are choices or settings; text
// is written per run.
func flows(a arg) bool {
	switch a.typ {
	case trace.SlotID, trace.SlotURL, trace.SlotNumber, trace.SlotPath:
		return len(a.value) >= 4
	}
	return false
}

// result indexes one recorded result once, so finding where a value came
// from is a lookup rather than a scan of every earlier output.
type result struct {
	hits   map[string]hit // values a parser takes: structured fields, lines, first fields
	digits map[string]bool
	c      trace.Call
}

type hit struct{ level, selector string }

func indexResult(c trace.Call) *result {
	r := &result{hits: map[string]hit{}, digits: map[string]bool{}, c: c}
	put := func(v string, h hit) {
		if old, ok := r.hits[v]; !ok || rank(h.level) > rank(old.level) {
			r.hits[v] = h
		}
	}
	for i, id := range c.OutIDs {
		if i < len(c.OutPaths) && c.OutPaths[i] != "" && c.OutPaths[i] != "*" {
			put(id, hit{Explicit, c.OutPaths[i]})
			continue
		}
		// An identifier at the start of an output line, ending the line or
		// its first field, is what a line parser takes.
		if i < len(c.OutCtx) {
			before, after, _ := strings.Cut(c.OutCtx[i], "\x00")
			if strings.TrimSpace(before) == "" && (after == "" || after == ":" || after == " " || after == "\t") {
				put(id, hit{Inferred, "first field of an output line"})
			}
		}
	}
	for _, line := range strings.Split(c.Output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		put(line, hit{Inferred, "output line"})
		if f := strings.FieldsFunc(line, func(r rune) bool { return r == ':' || r == ' ' || r == '\t' }); len(f) > 0 {
			put(f[0], hit{Inferred, "first field of an output line"})
		}
	}
	for _, t := range digitToken.FindAllString(c.Output, -1) {
		r.digits[t] = true
	}
	return r
}

// match says how a producer's recorded result supplies a value, and the
// selector that takes it out. A value merely mentioned inside text proves
// nothing: it is ambiguous.
func match(v string, r *result) (string, string) {
	if h, ok := r.hits[v]; ok {
		return h.level, h.selector
	}
	if trace.InResult(v, trace.Step{Output: r.c.Output, OutIDs: r.c.OutIDs, OutTokens: r.c.OutTokens}) {
		return Ambiguous, "mentioned in output text"
	}
	return "", ""
}

func rank(l string) int {
	switch l {
	case Explicit:
		return 3
	case Inferred:
		return 2
	case Ambiguous:
		return 1
	}
	return 0
}

// link records every argument's source. An execution is one user request:
// a value produced in an earlier request is not linked across that boundary
// (it is the caller's input there, with the reason recorded). Within a
// request the value's origin is the earliest producer; later producers that
// only echo it (they took it from that origin) do not compete. Explicit and
// inferred sources become edges; an ambiguous one is recorded, not chained.
func link(nodes []node, requests []string) {
	lowered := make([]string, len(requests))
	for i, r := range requests {
		lowered[i] = strings.ToLower(r)
	}
	requests = lowered
	for j := range nodes {
		n := &nodes[j]
		for a := range n.args {
			ag := &n.args[a]
			ag.given = "unknown"
			if !flows(*ag) {
				if requested(ag.value, requests, n.request) {
					ag.given = "request"
				}
				continue
			}
			obs := &Observed{Arg: ag.key, Source: "input", Label: Missing}
			ag.obs = obs
			if requested(ag.value, requests, n.request) {
				ag.given = "request"
				obs.Label, obs.Reason = Inferred, "given in the user's request"
				continue
			}
			type cand struct {
				i              int
				level, selName string
			}
			var cands []cand
			best := 0
			earlier := false
			for i := 0; i < j; i++ {
				l, sel := match(ag.value, nodes[i].res)
				if l == "" {
					continue
				}
				if nodes[i].request != n.request {
					if rank(l) >= rank(Inferred) {
						earlier = true
					}
					continue
				}
				cands = append(cands, cand{i, l, sel})
				if rank(l) > best {
					best = rank(l)
				}
			}
			var top []cand
			for _, c := range cands {
				if rank(c.level) == best {
					top = append(top, c)
				}
			}
			switch {
			case len(top) == 0 && earlier:
				obs.Reason = "produced in an earlier request; execution boundary"
			case len(top) == 0:
				obs.Reason = "no recorded source"
			case best == rank(Ambiguous):
				obs.Label, obs.Source = Ambiguous, "step"
				obs.Reason = fmt.Sprintf("only mentioned inside the text output of %d earlier call(s)", len(top))
			default:
				origin := top[0]
				echoes := true
				for _, c := range top[1:] {
					if !tookFrom(nodes[c.i], ag.value) {
						echoes = false
					}
				}
				if !echoes {
					obs.Label, obs.Source = Ambiguous, "step"
					obs.Reason = fmt.Sprintf("%d earlier calls returned it independently", len(top))
					break
				}
				obs.Label, obs.Source, obs.Selector = origin.level, "step", origin.selName
				obs.Reason = "taken from an earlier result"
				if len(top) > 1 {
					obs.Reason += fmt.Sprintf("; echoed by %d later call(s)", len(top)-1)
				}
				ag.given = "step"
				obs.From = origin.i // node index; rewritten to a step position later
				n.parents = append(n.parents, edge{from: origin.i, key: ag.key, label: origin.level, selector: origin.selName})
			}
		}
	}
}

// tookFrom reports that a node used the value as an argument: a producer
// that echoes a value it was given is not an independent source of it.
func tookFrom(n node, v string) bool {
	for _, a := range n.args {
		if a.value == v {
			return true
		}
	}
	return false
}

// requested reports v in any request up to upto; requests are lowercased.
func requested(v string, requests []string, upto int) bool {
	for r := 0; r <= upto && r < len(requests); r++ {
		if trace.InLoweredRequest(v, requests[r]) {
			return true
		}
	}
	return false
}

// digitToken finds the number-bearing pieces of a value (120 in "120,160p").
var digitToken = regexp.MustCompile(`[A-Za-z0-9_.-]*[0-9][A-Za-z0-9_.-]*`)

// decide marks calls whose arguments the agent constructed from what an
// earlier call just returned: an argument that is not itself a value of that
// output, yet carries a number from it (sed -n 120,160p after grep printed
// line 120). The turn between the calls was a decision, not a hand-off, so
// the call starts a new chain: its sources are not linked. It returns how
// many calls were decisions.
func decide(nodes []node) int {
	n := 0
	for j := range nodes {
		if constructed(nodes, j) {
			nodes[j].decided = true
			nodes[j].parents = nil
			for a := range nodes[j].args {
				if nodes[j].args[a].given == "step" {
					nodes[j].args[a].given = "unknown"
				}
				if o := nodes[j].args[a].obs; o != nil && o.Source == "step" {
					o.Source, o.Label = "input", Missing
					o.Reason = "decided from an earlier output in the same turn sequence; starts a new chain"
				}
			}
			n++
		}
	}
	return n
}

func constructed(nodes []node, j int) bool {
	n := nodes[j]
	for _, a := range n.args {
		if a.given != "unknown" || a.typ == trace.SlotText || a.typ == trace.SlotFlag {
			continue
		}
		for i := j - 1; i >= 0 && nodes[i].request == n.request; i-- {
			if l, _ := match(a.value, nodes[i].res); l != "" {
				return false // a value the output held: a selection, not a construction
			}
			for _, tok := range digitToken.FindAllString(a.value, -1) {
				if len(tok) >= 2 && tok != a.value && nodes[i].res.digits[tok] {
					return true
				}
			}
		}
	}
	return false
}
