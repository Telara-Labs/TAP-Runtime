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
	type branch struct {
		name  string
		graph *codegen.ProgramGraph
	}
	var branches []branch
	var head codegen.ProgramStep
	headInputs := map[string]codegen.ProgramInput{}
	seen := map[string]bool{}
	for _, p := range members {
		if len(p.Steps) != 2 || len(p.Loops) != 0 || len(p.Unresolved) > 0 || p.Confidence.Readiness == "needs_decision" {
			return nil, "a continuation has an unresolved binding, loop, or multi-step decision"
		}
		g, why := directGraph(p, by)
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
		if len(branches) == 0 {
			head = g.Steps[0]
			for _, arg := range head.Args {
				if arg.Value.Kind == "input" {
					for _, in := range g.Inputs {
						if in.Name == arg.Value.Input {
							headInputs[in.Name] = in
						}
					}
				}
			}
		} else if !reflect.DeepEqual(head, g.Steps[0]) {
			return nil, "the first operation has different input or effect contracts across continuations"
		}
		for input, want := range headInputs {
			found := false
			for _, in := range g.Inputs {
				if in.Name == input && reflect.DeepEqual(in, want) {
					found = true
				}
			}
			if !found {
				return nil, "the first operation has different caller inputs across continuations"
			}
		}
		if seen[name] {
			return nil, fmt.Sprintf("continuation %q has more than one execution shape", name)
		}
		seen[name] = true
		branches = append(branches, branch{name: name, graph: g})
	}
	if len(branches) != len(f.FollowUps) {
		return nil, "continuations cannot be mapped one-to-one to executable actions"
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
