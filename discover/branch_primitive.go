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

// branchGraph builds the narrow branch shape that the recorded calls prove:
// one common operation followed by one of several result-linked operations.
// The caller chooses one action at invocation. Co-occurrence alone cannot
// establish a rule for running several actions or choosing from a result.
func branchGraph(f primitive.Family, members []primitive.Primitive, by map[string]*trace.Session) (*codegen.ProgramGraph, string) {
	if len(f.FollowUps) < 2 {
		return nil, "no branch family"
	}
	if len(f.Sources) > 0 {
		return nil, "alternative first operations need an explicit source-selection rule"
	}
	var branches []branch
	seen := map[string]bool{}
	route := headRoute(members, by)
	for _, p := range members {
		if why := continuationIssue(p); why != "" {
			return nil, why
		}
		g, why := directGraphVia(p, by, route)
		if g == nil && why == "fallback: no complete uses" {
			continue // no recorded use went through the family's route
		}
		if g == nil || len(g.Steps) != 2 || len(g.Problems) > 0 {
			if why == "" {
				why = "a continuation has no exact executable tool binding"
			}
			return nil, why
		}
		name := branchName(p.Steps[1])
		if name == "" {
			return nil, "a continuation has no stable action name"
		}
		if seen[name] {
			return nil, fmt.Sprintf("continuation %q has more than one execution shape", name)
		}
		seen[name] = true
		branches = append(branches, branch{name: name, graph: g})
	}
	head, headInputs, why := mergeHeads(branches)
	if why != "" {
		return nil, why
	}
	if len(branches) < 2 {
		return nil, "fewer than two continuations were recorded through one route"
	}
	if _, exists := headInputs["action"]; exists {
		return nil, "the common operation already uses the action input name"
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
	result := &codegen.ProgramGraph{CandidateID: "lc_" + strings.TrimPrefix(f.ID, "pf_"), Executions: f.ExecutionCount, Sessions: f.SessionCount,
		Steps: []codegen.ProgramStep{head}}
	for _, in := range headInputs {
		result.Inputs = append(result.Inputs, in)
	}
	sort.Slice(result.Inputs, func(i, j int) bool { return result.Inputs[i].Name < result.Inputs[j].Name })
	choices := make([]string, 0, len(branches))
	for _, b := range branches {
		choices = append(choices, b.name)
	}
	result.Inputs = append(result.Inputs, codegen.ProgramInput{Name: "action", Type: "string", Allowed: choices,
		Source: "caller selects one recorded continuation at invocation"})
	for _, b := range branches {
		step := b.graph.Steps[1]
		step.WhenInput, step.WhenValue = "action", b.name
		renames := map[string]string{}
		for _, in := range b.graph.Inputs {
			if _, common := headInputs[in.Name]; common {
				continue
			}
			old := in.Name
			in.Name = b.name + "_" + old
			in.Optional = true
			in.RequiredWhenInput, in.RequiredWhenValue = "action", b.name
			renames[old] = in.Name
			result.Inputs = append(result.Inputs, in)
		}
		var profiles [][]string
		for _, profile := range step.OptionalProfiles {
			renamed := make([]string, 0, len(profile))
			for _, name := range profile {
				if r, ok := renames[name]; ok {
					name = r
				}
				renamed = append(renamed, name)
			}
			profiles = append(profiles, renamed)
		}
		step.OptionalProfiles = profiles
		if renamed, ok := renames[step.Loop]; ok {
			step.Loop = renamed
		}
		for i := range step.Args {
			v := &step.Args[i].Value
			if renamed, ok := renames[v.Input]; ok {
				v.Input = renamed
			}
			if v.Kind == "result" && v.Step != 1 {
				return nil, "a continuation reads an unsupported prior result"
			}
		}
		result.Steps = append(result.Steps, step)
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
