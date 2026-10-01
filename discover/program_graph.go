package discover

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/routine"

	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
)

// ProgramGraph is a proposed executable shape, not a claim about the user's
// intent. It contains parameter roles and result bindings, never the recorded
// resource values. Problems name the evidence that is still missing before
// code can be offered for private acceptance.
type ProgramGraph struct {
	CandidateID       string             `json:"candidate_id"`
	Executions        int                `json:"executions"`
	Sessions          int                `json:"sessions"`
	Inputs            []ProgramInput     `json:"inputs"`
	Steps             []ProgramStep      `json:"steps"`
	Problems          []string           `json:"problems,omitempty"`
	Sources           []string           `json:"sources"`
	InlineFileReplace *InlineFileReplace `json:"inline_file_replace,omitempty"`
}

type InlineFileReplace struct {
	Embedded bool `json:"embedded"`
}

type ProgramInput struct {
	Name     string              `json:"name"`
	Type     string              `json:"type"`
	List     bool                `json:"list,omitempty"`
	Optional bool                `json:"optional,omitempty"`
	Source   string              `json:"source"`
	Fields   []ProgramInputField `json:"fields,omitempty"`
}

type ProgramInputField struct {
	Name string   `json:"name"`
	Path []string `json:"path"`
	Type string   `json:"type"`
}

type ProgramStep struct {
	Role           string              `json:"role"`
	Tool           string              `json:"tool"`
	Binding        *ProgramToolBinding `json:"binding,omitempty"`
	Command        string              `json:"command,omitempty"`
	Pipeline       []ProgramCommand    `json:"pipeline,omitempty"`
	Effect         string              `json:"effect"`
	Loop           string              `json:"loop,omitempty"`             // input name, when for_each
	LoopResultStep int                 `json:"loop_result_step,omitempty"` // one-based earlier step
	LoopResultPath string              `json:"loop_result_path,omitempty"` // collection in that result
	Args           []ProgramArg        `json:"args"`
	// Pairs of caller-supplied result indexes that source evidence kept
	// distinct within this call. The generated program enforces each pair.
	DistinctResultInputs [][]string `json:"distinct_result_inputs,omitempty"`
	// A caller-selected subset of a prior result list must not repeat an
	// item when the source executions each used distinct items.
	DistinctLoopSelections bool `json:"distinct_loop_selections,omitempty"`
	// OptionalProfiles are the exact combinations of optional inputs that
	// occurred in successful source executions. The generator refuses an
	// unobserved combination instead of guessing the tool's defaults.
	OptionalProfiles [][]string `json:"optional_profiles,omitempty"`
}

type ProgramToolBinding struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

type ProgramCommand struct {
	Name      string `json:"name"`
	Effect    string `json:"effect"`
	Connector string `json:"connector,omitempty"` // pipe or and, for stages after the first
}

// ProgramArg identifies one leaf in an action's arguments. JSONString means
// the top-level tool argument was itself an encoded JSON object (a common MCP
// gateway convention), so generation must re-encode it after substitution.
type ProgramArg struct {
	Path       []string     `json:"path"`
	JSONString bool         `json:"json_string,omitempty"`
	Optional   bool         `json:"optional,omitempty"`
	Value      ProgramValue `json:"value"`
}

type ProgramValue struct {
	Kind           string `json:"kind"` // input, result, indexed_result, collection_index, collection_index_item, item, item_result, selected_result, or selector
	Input          string `json:"input,omitempty"`
	Step           int    `json:"step,omitempty"` // one-based producer step
	ResultPath     string `json:"result_path,omitempty"`
	CollectionPath string `json:"collection_path,omitempty"`
	PredicatePath  string `json:"predicate_path,omitempty"`
	Selector       string `json:"selector,omitempty"`
}

var indexedResultPath = regexp.MustCompile(`\[[0-9]+\]`)

type observedOp struct {
	node   retrieval.SpanNode
	role   string
	fields map[string]trace.ObservedField
}

type observedTrace struct {
	span   model.SpanProposal
	groups [][]observedOp
}

type observedInputVector struct {
	values   []string
	typeName string
}

// SynthesizeProgramGraph uses the exact calls named by a recurring logic
// candidate. Re-reading source calls and their hashes prevents a stale report
// from silently generating a program from different history. The classifier
// is deliberately action-agnostic: it reads operation names, argument trees,
// typed slots and structured result paths, not Jira/GitLab special cases.
func SynthesizeProgramGraph(c model.LogicCandidate, proposals []model.SpanProposal, sessions []trace.Session) (*ProgramGraph, error) {
	bySpan := make(map[string]model.SpanProposal, len(proposals))
	for _, p := range proposals {
		bySpan[p.ID] = p
	}
	wanted := map[string]bool{}
	for _, id := range c.Members {
		p, ok := bySpan[id]
		if !ok {
			return nil, fmt.Errorf("candidate %s names absent span %s", c.ID, id)
		}
		wanted[p.Client+"\x00"+p.Session] = true
	}
	cp := make([]trace.Session, 0, len(wanted))
	for _, s := range sessions {
		if !wanted[s.Client+"\x00"+s.ID] {
			continue
		}
		s.Calls = append([]trace.Call(nil), s.Calls...)
		cp = append(cp, s)
	}
	trace.DropCopiedCalls(cp)
	bySession := map[string]trace.Session{}
	for _, s := range cp {
		bySession[s.Client+"\x00"+s.ID] = s
	}
	norm := trace.Normalize(cp)
	byNorm := map[string]trace.NormSession{}
	for _, ns := range norm {
		byNorm[ns.Client+"\x00"+ns.ID] = ns
	}
	graph := &ProgramGraph{CandidateID: c.ID, Executions: c.Executions, Sessions: c.Sessions}
	var traces []observedTrace
	for _, id := range c.Members {
		p, ok := bySpan[id]
		if !ok {
			return nil, fmt.Errorf("candidate %s names absent span %s", c.ID, id)
		}
		key := p.Client + "\x00" + p.Session
		s, ok := bySession[key]
		if !ok || p.Request < 0 || p.Request >= len(s.Requests) {
			return nil, fmt.Errorf("source for span %s is unavailable", id)
		}
		ns, ok := byNorm[key]
		if !ok {
			return nil, fmt.Errorf("normalized source for span %s is unavailable", id)
		}
		byCall := map[int][]trace.Step{}
		for _, st := range ns.Steps {
			byCall[st.Call] = append(byCall[st.Call], st)
		}
		var calls []int
		for i := range s.Calls {
			if s.Calls[i].Request == p.Request {
				calls = append(calls, i)
			}
		}
		nodes := retrieval.BuildSpanNodes(s, p.Request, calls, byCall)
		byOrdinal := map[int]retrieval.SpanNode{}
		for _, n := range nodes {
			byOrdinal[n.Ordinal] = n
		}
		var ops []observedOp
		for i, ordinal := range p.Calls {
			n, ok := byOrdinal[ordinal]
			if !ok || i >= len(p.CallHashes) || retrieval.SpanCallHash(n.Call) != p.CallHashes[i] {
				return nil, fmt.Errorf("span %s source call %d changed", id, ordinal)
			}
			ops = append(ops, observedOp{node: n, role: retrieval.LogicRole(retrieval.SpanActionRole(n)), fields: trace.ObservedArgs(n.Call)})
		}
		if len(ops) == 0 {
			continue
		}
		tr := observedTrace{span: p}
		for _, op := range ops {
			if len(tr.groups) == 0 || tr.groups[len(tr.groups)-1][0].role != op.role {
				tr.groups = append(tr.groups, []observedOp{op})
			} else {
				tr.groups[len(tr.groups)-1] = append(tr.groups[len(tr.groups)-1], op)
			}
		}
		graph.Sources = append(graph.Sources, p.ID)
		traces = append(traces, tr)
	}
	if len(traces) == 0 {
		return nil, fmt.Errorf("candidate %s has no readable executions", c.ID)
	}
	sort.Strings(graph.Sources)
	if traces[0].span.CodeShape != "" {
		synthesizeInlineFileReplace(graph, traces)
		return graph, nil
	}
	// One candidate may carry several overlapping slices of a session. Shape
	// mismatches are explicit; they are not repaired by dropping an action.
	roles := make([]string, len(traces[0].groups))
	for i, group := range traces[0].groups {
		roles[i] = group[0].role
	}
	for _, tr := range traces[1:] {
		if len(tr.groups) != len(roles) {
			graph.Problems = append(graph.Problems, "executions have different action boundaries")
			continue
		}
		for i, group := range tr.groups {
			if group[0].role != roles[i] {
				graph.Problems = append(graph.Problems, "executions have different action order")
				break
			}
		}
	}
	if len(graph.Problems) > 0 {
		return graph, nil
	}
	inputVectors := map[string]observedInputVector{}
	for step, role := range roles {
		first := traces[0].groups[step][0]
		ps := ProgramStep{Role: role, Tool: first.node.Call.Tool, Effect: first.node.Effect}
		if ps.Tool == "shell" {
			plan, err := shellparse.ProgramShellPlan(first.node.Call.Command)
			if err != nil {
				graph.Problems = append(graph.Problems, fmt.Sprintf("step %d: %v", step+1, err))
			} else {
				if len(plan) > 1 && len(first.node.Steps) != len(plan) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d pipeline stage effects are not resolved", step+1))
				}
				for j, stage := range plan {
					words := stage.Words
					if shellparse.ProgramCommandRunsCode(words[0], words[1:]) {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d stage %d runs code whose reach is not described by argv", step+1, j+1))
					}
					if len(plan) == 1 {
						ps.Command = words[0]
						continue
					}
					effect := "unknown"
					if j < len(first.node.Steps) {
						effect = trace.StepEffect(first.node.Steps[j])
					}
					if effect != "read" && effect != "write" {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d stage %d has unknown effect", step+1, j+1))
					}
					ps.Pipeline = append(ps.Pipeline, ProgramCommand{Name: words[0], Effect: effect, Connector: stage.Connector})
				}
			}
		} else if first.node.Call.MCPServer != "" && first.node.Call.MCPTool != "" {
			ps.Binding = &ProgramToolBinding{Server: first.node.Call.MCPServer, Tool: first.node.Call.MCPTool}
		} else {
			graph.Problems = append(graph.Problems, fmt.Sprintf("step %d lacks an exact MCP server/tool binding", step+1))
		}
		if ps.Tool != "shell" && (!strings.HasPrefix(ps.Tool, "mcp:") || strings.Contains(role, "[unresolved]")) {
			graph.Problems = append(graph.Problems, fmt.Sprintf("step %d has no resolved MCP action binding", step+1))
		}
		if ps.Effect != "read" && ps.Effect != "write" {
			graph.Problems = append(graph.Problems, fmt.Sprintf("step %d has unknown effect", step+1))
			ps.Effect = "write" // conservative display, not approval to generate
		}
		loop := false
		for _, tr := range traces {
			for _, rep := range tr.span.Composition.Repetition {
				if retrieval.LogicRole(rep.Action) == role && rep.Kind == "for_each" {
					loop = true
				}
			}
			for _, op := range tr.groups[step] {
				if op.node.Call.Outcome == trace.OutcomeFailed {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d failed in a source execution", step+1))
				}
				if op.node.Call.Tool != ps.Tool || op.role != ps.Role {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d uses incompatible actions", step+1))
				}
				if ps.Binding != nil && (op.node.Call.MCPServer != ps.Binding.Server || op.node.Call.MCPTool != ps.Binding.Tool) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d uses incompatible MCP bindings", step+1))
				}
				if ps.Tool == "shell" {
					plan, err := shellparse.ProgramShellPlan(op.node.Call.Command)
					if err != nil || len(plan) == 0 || len(plan) != max(1, len(ps.Pipeline)) {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d has incompatible or non-literal command syntax", step+1))
					} else {
						for j, stage := range plan {
							words := stage.Words
							want := ps.Command
							if len(ps.Pipeline) > 0 {
								want = ps.Pipeline[j].Name
								if stage.Connector != ps.Pipeline[j].Connector {
									graph.Problems = append(graph.Problems, fmt.Sprintf("step %d compound connector changes", step+1))
								}
								if j >= len(op.node.Steps) || trace.StepEffect(op.node.Steps[j]) != ps.Pipeline[j].Effect {
									graph.Problems = append(graph.Problems, fmt.Sprintf("step %d pipeline stage %d changes effect", step+1, j+1))
								}
							}
							if words[0] != want || shellparse.ProgramCommandRunsCode(words[0], words[1:]) {
								graph.Problems = append(graph.Problems, fmt.Sprintf("step %d has incompatible command operations", step+1))
							}
						}
					}
				}
			}
		}
		if loop {
			for _, tr := range traces {
				if len(tr.groups[step]) > 1 && !observedForEach(tr.span, role) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d repeats without item-loop evidence", step+1))
				}
			}
		} else {
			for _, tr := range traces {
				if len(tr.groups[step]) != 1 {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d repeats without a supported loop", step+1))
				}
			}
		}
		fields := map[string]trace.ObservedField{}
		for _, tr := range traces {
			for _, op := range tr.groups[step] {
				for path, field := range op.fields {
					if _, seen := fields[path]; !seen {
						fields[path] = field
					}
				}
			}
		}
		paths := make([]string, 0, len(fields))
		for path := range fields {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		var itemPaths []string
		for _, path := range paths {
			base := fields[path]
			var values []string
			itemVaries := false
			shapeOK := true
			optional := false
			stableByTrace := make([]string, 0, len(traces))
			stableAcrossIterations := true
			for _, tr := range traces {
				if len(tr.groups) <= step {
					shapeOK = false
					break
				}
				seen := map[string]bool{}
				present := 0
				traceValue := ""
				for _, op := range tr.groups[step] {
					f, ok := op.fields[path]
					if !ok {
						optional = true
						continue
					}
					present++
					if f.TypeName != base.TypeName || f.JsonString != base.JsonString {
						shapeOK = false
						break
					}
					values = append(values, f.Value)
					seen[f.Value] = true
					traceValue = f.Value
				}
				if present != 0 && present != len(tr.groups[step]) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d argument %s is present in only some loop iterations", step+1, path))
				}
				if len(seen) > 1 {
					itemVaries = true
					stableAcrossIterations = false
				}
				stableByTrace = append(stableByTrace, traceValue)
			}
			if !shapeOK {
				graph.Problems = append(graph.Problems, fmt.Sprintf("step %d argument %s changes shape", step+1, path))
				continue
			}
			arg := ProgramArg{Path: base.Path, JSONString: base.JsonString, Optional: optional}
			resultStep, resultPath, resultOK := observedResultBinding(traces, step, path)
			listStep, listPath, itemPath, listOK := observedCollectionBinding(traces, step, path)
			selection, selectionOK := observedUniqueSelection(traces, step, path)
			indexedCollection, indexedCollectionOK := observedIndexedCollectionBinding(traces, step, path)
			switch {
			case trace.OperationSelector(first.node.Call, path):
				if optional || !allSame(values) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d selector %s changes", step+1, path))
				} else {
					arg.Value = ProgramValue{Kind: "selector", Selector: values[0]}
				}
			case loop && itemVaries:
				itemPaths = append(itemPaths, path)
				if !optional && listOK {
					if ps.LoopResultStep != 0 && (ps.LoopResultStep != listStep || ps.LoopResultPath != listPath) {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d item fields come from different collections", step+1))
					}
					ps.LoopResultStep, ps.LoopResultPath = listStep, listPath
					arg.Value = ProgramValue{Kind: "item_result", Step: listStep, ResultPath: itemPath}
					break
				}
				if !optional && indexedCollectionOK {
					name := programInputName(step, base.Path, true) + "_source_indexes"
					if ps.Loop != "" && ps.Loop != name {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d item fields use different selection lists", step+1))
					}
					ps.Loop = name
					ps.DistinctLoopSelections = true
					arg.Value = ProgramValue{Kind: "collection_index_item", Step: indexedCollection.step, CollectionPath: indexedCollection.collection, ResultPath: indexedCollection.item, Input: name}
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: "integer", List: true, Source: fmt.Sprintf("caller selects positions from step %d result%s", indexedCollection.step, indexedCollection.collection)})
					break
				}
				if optional || resultOK || possiblePriorResult(traces, step, path) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d item %s comes from an earlier result; collection binding is not determined", step+1, path))
				}
				name := programInputName(step, base.Path, true)
				ps.Loop = name
				arg.Value = ProgramValue{Kind: "item", Input: name}
				graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: base.TypeName, List: true, Source: "supplied at invocation"})
			case !loop && !optional && (!resultOK || indexedResultPath.MatchString(resultPath)) && selectionOK:
				name := fmt.Sprintf("step_%d_select_%s", step+1, routine.SanitizeName(selection.predicatePath))
				arg.Value = ProgramValue{Kind: "selected_result", Step: selection.step, ResultPath: selection.itemPath,
					CollectionPath: selection.collectionPath, PredicatePath: selection.predicatePath, Input: name}
				found := false
				for _, input := range graph.Inputs {
					if input.Name == name {
						found = true
						break
					}
				}
				if !found {
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: selection.predicateType, Source: "supplied at invocation (unique result selection)"})
				}
			case !loop && !optional && indexedCollectionOK:
				name := programInputName(step, base.Path, false) + "_source_index"
				arg.Value = ProgramValue{Kind: "collection_index", Step: indexedCollection.step, CollectionPath: indexedCollection.collection, ResultPath: indexedCollection.item, Input: name}
				graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: "integer", Source: fmt.Sprintf("caller selects one position from step %d result%s", indexedCollection.step, indexedCollection.collection)})
			case resultOK && !optional && !indexedResultPath.MatchString(resultPath):
				producer := graph.Steps[resultStep-1]
				if producer.Loop != "" || producer.LoopResultStep != 0 {
					if loop {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d argument %s selects one result of a loop without a determined per-item join", step+1, path))
						break
					}
					name := programInputName(step, base.Path, false) + "_source_index"
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: "integer", Source: fmt.Sprintf("caller selects one result from step %d by zero-based position", resultStep)})
					arg.Value = ProgramValue{Kind: "indexed_result", Step: resultStep, ResultPath: resultPath, Input: name}
				} else {
					arg.Value = ProgramValue{Kind: "result", Step: resultStep, ResultPath: resultPath}
				}
			case possiblePriorResult(traces, step, path) || resultOK:
				graph.Problems = append(graph.Problems, fmt.Sprintf("step %d argument %s appears to use an earlier result but its path is not determined", step+1, path))
			default:
				name := programInputName(step, base.Path, false)
				prior := ""
				if !optional && stableAcrossIterations {
					prior = sameInputVector(inputVectors, stableByTrace, base.TypeName, base.Path[len(base.Path)-1])
				}
				if prior != "" {
					name = prior
				} else {
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: base.TypeName, Optional: optional, Source: "supplied at invocation"})
					if !optional && stableAcrossIterations {
						inputVectors[name] = observedInputVector{values: stableByTrace, typeName: base.TypeName}
					}
				}
				arg.Value = ProgramValue{Kind: "input", Input: name}
			}
			ps.Args = append(ps.Args, arg)
		}
		for i := range ps.Args {
			a := ps.Args[i]
			if a.Value.Kind != "indexed_result" {
				continue
			}
			for j := i + 1; j < len(ps.Args); j++ {
				b := ps.Args[j]
				if b.Value.Kind != "indexed_result" || a.Value.Step != b.Value.Step {
					continue
				}
				if observedDistinctArgumentValues(traces, step, strings.Join(a.Path, "/"), strings.Join(b.Path, "/")) {
					ps.DistinctResultInputs = append(ps.DistinctResultInputs, []string{a.Value.Input, b.Value.Input})
				}
			}
		}
		if len(itemPaths) > 1 {
			callerItems, resultItems := 0, 0
			for _, arg := range ps.Args {
				switch arg.Value.Kind {
				case "item":
					callerItems++
				case "item_result":
					resultItems++
				}
			}
			switch {
			case resultItems == len(itemPaths) && callerItems == 0 && ps.LoopResultStep != 0:
				// Several fields of the same result item remain correlated.
			case callerItems == len(itemPaths) && resultItems == 0 && ps.LoopResultStep == 0:
				mergeProgramItemFields(graph, &ps, step)
			default:
				graph.Problems = append(graph.Problems, fmt.Sprintf("step %d has changing fields from incompatible item sources", step+1))
			}
		}
		optionalArgs := make([]ProgramArg, 0)
		for _, arg := range ps.Args {
			if arg.Optional {
				optionalArgs = append(optionalArgs, arg)
			}
		}
		if len(optionalArgs) > 0 {
			profiles := map[string][]string{}
			for _, tr := range traces {
				profile := make([]string, 0, len(optionalArgs))
				for _, arg := range optionalArgs {
					if _, present := tr.groups[step][0].fields[strings.Join(arg.Path, "/")]; present {
						profile = append(profile, arg.Value.Input)
					}
				}
				sort.Strings(profile)
				profiles[strings.Join(profile, "\x00")] = profile
			}
			keys := make([]string, 0, len(profiles))
			for key := range profiles {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				profile := profiles[key]
				if profile == nil {
					profile = []string{}
				}
				ps.OptionalProfiles = append(ps.OptionalProfiles, profile)
			}
		}
		graph.Steps = append(graph.Steps, ps)
	}
	graph.Problems = uniqueStrings(graph.Problems)
	return graph, nil
}

func mergeProgramItemFields(graph *ProgramGraph, ps *ProgramStep, step int) {
	name := fmt.Sprintf("step_%d_items", step+1)
	fields := make([]ProgramInputField, 0)
	oldInputs := map[string]bool{}
	usedNames := map[string]int{}
	for i := range ps.Args {
		arg := &ps.Args[i]
		if arg.Value.Kind != "item" {
			continue
		}
		oldInputs[arg.Value.Input] = true
		base := routine.SanitizeName(strings.Join(arg.Path, "_"))
		usedNames[base]++
		fieldName := base
		if usedNames[base] > 1 {
			fieldName = fmt.Sprintf("%s_%d", base, usedNames[base])
		}
		fieldType := "string"
		for _, input := range graph.Inputs {
			if input.Name == arg.Value.Input {
				fieldType = input.Type
				break
			}
		}
		fields = append(fields, ProgramInputField{Name: fieldName, Path: append([]string(nil), arg.Path...), Type: fieldType})
		arg.Value.Input = name
		arg.Value.ResultPath = trace.JqKeyPath(fieldName)
	}
	kept := graph.Inputs[:0]
	for _, input := range graph.Inputs {
		if !oldInputs[input.Name] {
			kept = append(kept, input)
		}
	}
	graph.Inputs = append(kept, ProgramInput{Name: name, Type: "object", List: true, Source: "supplied at invocation", Fields: fields})
	ps.Loop = name
}

func observedForEach(p model.SpanProposal, role string) bool {
	for _, r := range p.Composition.Repetition {
		if retrieval.LogicRole(r.Action) == role && r.Kind == "for_each" {
			return true
		}
	}
	return false
}

func allSame(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, v := range values[1:] {
		if v != values[0] {
			return false
		}
	}
	return true
}

func observedResultBinding(traces []observedTrace, step int, path string) (int, string, bool) {
	producer, resultPath := -1, ""
	for _, tr := range traces {
		for _, op := range tr.groups[step] {
			value := op.fields[path].Value
			foundStep, foundPath := -1, ""
			for prior := step - 1; prior >= 0; prior-- {
				for _, parent := range tr.groups[prior] {
					for i, id := range parent.node.Call.OutIDs {
						if id == value && i < len(parent.node.Call.OutPaths) && parent.node.Call.OutPaths[i] != "" && parent.node.Call.OutPaths[i] != "*" {
							if foundStep >= 0 {
								return 0, "", false // ambiguous producer
							}
							foundStep, foundPath = prior, parent.node.Call.OutPaths[i]
						}
					}
				}
			}
			if foundStep < 0 || (producer >= 0 && (producer != foundStep || resultPath != foundPath)) {
				return 0, "", false
			}
			producer, resultPath = foundStep, foundPath
		}
	}
	if producer < 0 {
		return 0, "", false
	}
	return producer + 1, resultPath, true
}

func observedDistinctArgumentValues(traces []observedTrace, step int, left, right string) bool {
	if len(traces) == 0 {
		return false
	}
	for _, tr := range traces {
		for _, op := range tr.groups[step] {
			a, aOK := op.fields[left]
			b, bOK := op.fields[right]
			if !aOK || !bOK || a.Value == b.Value {
				return false
			}
		}
	}
	return true
}

type uniqueSelection struct {
	step           int
	collectionPath string
	itemPath       string
	predicatePath  string
	predicateType  string
}

type selectionEvidence struct {
	selection uniqueSelection
	firstAll  bool
	lastAll   bool
}

// observedUniqueSelection learns only a parameterized equality predicate.
// A selected field value must be stable and unique in every complete source
// list, and the selected position must vary so a fixed first/last choice does
// not explain all evidence. The literal value is never placed in the graph.
func observedUniqueSelection(traces []observedTrace, step int, path string) (uniqueSelection, bool) {
	if len(traces) < 2 {
		return uniqueSelection{}, false
	}
	sessions := map[string]bool{}
	var common map[string]selectionEvidence
	for _, tr := range traces {
		sessions[tr.span.Client+"\x00"+tr.span.Session] = true
		if len(tr.groups[step]) != 1 {
			return uniqueSelection{}, false
		}
		value, ok := tr.groups[step][0].fields[path]
		if !ok {
			return uniqueSelection{}, false
		}
		wanted := trace.ResultValueDigest(value.Value)
		matches := map[string]selectionEvidence{}
		for prior := 0; prior < step; prior++ {
			if len(tr.groups[prior]) != 1 {
				continue
			}
			call := tr.groups[prior][0].node.Call
			collections := call.OutCollections
			if len(collections) == 0 {
				collections = trace.ResultCollections(call.Output)
			}
			for _, collection := range collections {
				if collection.Count < 2 {
					continue
				}
				for itemPath, itemField := range collection.Fields {
					if itemField.Type != value.TypeName || len(itemField.Digests) != collection.Count {
						continue
					}
					index := -1
					for i, digest := range itemField.Digests {
						if digest == wanted {
							if index >= 0 {
								index = -2
								break
							}
							index = i
						}
					}
					if index < 0 {
						continue
					}
					for predicatePath, predicateField := range collection.Fields {
						if predicatePath == itemPath || len(predicateField.Digests) != collection.Count {
							continue
						}
						selected := predicateField.Digests[index]
						unique := true
						for i, digest := range predicateField.Digests {
							if i != index && digest == selected {
								unique = false
								break
							}
						}
						if !unique {
							continue
						}
						candidate := uniqueSelection{prior + 1, collection.Path, itemPath, predicatePath, predicateField.Type}
						key := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s", candidate.step, candidate.collectionPath, candidate.itemPath, candidate.predicatePath, candidate.predicateType, selected)
						matches[key] = selectionEvidence{candidate, index == 0, index == collection.Count-1}
					}
				}
			}
		}
		if common == nil {
			common = matches
		} else {
			for key, existing := range common {
				current, ok := matches[key]
				if !ok {
					delete(common, key)
					continue
				}
				existing.firstAll = existing.firstAll && current.firstAll
				existing.lastAll = existing.lastAll && current.lastAll
				common[key] = existing
			}
		}
		if len(common) == 0 {
			return uniqueSelection{}, false
		}
	}
	if len(sessions) < 2 {
		return uniqueSelection{}, false
	}
	var chosen uniqueSelection
	count := 0
	for _, evidence := range common {
		if evidence.firstAll || evidence.lastAll {
			continue
		}
		chosen = evidence.selection
		count++
	}
	return chosen, count == 1
}

// observedCollectionBinding proves that every item in an earlier result list
// was consumed once, in order. A subset, reordered list, malformed result,
// or ambiguous source is not a safe program to synthesize.
func observedCollectionBinding(traces []observedTrace, step int, path string) (int, string, string, bool) {
	var common map[string]collectionBinding
	for _, tr := range traces {
		group := tr.groups[step]
		values := make([]trace.ObservedField, 0, len(group))
		for _, op := range group {
			field, ok := op.fields[path]
			if !ok {
				return 0, "", "", false
			}
			values = append(values, field)
		}
		matches := map[string]collectionBinding{}
		for prior := 0; prior < step; prior++ {
			if len(tr.groups[prior]) == len(values) {
				path, ok := observedParallelResultPath(tr.groups[prior], values)
				if ok {
					match := collectionBinding{prior + 1, "", path}
					matches[fmt.Sprintf("%d\x00%s\x00%s", prior+1, "", path)] = match
				}
			}
			if len(tr.groups[prior]) != 1 {
				continue
			}
			call := tr.groups[prior][0].node.Call
			collections := call.OutCollections
			if len(collections) == 0 {
				collections = trace.ResultCollections(call.Output)
			}
			for _, collection := range collections {
				if collection.Count != len(values) {
					continue
				}
				for itemPath, field := range collection.Fields {
					if len(field.Digests) != len(values) {
						continue
					}
					aligned := true
					for i, value := range values {
						if value.TypeName != field.Type || trace.ResultValueDigest(value.Value) != field.Digests[i] {
							aligned = false
							break
						}
					}
					if aligned {
						match := collectionBinding{prior + 1, collection.Path, itemPath}
						matches[fmt.Sprintf("%d\x00%s\x00%s", prior+1, collection.Path, itemPath)] = match
					}
				}
			}
		}
		if common == nil {
			common = matches
		} else {
			for key := range common {
				if _, ok := matches[key]; !ok {
					delete(common, key)
				}
			}
		}
		if len(common) == 0 {
			return 0, "", "", false
		}
	}
	if len(common) != 1 {
		return 0, "", "", false
	}
	for _, match := range common {
		return match.step, match.collection, match.item, true
	}
	return 0, "", "", false
}

// observedParallelResultPath proves an index-preserving join between two
// repeated steps. The producer's generated result is itself a list even when
// each individual call returned a scalar object.
func observedParallelResultPath(producers []observedOp, values []trace.ObservedField) (string, bool) {
	if len(producers) != len(values) || len(values) == 0 {
		return "", false
	}
	path := ""
	seen := map[string]bool{}
	for i, producer := range producers {
		value := values[i].Value
		if len(values) > 1 && seen[value] {
			return "", false
		}
		seen[value] = true
		found := ""
		for j, id := range producer.node.Call.OutIDs {
			if id != value || j >= len(producer.node.Call.OutPaths) {
				continue
			}
			candidate := producer.node.Call.OutPaths[j]
			if candidate == "" || candidate == "*" || found != "" {
				return "", false
			}
			found = candidate
		}
		if found == "" || (path != "" && found != path) {
			return "", false
		}
		path = found
	}
	return path, true
}

type collectionBinding struct {
	step       int
	collection string
	item       string
}

// observedIndexedCollectionBinding proves that each consumed value is one
// distinct item of a complete earlier result list. It does not infer why the
// agent chose those items: the generated program asks the caller for their
// zero-based positions. Multiple possible source fields remain unresolved.
func observedIndexedCollectionBinding(traces []observedTrace, step int, path string) (collectionBinding, bool) {
	var common map[string]collectionBinding
	for _, tr := range traces {
		if step >= len(tr.groups) {
			return collectionBinding{}, false
		}
		values := make([]trace.ObservedField, 0, len(tr.groups[step]))
		for _, op := range tr.groups[step] {
			value, ok := op.fields[path]
			if !ok {
				return collectionBinding{}, false
			}
			values = append(values, value)
		}
		matches := map[string]collectionBinding{}
		for prior := 0; prior < step; prior++ {
			if len(tr.groups[prior]) != 1 {
				continue
			}
			call := tr.groups[prior][0].node.Call
			collections := call.OutCollections
			if len(collections) == 0 {
				collections = trace.ResultCollections(call.Output)
			}
			for _, collection := range collections {
				if collection.Count < len(values) {
					continue
				}
				for itemPath, field := range collection.Fields {
					if len(field.Digests) != collection.Count {
						continue
					}
					used := map[int]bool{}
					valid := true
					for _, value := range values {
						if value.TypeName != field.Type {
							valid = false
							break
						}
						wanted, index := trace.ResultValueDigest(value.Value), -1
						for i, digest := range field.Digests {
							if digest == wanted {
								if index >= 0 {
									index = -2 // duplicate field value: position is ambiguous
									break
								}
								index = i
							}
						}
						if index < 0 || used[index] {
							valid = false
							break
						}
						used[index] = true
					}
					if valid {
						binding := collectionBinding{step: prior + 1, collection: collection.Path, item: itemPath}
						matches[fmt.Sprintf("%d\x00%s\x00%s", binding.step, binding.collection, binding.item)] = binding
					}
				}
			}
		}
		if common == nil {
			common = matches
		} else {
			for key := range common {
				if _, ok := matches[key]; !ok {
					delete(common, key)
				}
			}
		}
		if len(common) == 0 {
			return collectionBinding{}, false
		}
	}
	if len(common) != 1 {
		return collectionBinding{}, false
	}
	for _, binding := range common {
		return binding, true
	}
	return collectionBinding{}, false
}

func possiblePriorResult(traces []observedTrace, step int, path string) bool {
	for _, tr := range traces {
		for _, op := range tr.groups[step] {
			field, ok := op.fields[path]
			if !ok {
				continue
			}
			value := field.Value
			digest := trace.ResultValueDigest(value)
			for prior := 0; prior < step; prior++ {
				for _, parent := range tr.groups[prior] {
					for _, id := range parent.node.Call.OutIDs {
						if id == value {
							return true
						}
					}
					// OutIDs covers only the first 64 identifiers. A complete
					// bounded collection can prove a later item was returned as
					// well, even when its value was beyond that preview. An exact
					// argument echo on this call can instead be a shared caller
					// input and is handled by input-vector alignment.
					if priorField, echoed := parent.fields[path]; echoed && priorField.TypeName == field.TypeName && priorField.Value == value {
						continue
					}
					for _, collection := range parent.node.Call.OutCollections {
						for _, itemField := range collection.Fields {
							if itemField.Type != field.TypeName {
								continue
							}
							for _, itemDigest := range itemField.Digests {
								if itemDigest == digest {
									return true
								}
							}
						}
					}
				}
			}
		}
	}
	return false
}

func programInputName(step int, path []string, list bool) string {
	name := routine.SanitizeName(strings.Join(path, "_"))
	if list {
		name += "s"
	}
	return "step_" + strconv.Itoa(step+1) + "_" + name
}

func sameInputVector(prior map[string]observedInputVector, values []string, typeName, field string) string {
	names := make([]string, 0, len(prior))
	for name := range prior {
		names = append(names, name)
	}
	sort.Strings(names)
	fallback, fallbackCount := "", 0
	for _, name := range names {
		vector := prior[name]
		seen := vector.values
		if vector.typeName != typeName {
			continue
		}
		if len(seen) != len(values) {
			continue
		}
		match, distinct := true, false
		for i, v := range values {
			match = match && seen[i] == v
			distinct = distinct || (i > 0 && v != values[0])
		}
		if match && strings.HasSuffix(name, "_"+routine.SanitizeName(field)) {
			return name
		}
		if match && distinct {
			fallback, fallbackCount = name, fallbackCount+1
		}
	}
	if fallbackCount == 1 {
		return fallback
	}
	return ""
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}
