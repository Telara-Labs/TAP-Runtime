package discover

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"
	"gitlab.com/telara-labs/tap-runtime/discover/genreview"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// primitiveInstaller generates an accepted family's program from its
// recorded runs and installs it privately for the person's agent. A family
// with several continuations needs an executable causal contract; silently
// installing its most-used chain would change the reviewed contract.
func primitiveInstaller(sessions []trace.Session, client, home, cwd string) func(primitive.Family, []primitive.Primitive) (primitive.InstallResult, error) {
	// Discover dropped calls copied between session files before indexing
	// them; the generator must see the same call positions.
	cp := make([]trace.Session, len(sessions))
	for i, s := range sessions {
		s.Calls = append([]trace.Call(nil), s.Calls...)
		cp[i] = s
	}
	trace.DropCopiedCalls(cp)
	by := map[string]*trace.Session{}
	for i := range cp {
		by[cp[i].Client+"\x00"+cp[i].ID] = &cp[i]
	}
	return func(f primitive.Family, members []primitive.Primitive) (primitive.InstallResult, error) {
		var r primitive.InstallResult
		if len(f.FollowUps) > 1 {
			// The continuations left out are named in the plan's reason,
			// shown before the person accepts.
			keep, kept, _ := executableFamily(f, members, by)
			if len(keep.FollowUps) == 0 {
				r.Reason = "the recorded uses do not establish an executable causal bundle: none of its continuations compiles as recorded; prepare a handoff to refine it"
				return r, nil
			}
			f, members = keep, kept
		}
		if len(f.FollowUps) > 1 || (f.APIMode == "optional_followups" && len(f.FollowUps) > 0) {
			g, why := bundleGraph(f, members, by)
			if g == nil {
				r.Reason = "the recorded uses show multiple continuations, but do not establish an executable causal bundle: " + why
				return r, nil
			}
			pkg, err := codegen.GenerateProgramPackage(g)
			if err != nil {
				r.Reason = "the optional-follow-up program could not be generated: " + err.Error()
				return r, nil
			}
			return install(r, pkg, client, home, cwd)
		}
		if len(members) == 0 {
			r.Reason = "no recorded chain"
			return r, nil
		}
		main := members[0]
		for _, m := range members[1:] {
			if m.ExecutionCount > main.ExecutionCount {
				main = m
			}
		}
		// Tool-call chains are built straight from the bindings discovery
		// recorded; anything else is re-derived by the older synthesizer.
		if g, why := directGraph(main, by); g != nil {
			pkg, err := codegen.GenerateProgramPackage(g)
			if err != nil {
				r.Reason = "the package could not be generated: " + err.Error()
				return r, nil
			}
			return install(r, pkg, client, home, cwd)
		} else if why != "" && !strings.HasPrefix(why, "fallback:") {
			r.Reason = why
			return r, nil
		}
		var proposals []model.SpanProposal
		cand := model.LogicCandidate{ID: "lc_" + packageSlug(main), Actions: main.Steps, Executions: main.ExecutionCount, Sessions: main.SessionCount}
		for n, ex := range main.Executions {
			if ex.Overlaps != "" {
				continue
			}
			s := by[ex.Client+"\x00"+ex.Session]
			if s == nil {
				continue
			}
			p := model.SpanProposal{ID: fmt.Sprintf("%s-%d", main.ID, n), Client: ex.Client, Session: ex.Session, Request: ex.Request}
			ok := true
			for _, c := range ex.Calls {
				if c.Index >= len(s.Calls) {
					ok = false
					break
				}
				ordinal := 0
				for i := 0; i <= c.Index; i++ {
					if s.Calls[i].Request == ex.Request {
						ordinal++
					}
				}
				p.Calls = append(p.Calls, ordinal)
				p.CallHashes = append(p.CallHashes, retrieval.SpanCallHash(s.Calls[c.Index]))
			}
			if ok {
				proposals = append(proposals, p)
				cand.Members = append(cand.Members, p.ID)
			}
		}
		if len(proposals) == 0 {
			r.Reason = "its recorded uses are not available any more"
			return r, nil
		}
		g, err := codegen.SynthesizeProgramGraph(cand, proposals, cp)
		if err != nil {
			r.Reason = "the program could not be built from the recorded uses: " + err.Error()
			return r, nil
		}
		if len(g.Problems) > 0 {
			r.Reason = "the recorded uses leave this undetermined: " + g.Problems[0]
			if len(g.Problems) > 1 {
				r.Reason += " (and " + strconv.Itoa(len(g.Problems)-1) + " more)"
			}
			return r, nil
		}
		pkg, err := codegen.GenerateProgramPackage(g)
		if err != nil {
			r.Reason = "the package could not be generated: " + err.Error()
			return r, nil
		}
		return install(r, pkg, client, home, cwd)
	}
}

func install(r primitive.InstallResult, pkg *codegen.GeneratedPackage, client, home, cwd string) (primitive.InstallResult, error) {
	root, err := pack.SkillsDir(client, false, home, cwd)
	if err != nil {
		return r, err
	}
	where, _, err := genreview.SaveGeneratedPackage(pkg, root)
	if err != nil {
		return r, err
	}
	r.Installed, r.Name, r.Where = true, pkg.Manifest.Metadata.Name, where
	return r, nil
}

// directGraph builds the program from a chain's recorded bindings: every
// argument is a caller input, a fixed value (the same in every use), or a
// structured field of an earlier step's result. A step that repeated over
// several items of one earlier result acts on the items the caller picks by
// position (never on every item: the recorded uses took a subset). Uses that
// reached a step through a different tool route than most are left out. It
// covers tool-call chains; for others it returns a "fallback:" reason so the
// older synthesizer is tried. A binding the uses do not determine (taken
// from an output line rather than a field) is returned as the reason.
func directGraph(p primitive.Primitive, by map[string]*trace.Session) (*codegen.ProgramGraph, string) {
	return directGraphVia(p, by, "")
}

// directGraphVia is directGraph with the first step's tool route fixed
// ("server/tool"), so continuations of one family share one first step.
func directGraphVia(p primitive.Primitive, by map[string]*trace.Session, headRoute string) (*codegen.ProgramGraph, string) {
	for _, st := range p.Steps {
		if !(strings.HasPrefix(st, "mcp:") || strings.HasPrefix(st, "op:")) || strings.Contains(st, "#") {
			return nil, "fallback: not a tool-call chain"
		}
	}
	loop := map[int]bool{}
	for _, l := range p.Loops {
		loop[l] = true
	}
	// A value most runs took from an earlier step's result is that result;
	// the runs that took it elsewhere did something else and are left out.
	bind := map[string]primitive.Binding{}
	exclude := map[string]bool{}
	runs := 0
	for _, ex := range p.Executions {
		if ex.Overlaps == "" {
			runs++
		}
	}
	for _, b := range p.Bindings {
		if b.Source == "step" && b.Label == primitive.Explicit && b.From > 0 && len(b.Contradicting) > 0 {
			against := 0
			for _, ex := range p.Executions {
				if ex.Overlaps == "" && contains(b.Contradicting, ex.ID) {
					against++
				}
			}
			if 2*(runs-against) > runs {
				for _, id := range b.Contradicting {
					exclude[id] = true
				}
				b.Contradicting = nil
			}
		}
		bind[strconv.Itoa(b.Step)+"|"+b.Arg] = b
	}
	// Each use's calls by step; the most common tool route per step.
	type use map[int][]trace.Call
	var uses []use
	routes := map[int]map[string]int{}
	for _, ex := range p.Executions {
		if ex.Overlaps != "" || exclude[ex.ID] {
			continue
		}
		s := by[ex.Client+"\x00"+ex.Session]
		if s == nil {
			continue
		}
		u := use{}
		for _, c := range ex.Calls {
			if c.Index >= len(s.Calls) || s.Calls[c.Index].MCPTool == "" {
				u = nil
				break
			}
			call := s.Calls[c.Index]
			u[c.Step] = append(u[c.Step], call)
			if routes[c.Step] == nil {
				routes[c.Step] = map[string]int{}
			}
			routes[c.Step][call.MCPServer+"/"+call.MCPTool]++
		}
		if len(u) == len(p.Steps) {
			uses = append(uses, u)
		}
	}
	route := map[int]string{}
	for st, m := range routes {
		best, n := "", -1
		for r, c := range m {
			if c > n || c == n && r < best {
				best, n = r, c
			}
		}
		route[st] = best
	}
	if headRoute != "" && routes[1][headRoute] > 0 {
		route[1] = headRoute
	}
	var kept []use
	for _, u := range uses {
		ok := true
		for st, calls := range u {
			for _, c := range calls {
				ok = ok && c.MCPServer+"/"+c.MCPTool == route[st]
			}
		}
		if ok {
			kept = append(kept, u)
		}
	}
	if len(kept) == 0 {
		return nil, "fallback: no complete uses"
	}
	g := &codegen.ProgramGraph{CandidateID: "lc_" + packageSlug(p), Executions: len(kept), Sessions: p.SessionCount}
	inputName := func(step int, path string) string {
		return fmt.Sprintf("step_%d_%s", step, strings.NewReplacer("/", "_", "-", "_").Replace(path))
	}
	for i, op := range p.Steps {
		n := i + 1
		server, tool, _ := strings.Cut(route[n], "/")
		effect := "write"
		if i < len(p.StepEffects) && p.StepEffects[i] == "read" {
			effect = "read"
		}
		step := codegen.ProgramStep{Role: op, Tool: kept[0][n][0].Tool, Binding: &codegen.ProgramToolBinding{Server: server, Tool: tool}, Effect: effect}
		values := map[string][]string{}
		fields := map[string]trace.ObservedField{}
		present := map[string]int{}
		inCall := map[string]map[int]bool{} // path -> calls that passed it
		calls := 0
		for _, u := range kept {
			for _, c := range u[n] {
				calls++
				for path, f := range trace.ObservedArgs(c) {
					values[path] = append(values[path], f.Value)
					fields[path] = f
					present[path]++
					if inCall[path] == nil {
						inCall[path] = map[int]bool{}
					}
					inCall[path][calls] = true
				}
			}
		}
		var paths []string
		for path := range values {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		// Arguments that choose a dispatched call's operation stay fixed:
		// they are what the step is, not a value it carries.
		chooses := map[string]bool{}
		for _, u := range kept {
			for _, c := range u[n] {
				for k := range primitive.DispatchSelectors(c) {
					chooses[k] = true
				}
			}
		}
		var optional []string
		loopFrom, loopList := 0, ""
		for _, path := range paths {
			f := fields[path]
			arg := codegen.ProgramArg{Path: f.Path, JSONString: f.JsonString, Optional: present[path] < calls}
			b, bound := bind[strconv.Itoa(n)+"|"+path]
			if _, rest, nested := strings.Cut(path, "/"); !bound && nested {
				// Discovery names a dispatched call's arguments without the
				// object that carries them (params/issue_key is issue_key).
				b, bound = bind[strconv.Itoa(n)+"|"+rest]
			}
			same := true
			for _, v := range values[path] {
				same = same && v == values[path][0]
			}
			switch {
			case bound && b.Source == "step" && b.Label == primitive.Explicit && b.From > 0 && loop[n]:
				m := listItem.FindStringSubmatch(b.Selector)
				if m == nil {
					return nil, fmt.Sprintf("step %d repeats, but %s is not taken from a list in step %d's result", n, path, b.From)
				}
				// One set of picked positions per repeated step: every
				// argument taken from the same list item shares it.
				if step.Loop == "" {
					step.Loop, step.DistinctLoopSelections = inputName(n, "positions"), true
					loopFrom, loopList = b.From, m[1]
					g.Inputs = append(g.Inputs, codegen.ProgramInput{Name: step.Loop, Type: "integer", List: true,
						Source: fmt.Sprintf("caller picks positions in step %d's result%s", b.From, m[1])})
				} else if b.From != loopFrom || m[1] != loopList {
					return nil, fmt.Sprintf("step %d repeats over two different lists", n)
				}
				arg.Value = codegen.ProgramValue{Kind: "collection_index_item", Step: b.From, CollectionPath: m[1], ResultPath: m[2], Input: step.Loop}
			case bound && b.Source == "step" && b.Label == primitive.Explicit && b.From > 0 && len(b.Contradicting) == 0:
				arg.Value = codegen.ProgramValue{Kind: "result", Step: b.From, ResultPath: b.Selector}
			case bound && b.Source == "step" && b.Label != primitive.Ambiguous && len(b.Contradicting) == 0:
				return nil, fmt.Sprintf("step %d %s comes from step %d's output by %s; generating a parser for that is not supported yet", n, path, b.From, b.Selector)
			case same && !arg.Optional && (chooses[path] || values[path][0] == "" || codegen.ProgramJSONType(f.TypeName) != "string"):
				arg.Value = codegen.ProgramValue{Kind: "selector", Selector: values[path][0]}
			case same && !arg.Optional:
				// Every recorded use passed the same value, but nothing says
				// where it came from: the caller may change it, and the
				// recorded value is sent when they do not.
				name := inputName(n, path)
				arg.Value = codegen.ProgramValue{Kind: "input", Input: name}
				g.Inputs = append(g.Inputs, codegen.ProgramInput{Name: name, Type: f.TypeName, Default: values[path][0], Source: "supplied at invocation; every recorded use passed the default"})
			default:
				name := inputName(n, path)
				arg.Value = codegen.ProgramValue{Kind: "input", Input: name}
				g.Inputs = append(g.Inputs, codegen.ProgramInput{Name: name, Type: f.TypeName, Optional: arg.Optional, Source: "supplied at invocation"})
				if arg.Optional {
					optional = append(optional, name)
				}
			}
			step.Args = append(step.Args, arg)
		}
		step.Args = mergeArgNames(step.Args, inCall, calls)
		// An optional argument carrying exactly what a required one carries
		// (the same earlier result) is an older name for the same value:
		// every recorded use worked without it, so it is left out.
		var args []codegen.ProgramArg
		for _, a := range step.Args {
			dup := false
			if a.Optional && a.Value.Kind != "input" && a.Value.Kind != "selector" {
				for _, b := range step.Args {
					if !b.Optional && b.Value == a.Value {
						dup = true
					}
				}
			}
			if !dup {
				args = append(args, a)
			}
		}
		step.Args = args
		if len(optional) > 0 {
			seen := map[string]bool{}
			for _, u := range kept {
				for _, c := range u[n] {
					combo := []string{} // never null in the generated program
					obs := trace.ObservedArgs(c)
					for _, path := range paths {
						if _, ok := obs[path]; ok && contains(optional, inputName(n, path)) {
							combo = append(combo, inputName(n, path))
						}
					}
					if k := strings.Join(combo, ","); !seen[k] {
						seen[k] = true
						step.OptionalProfiles = append(step.OptionalProfiles, combo)
					}
				}
			}
		}
		g.Steps = append(g.Steps, step)
	}
	return g, ""
}

// mergeArgNames finds one value sent under several argument names: optional
// arguments taken from the same earlier result, never two in one call, and
// together present in every call. Each recorded call succeeded with the name
// it used, so the program sends the one most calls used, always.
func mergeArgNames(args []codegen.ProgramArg, inCall map[string]map[int]bool, calls int) []codegen.ProgramArg {
	groups := map[codegen.ProgramValue][]int{}
	for i, a := range args {
		if a.Optional && a.Value.Kind == "result" {
			groups[a.Value] = append(groups[a.Value], i)
		}
	}
	drop := map[int]bool{}
	for _, idx := range groups {
		if len(idx) < 2 {
			continue
		}
		seen := map[int]bool{}
		disjoint := true
		for _, i := range idx {
			for c := range inCall[strings.Join(args[i].Path, "/")] {
				disjoint = disjoint && !seen[c]
				seen[c] = true
			}
		}
		if !disjoint || len(seen) != calls {
			continue
		}
		best := idx[0]
		for _, i := range idx[1:] {
			a, b := strings.Join(args[i].Path, "/"), strings.Join(args[best].Path, "/")
			if len(inCall[a]) > len(inCall[b]) || len(inCall[a]) == len(inCall[b]) && a < b {
				best = i
			}
		}
		for _, i := range idx {
			drop[i] = i != best
		}
		args[best].Optional = false
	}
	var out []codegen.ProgramArg
	for i, a := range args {
		if !drop[i] {
			out = append(out, a)
		}
	}
	return out
}

// listItem splits a path to one list item into the list and the item's
// field ("$.issues[2].key" into ".issues" and ".key").
var listItem = regexp.MustCompile(`^(.*)\[\d+\](.*)$`)

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// stepKeys are steps by command, as follow-ups are named.
func stepKeys(steps []string) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		if strings.HasPrefix(s, "sh:") {
			s = strings.Split(s, "+")[0]
		}
		out[i] = s
	}
	return out
}

// packageSlug names a generated package after what it does (its steps'
// tools), with the chain's ID tail so two chains never share a name:
// discovered-jira-search-issues-add-comment-cae7a0.
func packageSlug(p primitive.Primitive) string {
	var words []string
	seen := map[string]bool{}
	for _, st := range p.Steps {
		name := strings.TrimPrefix(strings.TrimPrefix(st, "mcp:"), "op:")
		name = strings.TrimPrefix(strings.Split(name, "+")[0], "sh:")
		for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			if !seen[w] {
				seen[w] = true
				words = append(words, w)
			}
		}
	}
	slug := strings.Join(words, "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	id := strings.TrimPrefix(p.ID, "pr_")
	if len(id) > 6 {
		id = id[:6]
	}
	if slug == "" {
		return id
	}
	return slug + "-" + id
}

// headRoute is the tool route ("server/tool") most of a family's recorded
// first calls took, across all its continuations.
func headRoute(members []primitive.Primitive, by map[string]*trace.Session) string {
	n := map[string]int{}
	for _, p := range members {
		for _, ex := range p.Executions {
			s := by[ex.Client+"\x00"+ex.Session]
			if s == nil || ex.Overlaps != "" {
				continue
			}
			for _, c := range ex.Calls {
				if c.Step == 1 && c.Index < len(s.Calls) {
					n[s.Calls[c.Index].MCPServer+"/"+s.Calls[c.Index].MCPTool]++
				}
			}
		}
	}
	best, most := "", 0
	for r, k := range n {
		if k > most || k == most && r < best {
			best, most = r, k
		}
	}
	return best
}
