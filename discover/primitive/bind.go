package primitive

import (
	"fmt"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
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

// match says how a producer's recorded result supplies a value, and the
// selector that takes it out. A value merely mentioned inside text proves
// nothing: it is ambiguous.
func match(v string, c trace.Call) (string, string) {
	for i, id := range c.OutIDs {
		if id != v {
			continue
		}
		if i < len(c.OutPaths) && c.OutPaths[i] != "" && c.OutPaths[i] != "*" {
			return Explicit, c.OutPaths[i]
		}
		// An identifier at the start of an output line, ending the line or
		// its first field, is what a line parser takes.
		if i < len(c.OutCtx) {
			before, after, _ := strings.Cut(c.OutCtx[i], "\x00")
			if strings.TrimSpace(before) == "" && (after == "" || after == ":" || after == " " || after == "\t") {
				return Inferred, "first field of an output line"
			}
		}
	}
	for _, line := range strings.Split(c.Output, "\n") {
		line = strings.TrimSpace(line)
		if line == v {
			return Inferred, "output line"
		}
		if f := strings.FieldsFunc(line, func(r rune) bool { return r == ':' || r == ' ' || r == '\t' }); len(f) > 0 && f[0] == v {
			return Inferred, "first field of an output line"
		}
	}
	if trace.InResult(v, trace.Step{Output: c.Output, OutIDs: c.OutIDs, OutTokens: c.OutTokens}) {
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
				l, sel := match(ag.value, nodes[i].c)
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

func requested(v string, requests []string, upto int) bool {
	for r := 0; r <= upto && r < len(requests); r++ {
		if trace.InRequest(v, requests[r]) {
			return true
		}
	}
	return false
}
