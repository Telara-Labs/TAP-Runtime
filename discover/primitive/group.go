package primitive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// structOp is a node's operation without its values: every stage of a shell
// pipeline (a sort or a filter changes the result, so it stays), or the
// resolved tool.
func structOp(n node) string { return n.base }

// layered is a node and its sources within d hops, in call order.
func layered(g []node, j, d int) []int {
	seen := map[int]bool{j: true}
	frontier := []int{j}
	for i := 0; i < d && len(frontier) > 0; i++ {
		var next []int
		for _, k := range frontier {
			for _, e := range g[k].parents {
				if !seen[e.from] {
					seen[e.from] = true
					next = append(next, e.from)
				}
			}
		}
		frontier = next
	}
	out := make([]int, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// maxDepth bounds how far back the search looks; it limits cost only.
const maxDepth = 8

// extensions are a call's backward extensions, nearest first: the call, then
// it with its sources one hop back, two hops back, until nothing is added.
func extensions(g []node, j int) [][]int {
	var out [][]int
	prev := -1
	for d := 0; d <= maxDepth; d++ {
		m := layered(g, j, d)
		if len(m) == prev {
			break
		}
		prev = len(m)
		out = append(out, m)
	}
	return out
}

type step struct {
	op      string
	members []int // more than one: a loop over a collection
}

// fold builds the steps of a member set. Consecutive calls of one operation
// are one loop step only when every one of them takes a value from the same
// earlier call (an item of one collection); otherwise each stays its own step.
func fold(g []node, members []int) []step {
	var steps []step
	for _, k := range members {
		op := structOp(g[k])
		if len(steps) > 0 {
			last := &steps[len(steps)-1]
			if last.op == op && shareProducer(g, last.members[0], k) {
				last.members = append(last.members, k)
				continue
			}
		}
		steps = append(steps, step{op: op, members: []int{k}})
	}
	return steps
}

// shareProducer reports two calls that take different items from the same
// earlier call: an iteration. The same item taken twice is a sequence of
// operations on one target, not a loop.
func shareProducer(g []node, a, b int) bool {
	for _, ea := range g[a].parents {
		for _, eb := range g[b].parents {
			if ea.from == eb.from && ea.key == eb.key && argValue(g[a], ea.key) != argValue(g[b], eb.key) {
				return true
			}
		}
	}
	return false
}

func argValue(n node, key string) string {
	for _, a := range n.args {
		if a.key == key {
			return a.value
		}
	}
	return ""
}

// item is the values a call took from earlier results: what distinguishes
// one iteration from another.
func item(n node) string {
	var vs []string
	for _, e := range n.parents {
		vs = append(vs, e.key+"="+argValue(n, e.key))
	}
	sort.Strings(vs)
	return strings.Join(vs, ",")
}

// shape keys a member set by its steps and its value flows between step
// positions (which argument carried a value is a mapping, not identity).
func shape(g []node, members []int) (string, []step, []string) {
	steps := fold(g, members)
	posOf := map[int]int{}
	for p, st := range steps {
		for _, k := range st.members {
			posOf[k] = p
		}
	}
	ops := make([]string, len(steps))
	for i, st := range steps {
		ops[i] = st.op
	}
	set := map[string]bool{}
	for _, k := range members {
		for _, e := range g[k].parents {
			if p, ok := posOf[e.from]; ok && p != posOf[k] {
				set[strconv.Itoa(p+1)+">"+strconv.Itoa(posOf[k]+1)] = true
			}
		}
	}
	edges := make([]string, 0, len(set))
	for e := range set {
		edges = append(edges, e)
	}
	sort.Strings(edges)
	sum := sha256.Sum256([]byte(strings.Join(ops, "\x1f") + "\x1e" + strings.Join(edges, ",")))
	return "pr_" + hex.EncodeToString(sum[:6]), steps, edges
}

func reqKey(si, req int) string { return strconv.Itoa(si) + "/" + strconv.Itoa(req) }

// listIndex finds an item index in a JSON path ("$.pipelines[0].id").
var listIndex = regexp.MustCompile(`\[(\d+)\]`)

type occurrence struct {
	session int
	members []int
}

// condense groups calls into primitives. A call pulls in its supported
// sources for as long as the longer procedure still recurs (in two or more
// requests, in one session or several) and joins the longest one that does.
// A single command is that command's own API: it is a primitive only when it
// loops over a collection.
func condense(ss []trace.Session, graphs [][]node) []Primitive {
	support := map[string]map[string]bool{}
	for si, g := range graphs {
		for j := range g {
			for _, m := range extensions(g, j) {
				id, _, _ := shape(g, m)
				if support[id] == nil {
					support[id] = map[string]bool{}
				}
				support[id][reqKey(si, g[j].request)] = true
			}
		}
	}
	type bindAgg struct {
		b       Binding
		perExec map[string]Observed
	}
	type agg struct {
		ops, edges  []string
		execs       []Execution
		used        map[string]bool // "session/call" already counted
		disjoint    int
		sessions    map[int]bool
		values      map[string]map[string]bool
		given       map[string]Input
		loops       map[int]bool
		variants    map[string]bool
		control     map[string]bool
		unresolved  map[string]bool
		binds       map[string]*bindAgg
		effect      string
		stepEffects []string
		saved       trace.Usage
	}
	by := map[string]*agg{}
	var order []string
	// Each call joins its longest recurring extension. Calls of one
	// operation in the same request that took values from the same earlier
	// calls are one execution: a loop over that collection, not separate runs.
	type pick struct {
		si      int
		sinks   []int
		members []int
		items   map[string]bool
	}
	var picks []*pick
	byPrefix := map[string]*pick{}
	for si, g := range graphs {
		for j := range g {
			var members []int
			var id string
			for _, m := range extensions(g, j) {
				mid, _, _ := shape(g, m)
				if len(support[mid]) < 2 {
					break
				}
				members, id = m, mid
			}
			if members == nil {
				continue
			}
			key := ""
			if len(members) > 1 {
				var pre []string
				for _, k := range members {
					if k != j {
						pre = append(pre, strconv.Itoa(k))
					}
				}
				key = reqKey(si, g[j].request) + "|" + id + "|" + strings.Join(pre, ",") + "|" + g[j].op
			}
			if pk := byPrefix[key]; key != "" && pk != nil && !pk.items[item(g[j])] {
				pk.sinks = append(pk.sinks, j)
				pk.members = append(pk.members, j)
				pk.items[item(g[j])] = true
				continue
			}
			pk := &pick{si: si, sinks: []int{j}, members: append([]int(nil), members...), items: map[string]bool{item(g[j]): true}}
			picks = append(picks, pk)
			if key != "" {
				byPrefix[key] = pk
			}
		}
	}
	for _, pk := range picks {
		si, g := pk.si, graphs[pk.si]
		members := pk.members
		sort.Ints(members)
		id, steps, edges := shape(g, members)
		j := pk.sinks[0]
		for _, k := range pk.sinks {
			g[k].shape = id
		}
		a := by[id]
		if a == nil {
			a = &agg{edges: edges, used: map[string]bool{}, sessions: map[int]bool{}, values: map[string]map[string]bool{},
				given: map[string]Input{}, loops: map[int]bool{}, variants: map[string]bool{}, control: map[string]bool{},
				unresolved: map[string]bool{}, binds: map[string]*bindAgg{}, effect: "read"}
			for _, st := range steps {
				a.ops = append(a.ops, st.op)
			}
			by[id] = a
			order = append(order, id)
		}
		s := ss[si]
		ex := Execution{Client: s.Client, Session: s.ID, Request: g[j].request, MaxGapSeconds: -1}
		posOf := map[int]int{}
		for p, st := range steps {
			for _, k := range st.members {
				posOf[k] = p
			}
		}
		overlap := ""
		for _, k := range members {
			key := strconv.Itoa(si) + "/" + strconv.Itoa(g[k].call)
			if a.used[key] && overlap == "" {
				overlap = key
			}
		}
		for p, st := range steps {
			if len(st.members) > 1 {
				a.loops[p] = true
			}
			for _, k := range st.members {
				n := g[k]
				ref := CallRef{Step: p + 1, Index: n.call, ID: n.c.ID, Op: n.op, OK: n.c.Outcome == trace.OutcomeOK, Tokens: n.c.Tokens.Total()}
				if len(ex.Calls) > 0 {
					ex.SavedTokens += ref.Tokens
					ex.Saved = ex.Saved.Add(n.c.Tokens)
				}
				for len(a.stepEffects) <= p {
					a.stepEffects = append(a.stepEffects, "read")
				}
				if rankEffect(n.effect) > rankEffect(a.stepEffects[p]) {
					a.stepEffects[p] = n.effect
				}
				if !n.c.Time.IsZero() {
					ref.Time = n.c.Time.UTC().Format("2006-01-02T15:04:05Z")
				}
				ex.Calls = append(ex.Calls, ref)
				for _, c := range n.control {
					a.control[fmt.Sprintf("step %d: %s", p+1, c)] = true
				}
				if i := strings.Index(n.op, "#"); i >= 0 {
					a.variants[strconv.Itoa(p+1)+":"+n.op[i+1:]] = true
				}
				switch n.effect {
				case "write":
					a.effect = "write"
				case "unknown":
					if a.effect == "read" {
						a.effect = "unknown"
					}
				}
				for _, ag := range n.args {
					vk := strconv.Itoa(p+1) + ":" + ag.key
					if ag.obs != nil {
						o := *ag.obs
						o.Step = p + 1
						if o.Source == "step" && o.Label != Ambiguous {
							if fp, ok := posOf[o.From]; ok {
								o.From = fp + 1
							} else {
								// The producer lies outside this primitive's
								// boundary: here the value is the caller's.
								o.Source, o.From, o.Reason = "input", 0, o.Reason+"; producer outside this primitive"
							}
						} else {
							o.From = 0
						}
						ex.Observed = append(ex.Observed, o)
						if o.Source == "step" && o.Label == Explicit && len(st.members) == 1 {
							if m := listIndex.FindStringSubmatch(o.Selector); m != nil {
								a.unresolved[fmt.Sprintf("step %d %s: one item selected from step %d's list; selection rule unknown", p+1, ag.key, o.From)] = true
							}
						}
					}
					if ag.given == "step" {
						continue
					}
					if prev, ok := a.given[vk]; !ok || prev.Given == "request" && ag.given == "unknown" {
						a.given[vk] = Input{Step: p + 1, Key: ag.key, Type: ag.typ, Given: ag.given}
					}
					if a.values[vk] == nil {
						a.values[vk] = map[string]bool{}
					}
					if len(a.values[vk]) < 2 {
						a.values[vk][ag.value] = true
					}
				}
			}
		}
		var prevT int64 = -1
		for _, k := range members {
			t := g[k].c.Time
			if t.IsZero() {
				prevT = -1
				ex.MaxGapSeconds = -1
				break
			}
			if prevT >= 0 {
				if gap := int(t.Unix() - prevT); gap > ex.MaxGapSeconds {
					ex.MaxGapSeconds = gap
				}
			} else if ex.MaxGapSeconds < 0 {
				ex.MaxGapSeconds = 0
			}
			prevT = t.Unix()
		}
		sum := sha256.Sum256([]byte(s.Client + "\x00" + s.ID + "\x00" + strconv.Itoa(g[j].call) + "\x00" + id))
		ex.ID = "ex_" + hex.EncodeToString(sum[:5])
		last := pk.sinks[len(pk.sinks)-1]
		if last+1 < len(g) && g[last+1].request == g[last].request && g[last+1].decided {
			ex.ThenDecided = true
		}
		if overlap != "" {
			ex.Overlaps = overlap
		} else {
			a.disjoint++
			a.saved = a.saved.Add(ex.Saved)
			a.sessions[si] = true
			for _, k := range members {
				a.used[strconv.Itoa(si)+"/"+strconv.Itoa(g[k].call)] = true
			}
		}
		a.execs = append(a.execs, ex)
		for _, o := range ex.Observed {
			bk := strconv.Itoa(o.Step) + ":" + o.Arg
			ba := a.binds[bk]
			if ba == nil {
				ba = &bindAgg{b: Binding{Step: o.Step, Arg: o.Arg, Counts: map[string]int{}}, perExec: map[string]Observed{}}
				a.binds[bk] = ba
			}
			ba.b.Counts[o.Source+"/"+o.Label]++
			ba.perExec[ex.ID] = o
		}
	}
	var out []Primitive
	for _, id := range order {
		a := by[id]
		if len(a.ops) == 1 && !a.loops[0] {
			continue
		}
		if a.disjoint < 2 {
			continue
		}
		p := Primitive{ID: id, Steps: a.ops, Effect: a.effect, StepEffects: a.stepEffects, SavedTokens: a.saved.Total(), Saved: a.saved, SessionCount: len(a.sessions), ExecutionCount: a.disjoint, Executions: a.execs}
		for pos := range a.loops {
			p.Loops = append(p.Loops, pos+1)
		}
		sort.Ints(p.Loops)
		for vk, in := range a.given {
			if len(a.values[vk]) == 1 {
				p.Defaults = append(p.Defaults, vk)
				continue
			}
			p.Inputs = append(p.Inputs, in)
		}
		sort.Slice(p.Inputs, func(i, j int) bool {
			if p.Inputs[i].Step != p.Inputs[j].Step {
				return p.Inputs[i].Step < p.Inputs[j].Step
			}
			return p.Inputs[i].Key < p.Inputs[j].Key
		})
		sort.Strings(p.Defaults)
		for v := range a.variants {
			p.Variants = append(p.Variants, v)
		}
		sort.Strings(p.Variants)
		for c := range a.control {
			p.ControlEdges = append(p.ControlEdges, c)
		}
		sort.Strings(p.ControlEdges)
		// Aggregate each binding: the majority observation, its reasons, and
		// the executions that disagree with it.
		var keys []string
		for k := range a.binds {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		edgeSet := map[string]bool{}
		for _, k := range keys {
			ba := a.binds[k]
			// The majority is the whole observation, producing step and
			// field included: a value taken from step 2 in some runs and
			// step 1 in others is a disagreement, not one source.
			full := func(o Observed) string {
				return fmt.Sprintf("%s/%s/%d/%s", o.Source, o.Label, o.From, o.Selector)
			}
			tally := map[string]int{}
			byKey := map[string]Observed{}
			for _, o := range ba.perExec {
				tally[full(o)]++
				byKey[full(o)] = o
			}
			major, n := "", -1
			for c, cnt := range tally {
				if cnt > n || cnt == n && c < major {
					major, n = c, cnt
				}
			}
			m := byKey[major]
			ba.b.Source, ba.b.Label, ba.b.From, ba.b.Selector = m.Source, m.Label, m.From, m.Selector
			reasons := map[string]bool{}
			for exID, o := range ba.perExec {
				if full(o) != major {
					ba.b.Contradicting = append(ba.b.Contradicting, exID)
					continue
				}
				reasons[o.Reason] = true
			}
			for r := range reasons {
				ba.b.Reasons = append(ba.b.Reasons, r)
			}
			sort.Strings(ba.b.Reasons)
			sort.Strings(ba.b.Contradicting)
			if ba.b.Source == "step" && ba.b.From != 0 {
				edgeSet[fmt.Sprintf("%d>%d:%s", ba.b.From, ba.b.Step, ba.b.Arg)] = true
			}
			if ba.b.Label == Ambiguous {
				a.unresolved[fmt.Sprintf("step %d %s: source ambiguous (%s)", ba.b.Step, ba.b.Arg, strings.Join(ba.b.Reasons, "; "))] = true
			}
			if len(ba.b.Contradicting) > 0 {
				a.unresolved[fmt.Sprintf("step %d %s: %d execution(s) disagree with the majority source", ba.b.Step, ba.b.Arg, len(ba.b.Contradicting))] = true
			}
			p.Bindings = append(p.Bindings, ba.b)
		}
		for e := range edgeSet {
			p.Edges = append(p.Edges, e)
		}
		sort.Strings(p.Edges)
		long := 0
		for _, ex := range a.execs {
			if ex.MaxGapSeconds >= 20*60 {
				long++
			}
		}
		if long > 0 {
			a.unresolved[fmt.Sprintf("%d execution(s) have a gap of 20 minutes or more between calls; boundary unverified (proposed cutoff, not validated)", long)] = true
		}
		for u := range a.unresolved {
			p.Unresolved = append(p.Unresolved, u)
		}
		sort.Strings(p.Unresolved)
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (len(out[i].Steps) > 1) != (len(out[j].Steps) > 1) {
			return len(out[i].Steps) > 1
		}
		return out[i].ExecutionCount > out[j].ExecutionCount
	})
	return out
}

// compose runs after every primitive is found: a primitive is composed of
// the primitives its earlier calls belong to, and of any known primitive
// whose steps it contains in order.
func compose(ps []Primitive, graphs [][]node, known []Known) {
	isPrim := map[string]bool{}
	for _, p := range ps {
		isPrim[p.ID] = true
	}
	rep := map[string]occurrence{}
	for si, g := range graphs {
		for j := range g {
			id := g[j].shape
			if _, seen := rep[id]; !isPrim[id] || seen {
				continue
			}
			for _, m := range extensions(g, j) {
				if mid, _, _ := shape(g, m); mid == id {
					rep[id] = occurrence{si, m}
				}
			}
		}
	}
	for i := range ps {
		p := &ps[i]
		if o, ok := rep[p.ID]; ok {
			g := graphs[o.session]
			seen := map[string]bool{}
			for _, k := range o.members[:len(o.members)-1] {
				if id := g[k].shape; isPrim[id] && id != p.ID && !seen[id] {
					seen[id] = true
					p.ComposedOf = append(p.ComposedOf, id)
				}
			}
		}
		for _, k := range known {
			if len(k.Steps) > 0 && len(k.Steps) < len(p.Steps) && subsequence(k.Steps, p.Steps) {
				p.ComposedOf = append(p.ComposedOf, "known:"+k.Name)
			}
		}
	}
}

func subsequence(small, big []string) bool {
	i := 0
	for _, s := range big {
		if i < len(small) && s == small[i] {
			i++
		}
	}
	return i == len(small)
}

func rankEffect(e string) int {
	switch e {
	case "write":
		return 2
	case "unknown":
		return 1
	}
	return 0
}
