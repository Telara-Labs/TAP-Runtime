package codegen

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/routine"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// ProgramGraph is a proposed executable shape, not a claim about the user's
// intent. It contains parameter roles and result bindings, never the recorded
// resource values. Problems name the evidence that is still missing before
// code can be offered for private acceptance.
type ProgramGraph struct {
	CandidateID string         `json:"candidate_id"`
	Executions  int            `json:"executions"`
	Sessions    int            `json:"sessions"`
	Inputs      []ProgramInput `json:"inputs"`
	Steps       []ProgramStep  `json:"steps"`
	Problems    []string       `json:"problems,omitempty"`
	// Cautions do not block generation; they are shown on the card and in
	// the README (an effect the trace cannot show, treated as write).
	Cautions          []string           `json:"cautions,omitempty"`
	Sources           []string           `json:"sources"`
	InlineFileReplace *InlineFileReplace `json:"inline_file_replace,omitempty"`
}

type InlineFileReplace struct {
	Embedded bool `json:"embedded"`
}

type ProgramInput struct {
	Name              string              `json:"name"`
	Type              string              `json:"type"`
	List              bool                `json:"list,omitempty"`
	Optional          bool                `json:"optional,omitempty"`
	Allowed           []string            `json:"allowed,omitempty"`
	RequiredWhenInput string              `json:"required_when_input,omitempty"`
	RequiredWhenValue string              `json:"required_when_value,omitempty"`
	Source            string              `json:"source"`
	Fields            []ProgramInputField `json:"fields,omitempty"`
	// ItemProfiles are observed combinations of optional fields within each list item.
	ItemProfiles [][]string `json:"item_profiles,omitempty"`
}

type ProgramInputField struct {
	Name     string   `json:"name"`
	Path     []string `json:"path"`
	Type     string   `json:"type"`
	Optional bool     `json:"optional,omitempty"`
}

type ProgramStep struct {
	Role     string              `json:"role"`
	Tool     string              `json:"tool"`
	Binding  *ProgramToolBinding `json:"binding,omitempty"`
	Command  string              `json:"command,omitempty"`
	Pipeline []ProgramCommand    `json:"pipeline,omitempty"`
	Effect   string              `json:"effect"`
	// WhenInput and WhenValue select this step through a required caller
	// choice. The same choice guards any later step that reads its result.
	WhenInput      string       `json:"when_input,omitempty"`
	WhenValue      string       `json:"when_value,omitempty"`
	Loop           string       `json:"loop,omitempty"`             // input name, when for_each
	LoopResultStep int          `json:"loop_result_step,omitempty"` // one-based earlier step
	LoopResultPath string       `json:"loop_result_path,omitempty"` // collection in that result
	Args           []ProgramArg `json:"args"`
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

var IndexedResultPath = regexp.MustCompile(`\[[0-9]+\]`)

type ObservedOp struct {
	Node   retrieval.SpanNode             `json:"-"`
	Role   string                         `json:"-"`
	Fields map[string]trace.ObservedField `json:"-"`
}

type ObservedTrace struct {
	Span   model.SpanProposal `json:"-"`
	Groups [][]ObservedOp     `json:"-"`
}

type ObservedInputVector struct {
	Values   []string `json:"-"`
	TypeName string   `json:"-"`
}

// SynthesizeProgramGraph uses the exact calls named by a recurring logic
// candidate. Re-reading source calls and their hashes prevents a stale report
// from silently generating a program from different history. The classifier
// is deliberately action-agnostic: it reads operation names, argument trees,
// typed slots and structured result paths, not Jira/GitLab special cases.
// DeclaredEffect is the effect a generated program declares for a recorded
// one: an effect the trace cannot show is declared a write.
func DeclaredEffect(effect string) string {
	if effect != "read" {
		return "write"
	}
	return effect
}

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
	// Choice evidence comes from the whole corpus, not only the members.
	choices := trace.NewChoices(sessions)
	graph := &ProgramGraph{CandidateID: c.ID, Executions: c.Executions, Sessions: c.Sessions}
	var traces []ObservedTrace
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
		nodes := retrieval.BuildSpanNodes(s, p.Request, calls, byCall, choices)
		byOrdinal := map[int]retrieval.SpanNode{}
		for _, n := range nodes {
			byOrdinal[n.Ordinal] = n
		}
		var ops []ObservedOp
		for i, ordinal := range p.Calls {
			n, ok := byOrdinal[ordinal]
			if !ok || i >= len(p.CallHashes) || retrieval.SpanCallHash(n.Call) != p.CallHashes[i] {
				return nil, fmt.Errorf("span %s source call %d changed", id, ordinal)
			}
			ops = append(ops, ObservedOp{Node: n, Role: retrieval.LogicRole(retrieval.SpanActionRole(n)), Fields: trace.ObservedArgs(n.Call)})
		}
		if len(ops) == 0 {
			continue
		}
		tr := ObservedTrace{Span: p}
		for _, op := range ops {
			if len(tr.Groups) == 0 || tr.Groups[len(tr.Groups)-1][0].Role != op.Role {
				tr.Groups = append(tr.Groups, []ObservedOp{op})
			} else {
				tr.Groups[len(tr.Groups)-1] = append(tr.Groups[len(tr.Groups)-1], op)
			}
		}
		graph.Sources = append(graph.Sources, p.ID)
		traces = append(traces, tr)
	}
	if len(traces) == 0 {
		return nil, fmt.Errorf("candidate %s has no readable executions", c.ID)
	}
	sort.Strings(graph.Sources)
	if traces[0].Span.CodeShape != "" {
		SynthesizeInlineFileReplace(graph, traces)
		return graph, nil
	}
	// One candidate may carry several overlapping slices of a session. Shape
	// mismatches are explicit; they are not repaired by dropping an action.
	roles := make([]string, len(traces[0].Groups))
	for i, group := range traces[0].Groups {
		roles[i] = group[0].Role
	}
	for _, tr := range traces[1:] {
		if len(tr.Groups) != len(roles) {
			graph.Problems = append(graph.Problems, "executions have different action boundaries")
			continue
		}
		for i, group := range tr.Groups {
			if group[0].Role != roles[i] {
				graph.Problems = append(graph.Problems, "executions have different action order")
				break
			}
		}
	}
	if len(graph.Problems) > 0 {
		return graph, nil
	}
	inputVectors := map[string]ObservedInputVector{}
	for step, role := range roles {
		first := traces[0].Groups[step][0]
		ps := ProgramStep{Role: role, Tool: first.Node.Call.Tool, Effect: first.Node.Effect}
		if ps.Tool == "shell" {
			plan, err := shellparse.ProgramShellPlan(first.Node.Call.Command)
			if err != nil {
				graph.Problems = append(graph.Problems, fmt.Sprintf("step %d: %v", step+1, err))
			} else {
				if len(plan) > 1 && len(first.Node.Steps) != len(plan) {
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
					if j < len(first.Node.Steps) {
						effect = trace.StepEffect(first.Node.Steps[j])
					}
					if effect != "read" && effect != "write" {
						// As for a tool: an effect the trace cannot show is
						// declared a write, never guessed to be a read.
						graph.Cautions = append(graph.Cautions, fmt.Sprintf("step %d stage %d effect is not declared by the program; treated as write (asks before each call)", step+1, j+1))
						effect = "write"
					}
					ps.Pipeline = append(ps.Pipeline, ProgramCommand{Name: words[0], Effect: effect, Connector: stage.Connector})
				}
			}
		} else if first.Node.Call.MCPServer != "" && first.Node.Call.MCPTool != "" {
			ps.Binding = &ProgramToolBinding{Server: first.Node.Call.MCPServer, Tool: first.Node.Call.MCPTool}
		} else {
			graph.Problems = append(graph.Problems, fmt.Sprintf("step %d lacks an exact MCP server/tool binding", step+1))
		}
		if ps.Tool != "shell" && !strings.HasPrefix(ps.Tool, "mcp:") {
			graph.Problems = append(graph.Problems, fmt.Sprintf("step %d has no resolved MCP action binding", step+1))
		}
		if ps.Effect != "read" && ps.Effect != "write" {
			// The trace does not show what the tool does. Declare it a write:
			// the runner asks before every call, and its binder refuses a tool
			// whose own annotation claims more. Never guess that it only reads.
			graph.Cautions = append(graph.Cautions, fmt.Sprintf("step %d effect is not declared by the tool; treated as write (asks before each call)", step+1))
			ps.Effect = "write"
		}
		loop := false
		for _, tr := range traces {
			for _, rep := range tr.Span.Composition.Repetition {
				if retrieval.LogicRole(rep.Action) == role && rep.Kind == "for_each" {
					loop = true
				}
			}
			for _, op := range tr.Groups[step] {
				if op.Node.Call.Outcome == trace.OutcomeFailed {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d failed in a source execution", step+1))
				}
				if op.Node.Call.Tool != ps.Tool || op.Role != ps.Role {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d uses incompatible actions", step+1))
				}
				if ps.Binding != nil && (op.Node.Call.MCPServer != ps.Binding.Server || op.Node.Call.MCPTool != ps.Binding.Tool) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d uses incompatible MCP bindings", step+1))
				}
				if ps.Tool == "shell" {
					plan, err := shellparse.ProgramShellPlan(op.Node.Call.Command)
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
								if j >= len(op.Node.Steps) || DeclaredEffect(trace.StepEffect(op.Node.Steps[j])) != ps.Pipeline[j].Effect {
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
				if len(tr.Groups[step]) > 1 && !ObservedForEach(tr.Span, role) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d repeats without item-loop evidence", step+1))
				}
			}
		} else {
			for _, tr := range traces {
				if len(tr.Groups[step]) != 1 {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d repeats without a supported loop", step+1))
				}
			}
		}
		fields := map[string]trace.ObservedField{}
		for _, tr := range traces {
			for _, op := range tr.Groups[step] {
				for path, field := range op.Fields {
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
				if len(tr.Groups) <= step {
					shapeOK = false
					break
				}
				seen := map[string]bool{}
				present := 0
				traceValue := ""
				for _, op := range tr.Groups[step] {
					f, ok := op.Fields[path]
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
				if present != 0 && present != len(tr.Groups[step]) {
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
			resultStep, resultPath, resultOK := ObservedResultBinding(traces, step, path)
			listStep, listPath, itemPath, listOK := ObservedCollectionBinding(traces, step, path)
			selection, selectionOK := ObservedUniqueSelection(traces, step, path)
			indexedCollection, indexedCollectionOK := ObservedIndexedCollectionBinding(traces, step, path)
			switch {
			case choices.Selector(first.Node.Call, path):
				if optional || !AllSame(values) {
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
					name := ProgramInputName(step, base.Path, true) + "_source_indexes"
					if ps.Loop != "" && ps.Loop != name {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d item fields use different selection lists", step+1))
					}
					ps.Loop = name
					ps.DistinctLoopSelections = true
					arg.Value = ProgramValue{Kind: "collection_index_item", Step: indexedCollection.Step, CollectionPath: indexedCollection.Collection, ResultPath: indexedCollection.Item, Input: name}
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: "integer", List: true, Source: fmt.Sprintf("caller selects positions from step %d result%s", indexedCollection.Step, indexedCollection.Collection)})
					break
				}
				if optional || resultOK || PossiblePriorResult(traces, step, path) {
					graph.Problems = append(graph.Problems, fmt.Sprintf("step %d item %s comes from an earlier result; collection binding is not determined", step+1, path))
				}
				name := ProgramInputName(step, base.Path, true)
				ps.Loop = name
				arg.Value = ProgramValue{Kind: "item", Input: name}
				graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: base.TypeName, List: true, Source: "supplied at invocation"})
			case !loop && !optional && (!resultOK || IndexedResultPath.MatchString(resultPath)) && selectionOK:
				name := fmt.Sprintf("step_%d_select_%s", step+1, routine.SanitizeName(selection.PredicatePath))
				arg.Value = ProgramValue{Kind: "selected_result", Step: selection.Step, ResultPath: selection.ItemPath,
					CollectionPath: selection.CollectionPath, PredicatePath: selection.PredicatePath, Input: name}
				found := false
				for _, input := range graph.Inputs {
					if input.Name == name {
						found = true
						break
					}
				}
				if !found {
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: selection.PredicateType, Source: "supplied at invocation (unique result selection)"})
				}
			case !loop && !optional && indexedCollectionOK:
				name := ProgramInputName(step, base.Path, false) + "_source_index"
				arg.Value = ProgramValue{Kind: "collection_index", Step: indexedCollection.Step, CollectionPath: indexedCollection.Collection, ResultPath: indexedCollection.Item, Input: name}
				graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: "integer", Source: fmt.Sprintf("caller selects one position from step %d result%s", indexedCollection.Step, indexedCollection.Collection)})
			case resultOK && !optional && !IndexedResultPath.MatchString(resultPath):
				producer := graph.Steps[resultStep-1]
				if producer.Loop != "" || producer.LoopResultStep != 0 {
					if loop {
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d argument %s selects one result of a loop without a determined per-item join", step+1, path))
						break
					}
					name := ProgramInputName(step, base.Path, false) + "_source_index"
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: "integer", Source: fmt.Sprintf("caller selects one result from step %d by zero-based position", resultStep)})
					arg.Value = ProgramValue{Kind: "indexed_result", Step: resultStep, ResultPath: resultPath, Input: name}
				} else {
					arg.Value = ProgramValue{Kind: "result", Step: resultStep, ResultPath: resultPath}
				}
			case PossiblePriorResult(traces, step, path) || resultOK:
				graph.Problems = append(graph.Problems, fmt.Sprintf("step %d argument %s appears to use an earlier result but its path is not determined", step+1, path))
			default:
				name := ProgramInputName(step, base.Path, false)
				prior := ""
				if !optional && stableAcrossIterations {
					prior = SameInputVector(inputVectors, stableByTrace, base.TypeName, base.Path[len(base.Path)-1])
				}
				if prior != "" {
					name = prior
				} else {
					graph.Inputs = append(graph.Inputs, ProgramInput{Name: name, Type: base.TypeName, Optional: optional, Source: "supplied at invocation"})
					if !optional && stableAcrossIterations {
						inputVectors[name] = ObservedInputVector{Values: stableByTrace, TypeName: base.TypeName}
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
				if ObservedDistinctArgumentValues(traces, step, strings.Join(a.Path, "/"), strings.Join(b.Path, "/")) {
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
				MergeProgramItemFields(graph, &ps, step)
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
					if _, present := tr.Groups[step][0].Fields[strings.Join(arg.Path, "/")]; present {
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
	graph.Problems = UniqueStrings(graph.Problems)
	return graph, nil
}

func MergeProgramItemFields(graph *ProgramGraph, ps *ProgramStep, step int) {
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

func ObservedForEach(p model.SpanProposal, role string) bool {
	for _, r := range p.Composition.Repetition {
		if retrieval.LogicRole(r.Action) == role && r.Kind == "for_each" {
			return true
		}
	}
	return false
}

func AllSame(values []string) bool {
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

func ObservedResultBinding(traces []ObservedTrace, step int, path string) (int, string, bool) {
	producer, resultPath := -1, ""
	for _, tr := range traces {
		for _, op := range tr.Groups[step] {
			value := op.Fields[path].Value
			foundStep, foundPath := -1, ""
			for prior := step - 1; prior >= 0; prior-- {
				for _, parent := range tr.Groups[prior] {
					for i, id := range parent.Node.Call.OutIDs {
						if id == value && i < len(parent.Node.Call.OutPaths) && parent.Node.Call.OutPaths[i] != "" && parent.Node.Call.OutPaths[i] != "*" {
							if foundStep >= 0 {
								return 0, "", false // ambiguous producer
							}
							foundStep, foundPath = prior, parent.Node.Call.OutPaths[i]
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

func ObservedDistinctArgumentValues(traces []ObservedTrace, step int, left, right string) bool {
	if len(traces) == 0 {
		return false
	}
	for _, tr := range traces {
		for _, op := range tr.Groups[step] {
			a, aOK := op.Fields[left]
			b, bOK := op.Fields[right]
			if !aOK || !bOK || a.Value == b.Value {
				return false
			}
		}
	}
	return true
}

type UniqueSelection struct {
	Step           int    `json:"-"`
	CollectionPath string `json:"-"`
	ItemPath       string `json:"-"`
	PredicatePath  string `json:"-"`
	PredicateType  string `json:"-"`
}

type SelectionEvidence struct {
	Selection UniqueSelection `json:"-"`
	FirstAll  bool            `json:"-"`
	LastAll   bool            `json:"-"`
}

// ObservedUniqueSelection learns only a parameterized equality predicate.
// A selected field value must be stable and unique in every complete source
// list, and the selected position must vary so a fixed first/last choice does
// not explain all evidence. The literal value is never placed in the graph.
func ObservedUniqueSelection(traces []ObservedTrace, step int, path string) (UniqueSelection, bool) {
	if len(traces) < 2 {
		return UniqueSelection{}, false
	}
	sessions := map[string]bool{}
	var common map[string]SelectionEvidence
	for _, tr := range traces {
		sessions[tr.Span.Client+"\x00"+tr.Span.Session] = true
		if len(tr.Groups[step]) != 1 {
			return UniqueSelection{}, false
		}
		value, ok := tr.Groups[step][0].Fields[path]
		if !ok {
			return UniqueSelection{}, false
		}
		wanted := trace.ResultValueDigest(value.Value)
		matches := map[string]SelectionEvidence{}
		for prior := 0; prior < step; prior++ {
			if len(tr.Groups[prior]) != 1 {
				continue
			}
			call := tr.Groups[prior][0].Node.Call
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
						candidate := UniqueSelection{prior + 1, collection.Path, itemPath, predicatePath, predicateField.Type}
						key := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s", candidate.Step, candidate.CollectionPath, candidate.ItemPath, candidate.PredicatePath, candidate.PredicateType, selected)
						matches[key] = SelectionEvidence{candidate, index == 0, index == collection.Count-1}
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
				existing.FirstAll = existing.FirstAll && current.FirstAll
				existing.LastAll = existing.LastAll && current.LastAll
				common[key] = existing
			}
		}
		if len(common) == 0 {
			return UniqueSelection{}, false
		}
	}
	if len(sessions) < 2 {
		return UniqueSelection{}, false
	}
	var chosen UniqueSelection
	count := 0
	for _, evidence := range common {
		if evidence.FirstAll || evidence.LastAll {
			continue
		}
		chosen = evidence.Selection
		count++
	}
	return chosen, count == 1
}

// ObservedCollectionBinding proves that every item in an earlier result list
// was consumed once, in order. A subset, reordered list, malformed result,
// or ambiguous source is not a safe program to synthesize.
func ObservedCollectionBinding(traces []ObservedTrace, step int, path string) (int, string, string, bool) {
	var common map[string]CollectionBinding
	for _, tr := range traces {
		group := tr.Groups[step]
		values := make([]trace.ObservedField, 0, len(group))
		for _, op := range group {
			field, ok := op.Fields[path]
			if !ok {
				return 0, "", "", false
			}
			values = append(values, field)
		}
		matches := map[string]CollectionBinding{}
		for prior := 0; prior < step; prior++ {
			if len(tr.Groups[prior]) == len(values) {
				path, ok := ObservedParallelResultPath(tr.Groups[prior], values)
				if ok {
					match := CollectionBinding{prior + 1, "", path}
					matches[fmt.Sprintf("%d\x00%s\x00%s", prior+1, "", path)] = match
				}
			}
			if len(tr.Groups[prior]) != 1 {
				continue
			}
			call := tr.Groups[prior][0].Node.Call
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
						match := CollectionBinding{prior + 1, collection.Path, itemPath}
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
		return match.Step, match.Collection, match.Item, true
	}
	return 0, "", "", false
}

// ObservedParallelResultPath proves an index-preserving join between two
// repeated steps. The producer's generated result is itself a list even when
// each individual call returned a scalar object.
func ObservedParallelResultPath(producers []ObservedOp, values []trace.ObservedField) (string, bool) {
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
		for j, id := range producer.Node.Call.OutIDs {
			if id != value || j >= len(producer.Node.Call.OutPaths) {
				continue
			}
			candidate := producer.Node.Call.OutPaths[j]
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

type CollectionBinding struct {
	Step       int    `json:"-"`
	Collection string `json:"-"`
	Item       string `json:"-"`
}

// ObservedIndexedCollectionBinding proves that each consumed value is one
// distinct item of a complete earlier result list. It does not infer why the
// agent chose those items: the generated program asks the caller for their
// zero-based positions. Multiple possible source fields remain unresolved.
func ObservedIndexedCollectionBinding(traces []ObservedTrace, step int, path string) (CollectionBinding, bool) {
	var common map[string]CollectionBinding
	for _, tr := range traces {
		if step >= len(tr.Groups) {
			return CollectionBinding{}, false
		}
		values := make([]trace.ObservedField, 0, len(tr.Groups[step]))
		for _, op := range tr.Groups[step] {
			value, ok := op.Fields[path]
			if !ok {
				return CollectionBinding{}, false
			}
			values = append(values, value)
		}
		matches := map[string]CollectionBinding{}
		for prior := 0; prior < step; prior++ {
			if len(tr.Groups[prior]) != 1 {
				continue
			}
			call := tr.Groups[prior][0].Node.Call
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
						binding := CollectionBinding{Step: prior + 1, Collection: collection.Path, Item: itemPath}
						matches[fmt.Sprintf("%d\x00%s\x00%s", binding.Step, binding.Collection, binding.Item)] = binding
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
			return CollectionBinding{}, false
		}
	}
	if len(common) != 1 {
		return CollectionBinding{}, false
	}
	for _, binding := range common {
		return binding, true
	}
	return CollectionBinding{}, false
}

func PossiblePriorResult(traces []ObservedTrace, step int, path string) bool {
	for _, tr := range traces {
		for _, op := range tr.Groups[step] {
			field, ok := op.Fields[path]
			if !ok {
				continue
			}
			value := field.Value
			digest := trace.ResultValueDigest(value)
			for prior := 0; prior < step; prior++ {
				for _, parent := range tr.Groups[prior] {
					for _, id := range parent.Node.Call.OutIDs {
						if id == value {
							return true
						}
					}
					// OutIDs covers only the first 64 identifiers. A complete
					// bounded collection can prove a later item was returned as
					// well, even when its value was beyond that preview. An exact
					// argument echo on this call can instead be a shared caller
					// input and is handled by input-vector alignment.
					if priorField, echoed := parent.Fields[path]; echoed && priorField.TypeName == field.TypeName && priorField.Value == value {
						continue
					}
					for _, collection := range parent.Node.Call.OutCollections {
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

func ProgramInputName(step int, path []string, list bool) string {
	name := routine.SanitizeName(strings.Join(path, "_"))
	if list {
		name += "s"
	}
	return "step_" + strconv.Itoa(step+1) + "_" + name
}

func SameInputVector(prior map[string]ObservedInputVector, values []string, typeName, field string) string {
	names := make([]string, 0, len(prior))
	for name := range prior {
		names = append(names, name)
	}
	sort.Strings(names)
	fallback, fallbackCount := "", 0
	for _, name := range names {
		vector := prior[name]
		seen := vector.Values
		if vector.TypeName != typeName {
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

func UniqueStrings(in []string) []string {
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
