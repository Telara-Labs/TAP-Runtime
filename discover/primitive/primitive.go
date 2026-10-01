// Package primitive condenses agent history into proposed primitives.
//
// A primitive is a dataflow unit: a call and every earlier call whose result
// supplied one of its values. A value with a known source pulls that source
// in, so the primitive grows backward; a value with no known source is an
// input, and the primitive starts there. Nothing blocks on provenance.
//
// Operations are route-agnostic: a dispatching tool (a gateway, a plugin,
// nested tool calls) that selects an operation is the same operation as a
// direct tool the corpus shows for it. Nothing here is decided by a tool's,
// provider's or argument's name: identity, choices and settings come from the
// recorded calls alone.
package primitive

import (
	"encoding/json"
	"sort"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Primitive is one proposed task, condensed: counts are fields.
type Primitive struct {
	ID    string   `json:"id"`
	Steps []string `json:"steps"`
	// Edges are value flows between steps that the evidence supports
	// ("1>2:issue_key": step 2's issue_key came from step 1's result).
	Edges []string `json:"edges,omitempty"`
	// Bindings say, for every argument of every step, where its value came
	// from and how well the recorded executions support that.
	Bindings []Binding `json:"bindings,omitempty"`
	// ControlEdges are success dependencies inside a step (a && b): the
	// later command runs only when the earlier one succeeded.
	ControlEdges []string `json:"controlEdges,omitempty"`
	// Inputs are values the caller supplies: values with no source in an
	// earlier step of the same execution.
	Inputs []Input `json:"inputs,omitempty"`
	// Defaults are arguments whose value never changed across executions
	// ("step:key"): historical constants, not yet declared invariants.
	Defaults []string `json:"defaults,omitempty"`
	// Loops are step positions that repeat over a collection in one
	// execution: each repetition takes an item from the same earlier result.
	Loops []int `json:"loops,omitempty"`
	// Effect is read, write or unknown (unknown is treated as write).
	Effect string `json:"effect"`
	// StepEffects are each step's effect.
	StepEffects []string `json:"stepEffects"`
	// SavedTokens totals the model-turn tokens the counted runs spent after
	// their first call; Saved splits them into fresh input, cached input
	// (context re-read from the prompt cache) and output.
	SavedTokens float64     `json:"savedTokens"`
	Saved       trace.Usage `json:"saved"`
	// Variants are choices seen at a step ("step:key=value"): values that
	// change what follows.
	Variants []string `json:"variants,omitempty"`
	// Unresolved lists what the evidence does not settle: an ambiguous
	// source, a selection from a list with no known rule, a long gap.
	Unresolved []string `json:"unresolved,omitempty"`
	// ComposedOf names smaller primitives this one contains.
	ComposedOf   []string `json:"composedOf,omitempty"`
	SessionCount int      `json:"sessionCount"`
	// ExecutionCount counts disjoint executions: no call is counted twice.
	ExecutionCount int `json:"executionCount"`
	// Executions index every supporting execution with exact locators.
	Executions []Execution `json:"executions"`
	// Confidence scores how well the evidence supports this flow.
	Confidence Confidence `json:"confidence"`
}

// Binding is one argument's source, aggregated over the executions.
type Binding struct {
	Step int    `json:"step"`
	Arg  string `json:"arg"`
	// Source is "step" (an earlier step's result), "input" (the caller) or
	// "control".
	Source string `json:"source"`
	From   int    `json:"from,omitempty"`
	// Selector is how the value is taken from the producer's result: a JSON
	// path, "output line", or "first field of an output line".
	Selector string `json:"selector,omitempty"`
	// Label is the majority evidence level: explicit, inferred, ambiguous
	// or missing.
	Label   string         `json:"label"`
	Reasons []string       `json:"reasons,omitempty"`
	Counts  map[string]int `json:"counts"`
	// Contradicting are executions whose observation disagrees with the
	// majority (another source or evidence level).
	Contradicting []string `json:"contradicting,omitempty"`
}

// Execution is one supporting run of a primitive.
type Execution struct {
	ID      string    `json:"id"`
	Client  string    `json:"client"`
	Session string    `json:"session"`
	Request int       `json:"request"`
	Calls   []CallRef `json:"calls"`
	// Observed are this run's argument sources.
	Observed []Observed `json:"observed,omitempty"`
	// SavedTokens are the model-turn tokens of every call after the first:
	// the turns a primitive running this chain would remove. Saved splits
	// them into fresh input, cached input and output.
	SavedTokens float64     `json:"savedTokens"`
	Saved       trace.Usage `json:"saved"`
	// MaxGapSeconds is the longest start-to-start time between two of its
	// calls (a command's own running time is included; the client does
	// not record when a call ended). -1 when times were not recorded.
	MaxGapSeconds int `json:"maxGapSeconds"`
	// Overlaps names an earlier counted execution sharing a call with this
	// one; such a run is indexed but not counted again.
	Overlaps string `json:"overlaps,omitempty"`
}

// CallRef locates one call in its session.
type CallRef struct {
	Step  int    `json:"step"`
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	Time  string `json:"time,omitempty"`
	Op    string `json:"op"`
	// OK is true when the client recorded the call as succeeding; false
	// when it recorded nothing (failed calls are never steps).
	OK bool `json:"ok"`
	// Tokens is the call's share of the model turn that issued it.
	Tokens float64 `json:"tokens"`
}

// Observed is one argument's source in one execution.
type Observed struct {
	Step     int    `json:"step"`
	Arg      string `json:"arg"`
	Source   string `json:"source"`
	From     int    `json:"from,omitempty"`
	Selector string `json:"selector,omitempty"`
	Label    string `json:"label"`
	Reason   string `json:"reason"`
}

// Input is one value the caller supplies.
type Input struct {
	Step int    `json:"step"`
	Key  string `json:"key"`
	Type string `json:"type"`
	// Given is "request" when the user's text held the value, "unknown"
	// when no source was found (chosen by the agent, or from outside).
	Given string `json:"given"`
}

// Known is a primitive that exists already (accepted locally, or fetched from
// Telara): discovered primitives that contain its steps are composed of it.
type Known struct {
	Name  string   `json:"name"`
	Steps []string `json:"steps"`
}

// Summary describes what was read and found.
type Summary struct {
	Sessions        int `json:"sessions"`
	ToolCalls       int `json:"toolCalls"`
	Operations      int `json:"operations"`
	DistinctOps     int `json:"distinctOperations"`
	JudgmentCalls   int `json:"judgmentCalls"`
	FailedCalls     int `json:"failedCalls"`
	Primitives      int `json:"primitives"`
	MultiStep       int `json:"multiStep"`
	Composed        int `json:"composed"`
	RouteMerged     int `json:"routeMergedCalls"`
	SettingsAsInput int `json:"settingsAsInputs"`
	// DecisionCalls are calls whose arguments the agent built from an
	// earlier output: each starts a new chain.
	DecisionCalls int `json:"decisionCalls"`
	// Fragments are primitives dropped because every run of theirs is part
	// of a run of a longer primitive.
	Fragments int `json:"fragments"`
	Families  int `json:"families"`
	// Tokens are every turn's tokens in the history read.
	Tokens trace.Usage `json:"tokens"`
}

// Result is the condensed discovery.
type Result struct {
	Summary Summary `json:"summary"`
	// Families are the proposed procedures, ranked by the model-turn tokens
	// their follow-up calls cost; Primitives are the exact chains they group.
	Families   []Family    `json:"families"`
	Primitives []Primitive `json:"primitives"`
}

// node is one replayable call.
type node struct {
	call    int
	request int
	base    string
	op      string
	args    []arg
	effect  string
	parents []edge
	c       trace.Call // the recorded call and its result
	control []string   // && dependencies inside the call
	decided bool       // its arguments were constructed from an earlier output
	shape   string     // the primitive this call was grouped into
}

type arg struct {
	key, typ, value string
	given           string // "step", "request", "unknown"
	obs             *Observed
}

// edge is a supported value flow from an earlier node's result.
type edge struct {
	from     int
	key      string
	label    string
	selector string
}

// Discover condenses sessions into primitives.
func Discover(ss []trace.Session, known []Known) Result {
	cp := append([]trace.Session(nil), ss...)
	for i := range cp {
		cp[i].Calls = append([]trace.Call(nil), cp[i].Calls...)
	}
	trace.DropCopiedCalls(cp)
	norm := trace.Normalize(cp)
	byKey := map[string]*trace.NormSession{}
	for i := range norm {
		byKey[norm[i].Client+"\x00"+norm[i].ID] = &norm[i]
	}
	var res Result
	res.Summary.Sessions = len(cp)
	direct := directTools(cp)

	graphs := make([][]node, len(cp))
	for si := range cp {
		s := &cp[si]
		res.Summary.ToolCalls += len(s.Calls)
		for _, c := range s.Calls {
			res.Summary.Tokens = res.Summary.Tokens.Add(c.Tokens)
		}
		ns := byKey[s.Client+"\x00"+s.ID]
		if ns == nil {
			continue
		}
		byCall := map[int][]trace.Step{}
		for _, st := range ns.Steps {
			byCall[st.Call] = append(byCall[st.Call], st)
		}
		var nodes []node
		for ci, c := range s.Calls {
			steps := byCall[ci]
			if len(steps) == 0 {
				continue
			}
			if c.Outcome == trace.OutcomeFailed {
				res.Summary.FailedCalls++
				continue
			}
			if judgment(c, steps) {
				res.Summary.JudgmentCalls++
				continue
			}
			if !trace.Replayable(steps[0].Label) {
				continue
			}
			n, merged := buildNode(c, ci, steps, direct)
			if merged {
				res.Summary.RouteMerged++
			}
			nodes = append(nodes, n)
		}
		link(nodes, s.Requests)
		res.Summary.DecisionCalls += decide(nodes)
		graphs[si] = nodes
		res.Summary.Operations += len(nodes)
	}
	res.Summary.SettingsAsInput = assignChoices(graphs)
	ops := map[string]bool{}
	for _, g := range graphs {
		for _, n := range g {
			ops[n.op] = true
		}
	}
	res.Summary.DistinctOps = len(ops)
	res.Primitives = condense(cp, graphs)
	compose(res.Primitives, graphs, known)
	for i := range res.Primitives {
		res.Primitives[i].Confidence = score(res.Primitives[i])
	}
	res.Primitives, res.Summary.Fragments = dropFragments(res.Primitives)
	res.Families = families(res.Primitives)
	res.Summary.Families = len(res.Families)
	for _, p := range res.Primitives {
		if len(p.Steps) > 1 {
			res.Summary.MultiStep++
		}
		if len(p.ComposedOf) > 0 {
			res.Summary.Composed++
		}
	}
	res.Summary.Primitives = len(res.Primitives)
	return res
}

// judgment reports a call whose content the agent decided for this run: an
// edit, or a multi-line script handed to a program that runs code.
func judgment(c trace.Call, steps []trace.Step) bool {
	for _, st := range steps {
		if trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:") {
			return true
		}
	}
	if c.Tool == "shell" && strings.Contains(c.Command, "\n") {
		f := strings.Fields(c.Command)
		return len(f) > 0 && shellparse.ProgramCommandRunsCode(f[0], f[1:])
	}
	return false
}

// directTools maps each non-dispatching tool to the argument keys the corpus
// shows it taking.
func directTools(ss []trace.Session) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, s := range ss {
		for _, c := range s.Calls {
			if !strings.HasPrefix(c.Tool, "mcp:") {
				continue
			}
			if obj, _ := dispatchParts(c); obj != "" {
				continue
			}
			if out[c.Tool] == nil {
				out[c.Tool] = map[string]bool{}
			}
			for k := range c.Args {
				out[c.Tool][k] = true
			}
		}
	}
	return out
}

// dispatchParts recognizes a dispatching call by structure: an argument that
// is a JSON object (the operation's own arguments) beside plain-word
// arguments (what selects the operation). It returns the object's key and
// the selecting arguments, or "" when the call is not a dispatch.
func dispatchParts(c trace.Call) (string, map[string]string) {
	obj := ""
	sel := map[string]string{}
	for k, v := range c.Args {
		t := strings.TrimSpace(v)
		if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
			if obj != "" {
				return "", nil // two objects: not a single dispatch
			}
			obj = k
			continue
		}
		if trace.PlainChoiceValue(v) {
			sel[k] = v
		}
	}
	if obj == "" || len(sel) == 0 {
		return "", nil
	}
	return obj, sel
}

func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// resolve finds the direct tool a dispatch selects: one whose name holds
// every word of the selecting values and whose arguments overlap the
// dispatched ones. Otherwise the operation is named by the selecting values,
// so the same dispatch through any route is the same operation.
func resolve(sel map[string]string, inner map[string]bool, direct map[string]map[string]bool) (string, bool) {
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var want []string
	var vals []string
	for _, k := range keys {
		want = append(want, tokens(sel[k])...)
		vals = append(vals, strings.ToLower(sel[k]))
	}
	best, bestExtra, tie := "", -1, false
	for tool, args := range direct {
		have := map[string]bool{}
		for _, t := range tokens(strings.TrimPrefix(tool, "mcp:")) {
			have[t] = true
		}
		ok := len(want) > 0
		for _, t := range want {
			ok = ok && have[t]
		}
		if !ok {
			continue
		}
		overlap := len(inner) == 0 && len(args) == 0
		for k := range inner {
			overlap = overlap || args[k]
		}
		if !overlap {
			continue
		}
		extra := len(have) - len(want)
		switch {
		case best == "" || extra < bestExtra:
			best, bestExtra, tie = tool, extra, false
		case extra == bestExtra:
			tie = true
		}
	}
	if best != "" && !tie {
		return best, true
	}
	// No tool is named by every selecting word: match by shape instead. A
	// direct tool that takes every dispatched argument (keys compared without
	// case or separators) and shares a selecting word is the same operation,
	// when exactly one tool fits.
	if len(inner) > 0 {
		match, n := "", 0
		for tool, args := range direct {
			have := map[string]bool{}
			for k := range args {
				have[normKey(k)] = true
			}
			fits := true
			for k := range inner {
				fits = fits && have[normKey(k)]
			}
			if !fits {
				continue
			}
			shared := false
			name := map[string]bool{}
			for _, t := range tokens(strings.TrimPrefix(tool, "mcp:")) {
				name[t] = true
			}
			for _, t := range want {
				shared = shared || name[t]
			}
			if shared {
				match, n = tool, n+1
			}
		}
		if n == 1 {
			return match, true
		}
	}
	return "op:" + strings.Join(vals, "."), false
}

// normKey compares argument names without case or separators
// (maxResults, max_results).
func normKey(k string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' {
			return -1
		}
		return r
	}, strings.ToLower(k))
}

func buildNode(c trace.Call, ci int, steps []trace.Step, direct map[string]map[string]bool) (node, bool) {
	n := node{call: ci, request: c.Request, effect: "read"}
	n.c = c
	if c.Tool == "shell" {
		if plan, err := shellparse.ProgramShellPlan(c.Command); err == nil {
			for i := 1; i < len(plan); i++ {
				if plan[i].Connector == "and" && len(plan[i].Words) > 0 && len(plan[i-1].Words) > 0 {
					n.control = append(n.control, plan[i-1].Words[0]+" succeeds -> "+plan[i].Words[0])
				}
			}
		}
	}
	for _, st := range steps {
		switch trace.StepEffect(st) {
		case "write":
			n.effect = "write"
		case "unknown":
			if n.effect == "read" {
				n.effect = "unknown"
			}
		}
	}
	merged := false
	if c.Tool == "shell" {
		var labels []string
		// Every flag and argument, authority included, is an argument of
		// the command's API: the operation is the program and subcommand.
		for _, st := range steps {
			labels = append(labels, st.Label)
			for _, sl := range st.Slots {
				if sl.Sub || trace.Derived(sl.Key) {
					continue
				}
				n.args = append(n.args, arg{key: sl.Key, typ: sl.Type, value: sl.Value})
			}
		}
		n.base = strings.Join(labels, "+")
		return n, false
	}
	fields := trace.ObservedArgs(c)
	if obj, sel := dispatchParts(c); obj != "" {
		inner := map[string]bool{}
		for path := range fields {
			if rest, ok := strings.CutPrefix(path, obj+"/"); ok {
				inner[strings.SplitN(rest, "/", 2)[0]] = true
			}
		}
		n.base, merged = resolve(sel, inner, direct)
		for path, f := range fields {
			if rest, ok := strings.CutPrefix(path, obj+"/"); ok {
				n.args = append(n.args, arg{key: rest, typ: valueType(f.Value), value: f.Value})
			} else if _, isSel := sel[path]; !isSel {
				n.args = append(n.args, arg{key: path, typ: valueType(f.Value), value: f.Value})
			}
		}
	} else {
		n.base = steps[0].Label
		for path, f := range fields {
			n.args = append(n.args, arg{key: path, typ: valueType(f.Value), value: f.Value})
		}
	}
	sort.Slice(n.args, func(i, j int) bool { return n.args[i].key < n.args[j].key })
	return n, merged
}

func valueType(v string) string {
	return trace.TypeOf(shellparse.Word{Text: v, Quoted: strings.ContainsAny(v, " \n")})
}

// assignChoices decides, for every plain-word argument of an operation,
// whether its values choose the operation or are settings of it. Values are
// settings when every value is seen followed by the same operations: the
// procedure does not change with them. A value that changes what follows is
// a choice and becomes part of the operation. It returns how many arguments
// were settings.
func assignChoices(graphs [][]node) int {
	type key struct{ base, arg string }
	sigs := map[key]map[string]map[string]bool{} // value -> signatures
	for _, g := range graphs {
		children := make([][]string, len(g))
		for _, n := range g {
			for _, e := range n.parents {
				children[e.from] = append(children[e.from], n.base)
			}
		}
		for j, n := range g {
			kids := append([]string(nil), children[j]...)
			sort.Strings(kids)
			for _, a := range n.args {
				if a.typ != trace.SlotWord || !trace.PlainChoiceValue(a.value) {
					continue
				}
				sig := strings.Join(kids, ",")
				k := key{n.base, a.key}
				if sigs[k] == nil {
					sigs[k] = map[string]map[string]bool{}
				}
				if sigs[k][a.value] == nil {
					sigs[k][a.value] = map[string]bool{}
				}
				sigs[k][a.value][sig] = true
			}
		}
	}
	choice := map[key]bool{}
	settings := 0
	for k, vals := range sigs {
		if len(vals) < 2 {
			continue // one value: fixed, not a choice between operations
		}
		var common map[string]bool
		for _, ss := range vals {
			if common == nil {
				common = map[string]bool{}
				for s := range ss {
					common[s] = true
				}
				continue
			}
			for s := range common {
				if !ss[s] {
					delete(common, s)
				}
			}
		}
		if len(common) == 0 {
			choice[k] = true
		} else {
			settings++
		}
	}
	for gi := range graphs {
		for j := range graphs[gi] {
			n := &graphs[gi][j]
			n.op = n.base
			var cs []string
			for _, a := range n.args {
				if a.typ == trace.SlotWord && choice[key{n.base, a.key}] {
					cs = append(cs, a.key+"="+a.value)
				}
			}
			sort.Strings(cs)
			for _, c := range cs {
				n.op += "#" + c
			}
		}
	}
	return settings
}
