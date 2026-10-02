package discover

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"
	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// bundleGraph runs one head and zero or more independently requested
// continuations. Every included continuation has already passed the
// relationship check; no tool names or Jira-specific reactions are built in.
func bundleGraph(f primitive.Family, members []primitive.Primitive, by map[string]*trace.Session) (*codegen.ProgramGraph, string) {
	if len(f.FollowUps) < 1 {
		return nil, "no supported continuation"
	}
	if len(f.Sources) > 0 {
		return nil, "alternative first operations need an explicit source-selection rule"
	}
	var branches []branch
	seen := map[string]bool{}
	usedHeads := map[string]bool{}
	usedSessions := map[string]bool{}
	route := headRoute(members, by)
	for _, p := range members {
		if score, _, reason := primitive.RelationshipEvidence(p, by); score != 100 {
			return nil, reason
		}
		var supported int
		p, supported, _ = modalFollowUpShape(p, by, route)
		if supported == 0 {
			return nil, "no consistent call shape was recorded through the common head route"
		}
		if why := continuationIssue(p); why != "" {
			return nil, why
		}
		g, why := directGraphVia(p, by, route)
		if g == nil && why == "fallback: no complete uses" {
			continue // no recorded use went through the family's route
		}
		if g == nil || len(g.Steps) != len(p.Steps) || len(g.Steps) < 2 || len(g.Problems) > 0 {
			if why == "" {
				why = "a continuation has no exact executable tool binding"
			}
			return nil, why
		}
		var words []string
		for _, op := range p.Steps[1:] {
			words = append(words, branchName(op))
		}
		name := strings.Join(words, "_then_")
		if name == "" || contains(words, "") {
			return nil, "a continuation has no stable action name"
		}
		if seen[name] {
			return nil, fmt.Sprintf("continuation %q has more than one execution shape", name)
		}
		for _, step := range g.Steps[1:] {
			if step.Loop != "" || step.LoopResultStep != 0 {
				return nil, fmt.Sprintf("continuation %q has a dependent loop or optional argument combination", name)
			}
		}
		seen[name] = true
		branches = append(branches, branch{name: name, graph: g, ops: p.Steps})
		for _, ex := range p.Executions {
			if len(ex.Calls) == 0 {
				continue
			}
			key := ex.Client + "\x00" + ex.Session
			usedHeads[fmt.Sprintf("%s\x00%d", key, ex.Calls[0].Index)] = true
			usedSessions[key] = true
		}
	}
	head, headInputs, why := mergeHeads(branches)
	if why != "" {
		return nil, why
	}
	if len(branches) < 1 {
		return nil, "no continuation was recorded through one route"
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
	result := &codegen.ProgramGraph{CandidateID: "lc_" + strings.TrimPrefix(f.ID, "pf_"), Executions: len(usedHeads), Sessions: len(usedSessions),
		Steps: []codegen.ProgramStep{head}, Cautions: []string{"The head runs once. Requested follow-ups run in listed order; later failures can leave earlier writes completed, and the error reports partial results."}}
	for _, in := range headInputs {
		result.Inputs = append(result.Inputs, in)
	}
	sort.Slice(result.Inputs, func(i, j int) bool { return result.Inputs[i].Name < result.Inputs[j].Name })
	for _, b := range branches {
		listName := b.name + "_items"
		if _, exists := headInputs[listName]; exists {
			return nil, fmt.Sprintf("the common operation already uses %q", listName)
		}
		item := codegen.ProgramInput{Name: listName, Type: "object", List: true, Optional: true,
			Source: "caller supplies zero or more independent follow-ups"}
		inputByName := map[string]codegen.ProgramInput{}
		for _, in := range b.graph.Inputs {
			inputByName[in.Name] = in
		}
		// The follow-up's steps run in one pass per item: branch step k
		// becomes bundle step base+k, and a result of an earlier follow-up
		// step is that same item's result.
		base := len(result.Steps) - 1
		usedFields := map[string]bool{}
		profiles := [][]string{{}}
		for k := 1; k < len(b.graph.Steps); k++ {
			step := b.graph.Steps[k]
			step.Args = append([]codegen.ProgramArg(nil), step.Args...)
			optionalFields := map[string]string{}
			for i := range step.Args {
				arg := &step.Args[i]
				v := &arg.Value
				switch v.Kind {
				case "result":
					if v.Step < 1 || v.Step > k {
						return nil, "a continuation reads an unsupported prior result"
					}
					if v.Step > 1 {
						*v = codegen.ProgramValue{Kind: "iteration_result", Step: base + v.Step, ResultPath: v.ResultPath}
					}
				case "selector":
				case "input":
					in, ok := inputByName[v.Input]
					if !ok || in.List || len(in.Allowed) > 0 || in.Optional != arg.Optional {
						return nil, fmt.Sprintf("continuation %q has an input without a required scalar type", b.name)
					}
					field := strings.TrimPrefix(v.Input, fmt.Sprintf("step_%d_", k+1))
					if usedFields[field] {
						field = branchName(b.ops[k]) + "_" + field
					}
					if field == "" || usedFields[field] {
						return nil, fmt.Sprintf("continuation %q has duplicate or unnamed item fields", b.name)
					}
					usedFields[field] = true
					item.Fields = append(item.Fields, codegen.ProgramInputField{Name: field, Path: arg.Path, Type: in.Type, Optional: arg.Optional})
					if arg.Optional {
						optionalFields[in.Name] = field
					}
					*v = codegen.ProgramValue{Kind: "item", ResultPath: "." + field}
				default:
					return nil, fmt.Sprintf("continuation %q has a non-independent result selection", b.name)
				}
			}
			if len(optionalFields) > 0 {
				if len(step.OptionalProfiles) == 0 {
					return nil, fmt.Sprintf("continuation %q has no observed optional field profiles", b.name)
				}
				// Each step's observed combinations hold for its own call;
				// steps of one item combine freely.
				var next [][]string
				for _, prior := range profiles {
					for _, profile := range step.OptionalProfiles {
						fields := append([]string{}, prior...)
						for _, name := range profile {
							field, ok := optionalFields[name]
							if !ok {
								return nil, fmt.Sprintf("continuation %q has an unknown optional field profile", b.name)
							}
							fields = append(fields, field)
						}
						sort.Strings(fields)
						next = append(next, fields)
					}
				}
				profiles = next
			}
			step.OptionalProfiles = nil
			step.Loop = listName
			if k > 1 {
				step.SameItemAs = base + 2
			}
			result.Steps = append(result.Steps, step)
		}
		if len(profiles) > 1 || len(profiles[0]) > 0 {
			item.ItemProfiles = profiles
		}
		sort.Slice(item.Fields, func(i, j int) bool { return item.Fields[i].Name < item.Fields[j].Name })
		result.Inputs = append(result.Inputs, item)
		result.Sources = append(result.Sources, b.graph.Sources...)
	}
	return result, ""
}

func branchName(op string) string {
	op = strings.TrimPrefix(strings.TrimPrefix(op, "mcp:"), "sh:")
	var b strings.Builder
	for _, r := range op {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// branch is one continuation's compiled two-step program.
type branch struct {
	name  string
	graph *codegen.ProgramGraph
	ops   []string // the recorded steps, head first
}

// modalFollowUpShape keeps the most supported tool route for one continuation.
// Argument presence differences on that route are represented by observed
// optional profiles in directGraphVia, never by invented defaults.
func modalFollowUpShape(p primitive.Primitive, by map[string]*trace.Session, headRoute string) (primitive.Primitive, int, int) {
	groups := map[string][]primitive.Execution{}
	total := 0
	for _, ex := range p.Executions {
		if ex.Overlaps != "" {
			continue
		}
		total++
		s := by[ex.Client+"\x00"+ex.Session]
		if s == nil {
			continue
		}
		// A use's shape is the tool route of each follow-up step.
		routes := map[int]string{}
		valid := true
		for _, ref := range ex.Calls {
			if ref.Index < 0 || ref.Index >= len(s.Calls) || s.Calls[ref.Index].Request != ex.Request {
				valid = false
				break
			}
			call := s.Calls[ref.Index]
			route := call.MCPServer + "/" + call.MCPTool
			if ref.Step == 1 {
				valid = valid && route == headRoute
				continue
			}
			if ref.Step < 2 || ref.Step > len(p.Steps) || routes[ref.Step] != "" && routes[ref.Step] != route {
				valid = false
				break
			}
			routes[ref.Step] = route
		}
		var parts []string
		for st := 2; st <= len(p.Steps); st++ {
			parts = append(parts, routes[st])
			valid = valid && routes[st] != ""
		}
		shape := strings.Join(parts, " > ")
		if valid && shape != "" {
			groups[shape] = append(groups[shape], ex)
		}
	}
	best, n := "", 0
	for shape, executions := range groups {
		if len(executions) > n || len(executions) == n && shape < best {
			best, n = shape, len(executions)
		}
	}
	p.Executions = groups[best]
	p.ExecutionCount = n
	return p, n, total
}

// mergeHeads builds the shared first step from every continuation's view of
// it. The tool and its binding must match; an argument the continuations
// agree on stays as recorded, one they differ on becomes a caller input
// (optional where some continuations omit it).
func mergeHeads(branches []branch) (codegen.ProgramStep, map[string]codegen.ProgramInput, string) {
	headInputs := map[string]codegen.ProgramInput{}
	if len(branches) == 0 {
		return codegen.ProgramStep{}, headInputs, "no continuation"
	}
	first := branches[0].graph.Steps[0]
	head := codegen.ProgramStep{Role: first.Role, Tool: first.Tool, Binding: first.Binding, Effect: first.Effect}
	type seenArg struct {
		arg   codegen.ProgramArg
		count int
		same  bool
	}
	args := map[string]*seenArg{}
	var order []string
	inputs := map[string]codegen.ProgramInput{}
	for _, b := range branches {
		st := b.graph.Steps[0]
		if st.Tool != first.Tool || !reflect.DeepEqual(st.Binding, first.Binding) {
			return head, headInputs, "the continuations start with different tools"
		}
		if st.Effect == "write" {
			head.Effect = "write"
		}
		for _, in := range b.graph.Inputs {
			inputs[in.Name] = in
		}
		for _, a := range st.Args {
			k := strings.Join(a.Path, "/")
			sa := args[k]
			if sa == nil {
				args[k] = &seenArg{arg: a, count: 1, same: true}
				order = append(order, k)
				continue
			}
			sa.count++
			if !reflect.DeepEqual(sa.arg.Value, a.Value) || sa.arg.JSONString != a.JSONString {
				sa.same = false
			}
			sa.arg.Optional = sa.arg.Optional || a.Optional
		}
	}
	sort.Strings(order)
	merged := map[string]string{} // arg path -> merged input name
	for _, k := range order {
		sa := args[k]
		a := sa.arg
		optional := a.Optional || sa.count < len(branches)
		switch {
		case sa.same && a.Value.Kind == "input":
			in := inputs[a.Value.Input]
			in.Optional = in.Optional || optional
			headInputs[in.Name] = in
			merged[k] = in.Name
		case sa.same && !optional:
		default:
			name := "step_1_" + strings.NewReplacer("/", "_", "-", "_").Replace(k)
			typ := "string"
			if in, ok := inputs[a.Value.Input]; ok && in.Type != "" {
				typ = in.Type
			}
			a.Value = codegen.ProgramValue{Kind: "input", Input: name}
			headInputs[name] = codegen.ProgramInput{Name: name, Type: typ, Optional: optional, Source: "supplied at invocation"}
			merged[k] = name
		}
		a.Optional = optional
		head.Args = append(head.Args, a)
	}
	// Optional inputs need the combinations the recorded uses showed: each
	// continuation's own combinations, in the merged names, plus the
	// arguments that continuation always passed.
	seenProfile := map[string]bool{}
	for _, b := range branches {
		st := b.graph.Steps[0]
		byInput := map[string]string{} // continuation's input name -> merged name
		var always []string
		for _, a := range st.Args {
			k := strings.Join(a.Path, "/")
			name, ok := merged[k]
			if !ok || !headInputs[name].Optional {
				continue
			}
			if a.Value.Kind == "input" {
				byInput[a.Value.Input] = name
			}
			if !a.Optional {
				always = append(always, name)
			}
		}
		profiles := st.OptionalProfiles
		if len(profiles) == 0 {
			profiles = [][]string{{}}
		}
		for _, p := range profiles {
			combo := append([]string{}, always...)
			for _, n := range p {
				if m, ok := byInput[n]; ok {
					combo = append(combo, m)
				}
			}
			sort.Strings(combo)
			combo = uniqueStrings(combo)
			if key := strings.Join(combo, ","); !seenProfile[key] {
				seenProfile[key] = true
				head.OptionalProfiles = append(head.OptionalProfiles, combo)
			}
		}
	}
	optional := false
	for _, in := range headInputs {
		optional = optional || in.Optional
	}
	if !optional {
		head.OptionalProfiles = nil
	}
	return head, headInputs, ""
}

func uniqueStrings(xs []string) []string {
	out := []string{}
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}
