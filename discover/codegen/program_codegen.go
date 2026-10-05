package codegen

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
	"github.com/Telara-Labs/TAP-Runtime/discover/routine"
	"github.com/Telara-Labs/TAP-Runtime/discover/shellparse"
)

// GeneratedPackage is a private, exact draft generated without a model. The
// source is a contained TAP guest program: all effects still go through the
// host's declared tool bindings and normal runtime approval path.
type GeneratedPackage struct {
	Graph    *ProgramGraph
	Manifest *manifest.Manifest
	Files    map[string][]byte
	Digest   string
}

// GenerateProgramPackage refuses an underdetermined graph. A review may still
// show that graph and its problems, but it cannot offer Accept until every
// value and operation binding has a known source.
func GenerateProgramPackage(g *ProgramGraph) (*GeneratedPackage, error) {
	if g == nil {
		return nil, errors.New("no program graph")
	}
	if len(g.Problems) > 0 {
		return nil, fmt.Errorf("program is not determined: %s", strings.Join(g.Problems, "; "))
	}
	if len(g.Steps) == 0 {
		return nil, errors.New("program has no steps")
	}
	if g.InlineFileReplace != nil {
		return GenerateInlineFileReplace(g)
	}
	inputTypes := map[string]string{}
	inputSpecs := map[string]ProgramInput{}
	for _, input := range g.Inputs {
		if input.Name == "" {
			return nil, errors.New("program has an unnamed input")
		}
		if _, exists := inputSpecs[input.Name]; exists {
			return nil, fmt.Errorf("program has duplicate input %q", input.Name)
		}
		inputTypes[input.Name] = input.Type
		inputSpecs[input.Name] = input
	}
	for _, input := range g.Inputs {
		if input.RequiredWhenInput == "" {
			if input.RequiredWhenValue != "" {
				return nil, fmt.Errorf("input %q has a branch value without a selector", input.Name)
			}
			continue
		}
		selector, ok := inputSpecs[input.RequiredWhenInput]
		if !input.Optional || !ok || selector.Optional || selector.List || selector.Type != "string" || !containsString(selector.Allowed, input.RequiredWhenValue) {
			return nil, fmt.Errorf("input %q has an unresolved conditional requirement", input.Name)
		}
	}
	for i, step := range g.Steps {
		if step.WhenInput != "" {
			choice, ok := inputSpecs[step.WhenInput]
			if !ok || choice.Optional || choice.List || choice.Type != "string" || step.WhenValue == "" || !containsString(choice.Allowed, step.WhenValue) {
				return nil, fmt.Errorf("step %d has an unresolved branch choice", i+1)
			}
		} else if step.WhenValue != "" {
			return nil, fmt.Errorf("step %d has a branch value without an input", i+1)
		}
		if step.SameItemAs != 0 {
			// A follower sits right after its leader or another follower of
			// it, shares the leader's caller list, and has no loop of its own.
			lead := step.SameItemAs
			prev := g.Steps[i-1]
			if lead < 1 || lead > i || g.Steps[lead-1].Loop == "" || g.Steps[lead-1].SameItemAs != 0 || step.Loop != g.Steps[lead-1].Loop ||
				step.LoopResultStep != 0 || step.Tool == "shell" || step.WhenInput != "" || !(i == lead || prev.SameItemAs == lead) {
				return nil, fmt.Errorf("step %d joins an invalid loop", i+1)
			}
		}
		indexedInputs := map[string]bool{}
		for _, arg := range step.Args {
			v := arg.Value
			if v.Kind == "iteration_result" {
				producer := v.Step
				lead := step.SameItemAs
				if lead == 0 || producer < lead || producer > i || (producer != lead && g.Steps[producer-1].SameItemAs != lead) {
					return nil, fmt.Errorf("step %d reads a result outside its own loop iteration", i+1)
				}
				if arg.Optional {
					return nil, fmt.Errorf("step %d has an optional iteration result", i+1)
				}
				continue
			}
			if step.SameItemAs != 0 && (arg.Optional && arg.Value.Kind != "item" || v.Kind == "collection_index" || v.Kind == "collection_index_item" || v.Kind == "indexed_result") {
				return nil, fmt.Errorf("step %d has an unsupported argument inside a joined loop", i+1)
			}
			if (v.Kind == "result" || v.Kind == "indexed_result") && v.Step > 0 && v.Step <= i {
				producer := g.Steps[v.Step-1]
				if producer.WhenInput != "" && (producer.WhenInput != step.WhenInput || producer.WhenValue != step.WhenValue) {
					return nil, fmt.Errorf("step %d uses a result from another branch", i+1)
				}
			}
			if v.Kind == "collection_index" || v.Kind == "collection_index_item" {
				input, ok := inputSpecs[v.Input]
				// Optional only in a branch program, where it is required
				// whenever this step's branch is the one selected.
				branchRequired := input.RequiredWhenInput != "" && input.RequiredWhenInput == step.WhenInput && input.RequiredWhenValue == step.WhenValue
				if v.Step < 1 || v.Step > i || !ok || input.Type != "integer" || (input.Optional && !branchRequired) ||
					(v.Kind == "collection_index_item" && (!input.List || step.Loop != v.Input)) ||
					(v.Kind == "collection_index" && (input.List || step.Loop != "" || step.LoopResultStep != 0)) ||
					g.Steps[v.Step-1].Loop != "" || g.Steps[v.Step-1].LoopResultStep != 0 {
					return nil, fmt.Errorf("step %d has an unresolved collection index binding", i+1)
				}
				continue
			}
			if v.Kind != "result" && v.Kind != "indexed_result" {
				continue
			}
			if v.Step < 1 || v.Step > i {
				return nil, fmt.Errorf("step %d result binding has no earlier producer", i+1)
			}
			producer := g.Steps[v.Step-1]
			collection := producer.Loop != "" || producer.LoopResultStep != 0
			if v.Kind == "result" && collection {
				return nil, fmt.Errorf("step %d treats looped step %d as a scalar result", i+1, v.Step)
			}
			if v.Kind == "indexed_result" {
				if !collection || inputTypes[v.Input] != "integer" || step.Loop != "" || step.LoopResultStep != 0 {
					return nil, fmt.Errorf("step %d has an unresolved loop-result selection", i+1)
				}
				indexedInputs[v.Input] = true
			}
		}
		for _, pair := range step.DistinctResultInputs {
			if len(pair) != 2 || pair[0] == pair[1] || !indexedInputs[pair[0]] || !indexedInputs[pair[1]] {
				return nil, fmt.Errorf("step %d has an invalid distinct-result constraint", i+1)
			}
		}
		if step.DistinctLoopSelections {
			found := false
			for _, arg := range step.Args {
				if arg.Value.Kind == "collection_index_item" && arg.Value.Input == step.Loop {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("step %d has no loop selection for its distinctness guard", i+1)
			}
		}
	}
	name := "discovered-" + strings.TrimPrefix(g.CandidateID, "lc_")
	if !pack.SkillName.MatchString(name) {
		return nil, fmt.Errorf("candidate %q cannot name a package", g.CandidateID)
	}
	m := &manifest.Manifest{APIVersion: manifest.APIVersion, Kind: "Primitive",
		Metadata:  manifest.Metadata{Publisher: "local.discover", Name: name, Version: "0.1.0", Description: "Generated from local execution evidence; review before use."},
		Execution: manifest.Execution{Runtime: manifest.RuntimeWasm, Entrypoint: "main.py"}}
	props := map[string]any{}
	var required []string
	for _, in := range g.Inputs {
		typ := ProgramJSONType(in.Type)
		schema := map[string]any{"type": typ}
		if len(in.Allowed) > 0 {
			if typ != "string" || in.List {
				return nil, fmt.Errorf("input %q has unsupported allowed values", in.Name)
			}
			schema["enum"] = in.Allowed
		}
		if in.Default != "" {
			if typ != "string" || in.List || in.Optional {
				return nil, fmt.Errorf("input %q has an unsupported default", in.Name)
			}
			schema["default"] = in.Default
		}
		if in.List {
			schema = map[string]any{"type": "array", "items": map[string]any{"type": typ}}
			if len(in.Fields) > 0 {
				itemProps := map[string]any{}
				itemRequired := make([]string, 0, len(in.Fields))
				for _, field := range in.Fields {
					fieldSchema := map[string]any{"type": ProgramJSONType(field.Type)}
					if field.Default != "" {
						if ProgramJSONType(field.Type) != "string" || field.Optional {
							return nil, fmt.Errorf("input %q field %q has an unsupported default", in.Name, field.Name)
						}
						fieldSchema["default"] = field.Default
					}
					itemProps[field.Name] = fieldSchema
					if !field.Optional && field.Default == "" {
						itemRequired = append(itemRequired, field.Name)
					}
				}
				sort.Strings(itemRequired)
				schema = map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": itemProps, "required": itemRequired, "additionalProperties": false}}
			}
		}
		props[in.Name] = schema
		if !in.Optional && in.Default == "" {
			required = append(required, in.Name)
		}
	}
	sort.Strings(required)
	m.Interface = &manifest.Interface{InputSchema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false},
		OutputSchema: map[string]any{"type": "object"}}
	var code strings.Builder
	code.WriteString("import json\nimport sys\n\n")
	code.WriteString("def _at(value, path):\n    for key in path:\n        value = value[key]\n    return value\n\n")
	code.WriteString("def _one(items, path, wanted):\n    if not isinstance(items, list):\n        raise ValueError('selection source is not a list')\n    matches = [item for item in items if _at(item, path) == wanted]\n    if len(matches) != 1:\n        raise ValueError('selection requires exactly one matching item')\n    return matches[0]\n\n")
	code.WriteString("def _indexed(items, index):\n    if not isinstance(items, list) or type(index) is not int or index < 0 or index >= len(items):\n        raise ValueError('result selection index is out of range')\n    return items[index]\n\n")
	code.WriteString("def _typed(value, kind):\n    if kind == 'string': return isinstance(value, str)\n    if kind == 'integer': return type(value) is int\n    if kind == 'number': return isinstance(value, (int, float)) and not isinstance(value, bool)\n    if kind == 'boolean': return isinstance(value, bool)\n    if kind == 'array': return isinstance(value, list)\n    if kind == 'object': return isinstance(value, dict)\n    return False\n\n")
	code.WriteString("if len(sys.argv) != 2:\n    raise ValueError('pass one JSON object of typed inputs')\n")
	code.WriteString("inputs = json.loads(sys.argv[1])\nif not isinstance(inputs, dict):\n    raise ValueError('inputs must be an object')\n")
	for _, in := range g.Inputs {
		q := strconv.Quote(in.Name)
		if !in.Optional && in.Default == "" {
			code.WriteString("if " + q + " not in inputs:\n    raise ValueError('missing input ' + " + q + ")\n")
		}
		if in.RequiredWhenInput != "" {
			code.WriteString("if inputs[" + strconv.Quote(in.RequiredWhenInput) + "] == " + strconv.Quote(in.RequiredWhenValue) + " and " + q + " not in inputs:\n    raise ValueError('missing input ' + " + q + ")\n")
		}
		guard := ""
		if in.Optional || in.Default != "" {
			guard = q + " in inputs and "
		}
		if in.List {
			code.WriteString("if " + guard + "not isinstance(inputs[" + q + "], list):\n    raise ValueError('input ' + " + q + " + ' must be a list')\n")
			code.WriteString("if " + guard + "not all(_typed(item, " + strconv.Quote(ProgramJSONType(in.Type)) + ") for item in inputs[" + q + "]):\n    raise ValueError('input ' + " + q + " + ' has an item of the wrong type')\n")
			for _, field := range in.Fields {
				fq := strconv.Quote(field.Name)
				check := fq + " in item and _typed(item[" + fq + "], " + strconv.Quote(ProgramJSONType(field.Type)) + ")"
				if field.Optional || field.Default != "" {
					check = fq + " not in item or _typed(item[" + fq + "], " + strconv.Quote(ProgramJSONType(field.Type)) + ")"
				}
				code.WriteString("if " + guard + "not all(" + check + " for item in inputs[" + q + "]):\n    raise ValueError('input ' + " + q + " + ' has a missing or mistyped field ' + " + fq + ")\n")
			}
			if len(in.ItemProfiles) > 0 {
				var optional []string
				for _, field := range in.Fields {
					if field.Optional {
						optional = append(optional, field.Name)
					}
				}
				sort.Strings(optional)
				names, _ := json.Marshal(optional)
				profiles, _ := json.Marshal(in.ItemProfiles)
				code.WriteString("if " + guard + "any((set(item) & set(" + string(names) + ")) not in [set(profile) for profile in " + string(profiles) + "] for item in inputs[" + q + "]):\n    raise ValueError('input ' + " + q + " + ' has an unobserved optional item field combination')\n")
			}
			if len(in.Fields) > 0 {
				var names []string
				for _, field := range in.Fields {
					names = append(names, field.Name)
				}
				allowed, _ := json.Marshal(names)
				code.WriteString("if " + guard + "any(set(item) - set(" + string(allowed) + ") for item in inputs[" + q + "]):\n    raise ValueError('input ' + " + q + " + ' has an unknown item field')\n")
			}
		} else {
			code.WriteString("if " + guard + "not _typed(inputs[" + q + "], " + strconv.Quote(ProgramJSONType(in.Type)) + "):\n    raise ValueError('input ' + " + q + " + ' has the wrong type')\n")
			if len(in.Allowed) > 0 {
				allowed, _ := json.Marshal(in.Allowed)
				code.WriteString("if " + guard + "inputs[" + q + "] not in " + string(allowed) + ":\n    raise ValueError('input ' + " + q + " + ' is not an allowed choice')\n")
			}
		}
	}
	// Validate caller-selected result roles before any tool effects. A
	// caller-supplied loop has a known cardinality at invocation; a loop over
	// an earlier tool result is also guarded by _indexed after that result.
	for i, st := range g.Steps {
		for _, arg := range st.Args {
			if arg.Value.Kind != "indexed_result" {
				continue
			}
			producer := g.Steps[arg.Value.Step-1]
			if producer.Loop != "" {
				index, list := strconv.Quote(arg.Value.Input), strconv.Quote(producer.Loop)
				code.WriteString("if inputs[" + index + "] < 0 or inputs[" + index + "] >= len(inputs[" + list + "]):\n    raise ValueError('step " + strconv.Itoa(i+1) + " result selection index is out of range')\n")
			}
		}
		for _, pair := range st.DistinctResultInputs {
			left, right := strconv.Quote(pair[0]), strconv.Quote(pair[1])
			code.WriteString("if inputs[" + left + "] == inputs[" + right + "]:\n    raise ValueError('step " + strconv.Itoa(i+1) + " result selections must be distinct')\n")
		}
	}
	// Defaults are filled in after validation, so a check never mistakes a
	// default for something the caller sent.
	for _, in := range g.Inputs {
		q := strconv.Quote(in.Name)
		if in.Default != "" {
			code.WriteString("inputs.setdefault(" + q + ", " + strconv.Quote(in.Default) + ")\n")
		}
		for _, field := range in.Fields {
			if field.Default != "" {
				code.WriteString("for item in inputs.get(" + q + ", []):\n    item.setdefault(" + strconv.Quote(field.Name) + ", " + strconv.Quote(field.Default) + ")\n")
			}
		}
	}
	code.WriteString("outputs = {}\n")
	for i, st := range g.Steps {
		stepStart := code.Len()
		if st.Tool != "shell" && (!strings.HasPrefix(st.Tool, "mcp:") || st.Tool == "mcp:") {
			return nil, fmt.Errorf("step %d has no declared MCP tool", i+1)
		}
		if st.Tool != "shell" && (st.Binding == nil || st.Binding.Server == "" || st.Binding.Tool == "") {
			return nil, fmt.Errorf("step %d has no exact MCP server/tool binding", i+1)
		}
		alias := "step_" + strconv.Itoa(i+1)
		if len(st.Pipeline) > 0 {
			if st.Tool != "shell" || st.Command != "" || st.Loop != "" || st.LoopResultStep != 0 {
				return nil, fmt.Errorf("step %d pipeline has an unsupported control shape", i+1)
			}
			if len(st.Pipeline) < 2 || len(st.Pipeline) > 8 || st.Pipeline[0].Connector != "" {
				return nil, fmt.Errorf("step %d has an invalid compound command plan", i+1)
			}
			connector := st.Pipeline[1].Connector
			if connector != "pipe" && connector != "and" {
				return nil, fmt.Errorf("step %d has an unsupported shell connector", i+1)
			}
			for _, command := range st.Pipeline[1:] {
				if command.Connector != connector {
					return nil, fmt.Errorf("step %d has mixed shell connectors", i+1)
				}
			}
			for _, arg := range st.Args {
				stage, position := -1, -1
				if len(arg.Path) != 1 {
					return nil, fmt.Errorf("step %d pipeline has a nonpositional argument", i+1)
				}
				if _, err := fmt.Sscanf(arg.Path[0], "pipe_%d_argv_%d", &stage, &position); err != nil || fmt.Sprintf("pipe_%d_argv_%d", stage, position) != arg.Path[0] || stage < 0 || stage >= len(st.Pipeline) || position < 0 {
					return nil, fmt.Errorf("step %d pipeline has an unbound argument %q", i+1, arg.Path[0])
				}
			}
			previous := ""
			for stage, command := range st.Pipeline {
				args, err := ProgramPipelineStageArgs(st, stage)
				if err != nil {
					return nil, fmt.Errorf("step %d pipeline stage %d: %w", i+1, stage+1, err)
				}
				expr, patterns, err := ProgramCommandArgs(ProgramStep{Command: command.Name, Effect: command.Effect, Args: args})
				if err != nil {
					return nil, fmt.Errorf("step %d pipeline stage %d: %w", i+1, stage+1, err)
				}
				m.Commands = append(m.Commands, manifest.Command{Command: command.Name, Args: patterns, Effect: command.Effect})
				name := fmt.Sprintf("_pipe_%d_%d", i+1, stage+1)
				call := "tap.exec(" + strconv.Quote(command.Name) + ", " + expr
				if previous != "" && connector == "pipe" {
					call += ", " + previous + "['stdout']"
				}
				call += ")"
				code.WriteString(name + " = " + call + "\n")
				code.WriteString("if " + name + ".get('exit') != 0:\n    raise RuntimeError('compound command step " + strconv.Itoa(i+1) + " stage " + strconv.Itoa(stage+1) + " exited nonzero')\n")
				previous = name
			}
			code.WriteString("result_" + strconv.Itoa(i+1) + " = " + previous + "\n")
			code.WriteString("outputs[" + strconv.Quote(alias) + "] = result_" + strconv.Itoa(i+1) + "\n")
			wrapChosenStep(&code, stepStart, st)
			continue
		}
		if st.Tool != "shell" {
			tool := strings.TrimPrefix(st.Tool, "mcp:")
			// The transcript does not prove the full live argument/result schema,
			// so do not invent a capability contract. The exact pin is checked
			// against the active client's inventory at run time.
			capability := m.Metadata.Publisher + "/" + routine.CapName(tool) + "@1"
			m.Tools = append(m.Tools, manifest.Tool{Alias: alias, Capability: capability, Effect: st.Effect,
				Pin: &manifest.Pin{Server: st.Binding.Server, Tool: st.Binding.Tool}})
		}
		if st.SameItemAs != 0 {
			continue // called inside its leader's loop
		}
		optionalNames, err := ProgramOptionalNames(st)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		if len(optionalNames) > 0 {
			namesJSON, _ := json.Marshal(optionalNames)
			profilesJSON, _ := json.Marshal(st.OptionalProfiles)
			code.WriteString("_present = {name for name in " + string(namesJSON) + " if name in inputs}\n")
			code.WriteString("if _present not in [set(profile) for profile in " + string(profilesJSON) + "]:\n    raise ValueError('unobserved optional input combination at step " + strconv.Itoa(i+1) + "')\n")
		}
		// Check the entire caller-selected subset after its producer has run
		// and before making any downstream tool call. A late invalid index must
		// not leave a partially applied loop of effects.
		selectionGuards := map[string]bool{}
		for _, arg := range st.Args {
			v := arg.Value
			if v.Kind != "collection_index" && v.Kind != "collection_index_item" {
				continue
			}
			key := fmt.Sprintf("%d\x00%s\x00%s\x00%s", v.Step, v.CollectionPath, v.ResultPath, v.Input)
			if selectionGuards[key] {
				continue
			}
			selectionGuards[key] = true
			collection := "result_" + strconv.Itoa(v.Step)
			if v.CollectionPath != "" {
				path, err := ProgramResultPath(v.CollectionPath)
				if err != nil {
					return nil, fmt.Errorf("step %d selection source: %w", i+1, err)
				}
				collection = "_at(" + collection + ", " + path + ")"
			}
			selector := "inputs[" + strconv.Quote(v.Input) + "]"
			selected := "_indexed(" + collection + ", _selected_index)"
			if v.ResultPath != "" {
				path, err := ProgramResultPath(v.ResultPath)
				if err != nil {
					return nil, fmt.Errorf("step %d selected field: %w", i+1, err)
				}
				selected = "_at(" + selected + ", " + path + ")"
			}
			if v.Kind == "collection_index_item" {
				if st.DistinctLoopSelections {
					code.WriteString("if len(set(" + selector + ")) != len(" + selector + "):\n    raise ValueError('step " + strconv.Itoa(i+1) + " selected positions must be distinct')\n")
				}
				code.WriteString("for _selected_index in " + selector + ":\n    " + selected + "\n")
			} else {
				code.WriteString(strings.Replace(selected, "_selected_index", selector, 1) + "\n")
			}
		}
		expr := ""
		call := ""
		if st.Tool == "shell" {
			var patterns []string
			expr, patterns, err = ProgramCommandArgs(st)
			if err == nil {
				m.Commands = append(m.Commands, manifest.Command{Command: st.Command, Args: patterns, Effect: st.Effect})
				call = "tap.exec(" + strconv.Quote(st.Command) + ", " + expr + ")"
			}
		} else {
			expr, err = ProgramArgsExpression(st.Args)
			call = "tap.call(" + strconv.Quote(alias) + ", " + expr + ")"
		}
		if err != nil {
			return nil, fmt.Errorf("step %d arguments: %w", i+1, err)
		}
		resultName := "result_" + strconv.Itoa(i+1)
		if st.Loop != "" && st.LoopResultStep != 0 {
			return nil, fmt.Errorf("step %d has two loop sources", i+1)
		}
		if st.LoopResultStep != 0 {
			if st.LoopResultStep > i {
				return nil, fmt.Errorf("step %d loop source is not earlier", i+1)
			}
			if st.LoopResultPath == "" {
				producer := g.Steps[st.LoopResultStep-1]
				if producer.Loop == "" && producer.LoopResultStep == 0 {
					return nil, fmt.Errorf("step %d root result source is not a loop", i+1)
				}
			}
			collection := "result_" + strconv.Itoa(st.LoopResultStep)
			if st.LoopResultPath != "" {
				path, err := ProgramResultPath(st.LoopResultPath)
				if err != nil {
					return nil, fmt.Errorf("step %d collection path: %w", i+1, err)
				}
				collection = "_at(" + collection + ", " + path + ")"
			}
			code.WriteString(resultName + " = []\n")
			code.WriteString("for item in " + collection + ":\n")
			code.WriteString("    " + resultName + ".append(" + call + ")\n")
		} else if st.Loop != "" {
			// The leader and the steps joined to it run in one pass per
			// caller item; each step's results are collected in order.
			type member struct {
				n           int
				alias, call string
			}
			members := []member{{i + 1, alias, call}}
			for j := i + 1; j < len(g.Steps) && g.Steps[j].SameItemAs == i+1; j++ {
				expr, err := ProgramArgsExpression(g.Steps[j].Args)
				if err != nil {
					return nil, fmt.Errorf("step %d arguments: %w", j+1, err)
				}
				a := "step_" + strconv.Itoa(j+1)
				members = append(members, member{j + 1, a, "tap.call(" + strconv.Quote(a) + ", " + expr + ")"})
			}
			loopInput := "inputs[" + strconv.Quote(st.Loop) + "]"
			optional := inputSpecs[st.Loop].Optional
			if optional {
				loopInput = "inputs.get(" + strconv.Quote(st.Loop) + ", [])"
			}
			for _, mb := range members {
				code.WriteString("result_" + strconv.Itoa(mb.n) + " = []\n")
				if optional {
					// Keep each completed effect visible if a later item or
					// another follow-up fails after the head has succeeded.
					code.WriteString("outputs[" + strconv.Quote(mb.alias) + "] = result_" + strconv.Itoa(mb.n) + "\n")
				}
			}
			code.WriteString("for item in " + loopInput + ":\n")
			indent := "    "
			if optional {
				code.WriteString("    _step = " + strconv.Quote(alias) + "\n    try:\n")
				indent = "        "
			}
			for k, mb := range members {
				if optional && k > 0 {
					code.WriteString(indent + "_step = " + strconv.Quote(mb.alias) + "\n")
				}
				code.WriteString(indent + "_r_" + strconv.Itoa(mb.n) + " = " + mb.call + "\n")
				code.WriteString(indent + "result_" + strconv.Itoa(mb.n) + ".append(_r_" + strconv.Itoa(mb.n) + ")\n")
			}
			if optional {
				code.WriteString("    except Exception as exc:\n        print(json.dumps({'partial': outputs, 'failed_step': _step, 'error': str(exc)}, sort_keys=True), file=sys.stderr)\n        raise\n")
			}
			for _, mb := range members[1:] {
				code.WriteString("outputs[" + strconv.Quote(mb.alias) + "] = result_" + strconv.Itoa(mb.n) + "\n")
			}
		} else {
			code.WriteString(resultName + " = " + call + "\n")
		}
		if st.Tool == "shell" {
			if st.Loop != "" || st.LoopResultStep != 0 {
				code.WriteString("if any(result.get('exit') != 0 for result in " + resultName + "):\n    raise RuntimeError('command step " + strconv.Itoa(i+1) + " exited nonzero')\n")
			} else {
				code.WriteString("if " + resultName + ".get('exit') != 0:\n    raise RuntimeError('command step " + strconv.Itoa(i+1) + " exited nonzero')\n")
			}
		}
		code.WriteString("outputs[" + strconv.Quote(alias) + "] = " + resultName + "\n")
		wrapChosenStep(&code, stepStart, st)
	}
	code.WriteString("print(json.dumps(outputs, sort_keys=True))\n")
	if problems := m.RunProblems(); len(problems) > 0 {
		return nil, fmt.Errorf("generated manifest is not runnable: %s", strings.Join(problems, "; "))
	}
	var readme strings.Builder
	fmt.Fprintf(&readme, "# %s\n\nGenerated privately from %d disjoint execution(s) in %d session(s), represented by %d local span(s), including overlaps. Review the exact code and declared tool effects before accepting.\n\n", name, g.Executions, g.Sessions, len(g.Sources))
	if len(g.Cautions) > 0 {
		readme.WriteString("## Cautions\n\n")
		for _, c := range g.Cautions {
			fmt.Fprintf(&readme, "- %s\n", c)
		}
		readme.WriteString("\n")
	}
	readme.WriteString("## Inputs\n\n")
	for _, in := range g.Inputs {
		kind := in.Type
		if in.List {
			kind = "list<" + kind + ">"
		}
		if in.Optional {
			kind += " (optional; omitted when absent)"
		}
		if len(in.Allowed) > 0 {
			kind += "; one of " + strings.Join(in.Allowed, ", ")
		}
		if in.RequiredWhenInput != "" {
			kind += fmt.Sprintf("; required when `%s` is `%s`", in.RequiredWhenInput, in.RequiredWhenValue)
		}
		if in.Default != "" {
			kind += fmt.Sprintf(" (default `%s`)", in.Default)
		}
		fmt.Fprintf(&readme, "- `%s`: %s, from %s\n", in.Name, kind, in.Source)
		for _, field := range in.Fields {
			optional := ""
			if field.Optional {
				optional = " (optional)"
			} else if field.Default != "" {
				optional = fmt.Sprintf(" (default `%s`)", field.Default)
			}
			fmt.Fprintf(&readme, "  - `%s`: %s%s -> tool argument `%s`\n", field.Name, field.Type, optional, strings.Join(field.Path, "."))
		}
	}
	readme.WriteString("\n## Ordered calls\n\n")
	for i, st := range g.Steps {
		loop := ""
		if st.Loop != "" {
			loop = " for each item in `" + st.Loop + "`"
		} else if st.LoopResultStep != 0 {
			loop = fmt.Sprintf(" for each item in step %d result%s", st.LoopResultStep, st.LoopResultPath)
		}
		via := st.Tool
		if st.Tool == "shell" {
			via = "command " + st.Command
			if len(st.Pipeline) > 0 {
				var names []string
				for _, command := range st.Pipeline {
					names = append(names, command.Name+" ("+command.Effect+")")
				}
				separator, kind := " | ", "pipeline "
				if st.Pipeline[1].Connector == "and" {
					separator, kind = " && ", "success chain "
				}
				via = kind + strings.Join(names, separator)
			}
		}
		fmt.Fprintf(&readme, "%d. `%s` via `%s` (%s)%s\n", i+1, st.Role, via, st.Effect, loop)
		for _, arg := range st.Args {
			if arg.Value.Kind == "selected_result" {
				fmt.Fprintf(&readme, "   Select exactly one item from step %d result%s where `%s` equals invocation input `%s`; use item%s for `%s`.\n",
					arg.Value.Step, arg.Value.CollectionPath, arg.Value.PredicatePath, arg.Value.Input, arg.Value.ResultPath, strings.Join(arg.Path, "."))
			} else if arg.Value.Kind == "indexed_result" {
				fmt.Fprintf(&readme, "   Select step %d result at zero-based index input `%s`; use result%s for `%s`.\n",
					arg.Value.Step, arg.Value.Input, arg.Value.ResultPath, strings.Join(arg.Path, "."))
			} else if arg.Value.Kind == "collection_index" || arg.Value.Kind == "collection_index_item" {
				selection := "one zero-based index"
				if arg.Value.Kind == "collection_index_item" {
					selection = "each zero-based index in the caller list"
				}
				fmt.Fprintf(&readme, "   Select %s `%s` from step %d result%s; use item%s for `%s`.\n",
					selection, arg.Value.Input, arg.Value.Step, arg.Value.CollectionPath, arg.Value.ResultPath, strings.Join(arg.Path, "."))
			}
		}
		if st.DistinctLoopSelections {
			fmt.Fprintf(&readme, "   Selected positions in `%s` must be distinct.\n", st.Loop)
		}
		if st.SameItemAs != 0 {
			fmt.Fprintf(&readme, "   Runs right after step %d, for the same item.\n", st.SameItemAs)
		}
		if st.WhenInput != "" {
			fmt.Fprintf(&readme, "   Runs only when `%s` is `%s`.\n", st.WhenInput, st.WhenValue)
		}
		for _, pair := range st.DistinctResultInputs {
			fmt.Fprintf(&readme, "   Result index inputs `%s` and `%s` must select different items.\n", pair[0], pair[1])
		}
		if len(st.OptionalProfiles) > 0 {
			var profiles []string
			for _, profile := range st.OptionalProfiles {
				if len(profile) == 0 {
					profiles = append(profiles, "none")
				} else {
					profiles = append(profiles, strings.Join(profile, " + "))
				}
			}
			fmt.Fprintf(&readme, "   Observed optional combinations: %s.\n", strings.Join(profiles, "; "))
		}
	}
	readme.WriteString("\nOutput is a JSON object containing each step's result under its `step_N` key.\n")
	if len(g.Sources) > 0 {
		readme.WriteString("\n## Local evidence refs\n\n")
		for _, ref := range g.Sources {
			fmt.Fprintf(&readme, "- `%s`\n", ref)
		}
	}
	files := map[string][]byte{"primitive.yaml": m.YAML(), "main.py": []byte(code.String()), "README.md": []byte(readme.String())}
	_, digest, err := pack.PackFiles(files, func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	return &GeneratedPackage{Graph: g, Manifest: m, Files: files, Digest: digest}, nil
}

func ProgramPipelineStageArgs(st ProgramStep, stage int) ([]ProgramArg, error) {
	prefix := fmt.Sprintf("pipe_%d_", stage)
	var args []ProgramArg
	for _, arg := range st.Args {
		if len(arg.Path) != 1 {
			return nil, errors.New("pipeline argument path is not positional")
		}
		if !strings.HasPrefix(arg.Path[0], prefix) {
			continue
		}
		arg.Path = []string{strings.TrimPrefix(arg.Path[0], prefix)}
		args = append(args, arg)
	}
	return args, nil
}

func ProgramOptionalNames(st ProgramStep) ([]string, error) {
	names := map[string]bool{}
	for _, arg := range st.Args {
		if !arg.Optional {
			continue
		}
		if arg.Value.Kind == "item" && st.Loop != "" {
			continue // validated against the loop input's item profiles
		}
		if arg.Value.Kind != "input" || arg.Value.Input == "" {
			return nil, fmt.Errorf("optional argument %s is not bound to a caller input", strings.Join(arg.Path, "/"))
		}
		names[arg.Value.Input] = true
	}
	if len(names) == 0 {
		if len(st.OptionalProfiles) > 0 {
			return nil, errors.New("profiles declared for a step with no optional arguments")
		}
		return nil, nil
	}
	if len(st.OptionalProfiles) == 0 {
		return nil, errors.New("optional arguments lack observed presence profiles")
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	for _, profile := range st.OptionalProfiles {
		seen := map[string]bool{}
		for _, name := range profile {
			if !names[name] || seen[name] {
				return nil, fmt.Errorf("optional profile contains unknown or repeated input %q", name)
			}
			seen[name] = true
		}
	}
	return out, nil
}

func containsString(xs []string, wanted string) bool {
	for _, x := range xs {
		if x == wanted {
			return true
		}
	}
	return false
}

// wrapChosenStep keeps the normal code generator for each operation while
// making a caller-selected branch conditional. The branch is validated before
// any tool call, and steps that read its result must use the same condition.
func wrapChosenStep(code *strings.Builder, start int, step ProgramStep) {
	if step.WhenInput == "" {
		return
	}
	whole := code.String()
	prefix, body := strings.Clone(whole[:start]), strings.TrimSuffix(strings.Clone(whole[start:]), "\n")
	code.Reset()
	code.WriteString(prefix)
	code.WriteString("if inputs[" + strconv.Quote(step.WhenInput) + "] == " + strconv.Quote(step.WhenValue) + ":\n")
	code.WriteString("    " + strings.ReplaceAll(body, "\n", "\n    ") + "\n")
}

func ProgramJSONType(t string) string {
	switch t {
	case "integer", "number", "boolean", "array", "object":
		return t
	default:
		return "string"
	}
}

type ProgramArgTree struct {
	Children map[string]*ProgramArgTree `json:"-"`
	Value    *ProgramValue              `json:"-"`
	Encoded  bool                       `json:"-"`
	Optional bool                       `json:"-"`
}

func ProgramCommandArgs(st ProgramStep) (string, []string, error) {
	if st.Command == "" || st.Effect != "read" && st.Effect != "write" {
		return "", nil, errors.New("command or effect is unresolved")
	}
	if len(st.Args) == 0 {
		if shellparse.ProgramCommandRunsCode(st.Command, nil) {
			return "", nil, errors.New("command can run code outside declared argv reach")
		}
		return "[]", []string{}, nil
	}
	values := make([]string, len(st.Args))
	patterns := make([]string, len(st.Args))
	seen := make([]bool, len(st.Args))
	for _, arg := range st.Args {
		if len(arg.Path) != 1 || arg.Optional || arg.JSONString {
			return "", nil, errors.New("command arguments must be required positional words")
		}
		var index int
		if _, err := fmt.Sscanf(arg.Path[0], "argv_%d", &index); err != nil || fmt.Sprintf("argv_%d", index) != arg.Path[0] || index < 0 || index >= len(values) || seen[index] {
			return "", nil, fmt.Errorf("invalid command argument path %q", arg.Path[0])
		}
		seen[index] = true
		expr, _, err := RenderProgramTree(&ProgramArgTree{Value: &arg.Value})
		if err != nil {
			return "", nil, err
		}
		if arg.Value.Kind != "selector" && arg.Value.Kind != "input" && arg.Value.Kind != "result" && arg.Value.Kind != "indexed_result" && arg.Value.Kind != "item" && arg.Value.Kind != "item_result" {
			return "", nil, fmt.Errorf("unsupported command argument binding %q", arg.Value.Kind)
		}
		values[index] = expr
		patterns[index] = "*"
		if arg.Value.Kind == "selector" {
			if strings.ContainsAny(arg.Value.Selector, "*?[]") {
				return "", nil, errors.New("command selector contains a glob pattern")
			}
			patterns[index] = arg.Value.Selector
		}
	}
	for i, ok := range seen {
		if !ok {
			return "", nil, fmt.Errorf("command argument %d is missing", i)
		}
	}
	if shellparse.ProgramCommandRunsCode(st.Command, patterns) {
		return "", nil, errors.New("command can run code outside declared argv reach")
	}
	return "[" + strings.Join(values, ", ") + "]", patterns, nil
}

func ProgramArgsExpression(args []ProgramArg) (string, error) {
	root := &ProgramArgTree{Children: map[string]*ProgramArgTree{}}
	for i := range args {
		a := &args[i]
		if len(a.Path) == 0 || a.Value.Kind == "" {
			return "", errors.New("argument has no path or binding")
		}
		n := root
		for j, part := range a.Path {
			if n.Children == nil {
				return "", fmt.Errorf("argument %s collides with a value", strings.Join(a.Path, "/"))
			}
			child := n.Children[part]
			if child == nil {
				child = &ProgramArgTree{Children: map[string]*ProgramArgTree{}}
				n.Children[part] = child
			}
			n = child
			if j == 0 && a.JSONString {
				n.Encoded = true
			}
		}
		if n.Value != nil || len(n.Children) > 0 {
			return "", fmt.Errorf("argument %s overlaps another", strings.Join(a.Path, "/"))
		}
		n.Value, n.Children, n.Optional = &a.Value, nil, a.Optional
	}
	expr, _, err := RenderProgramTree(root)
	return expr, err
}

// RenderProgramTree returns an expression and the condition under which that
// node exists. Optional leaves are omitted, including an encoded parent whose
// only children are absent; they are never sent as null or an empty object.
func RenderProgramTree(n *ProgramArgTree) (string, string, error) {
	if n.Value != nil {
		presence := "True"
		if n.Optional {
			if n.Value.Kind == "item" {
				field := strings.TrimPrefix(n.Value.ResultPath, ".")
				if field == "" || strings.ContainsAny(field, ".[]") {
					return "", "", fmt.Errorf("optional item argument must be one item field")
				}
				presence = strconv.Quote(field) + " in item"
			} else if n.Value.Kind != "input" {
				return "", "", fmt.Errorf("optional argument must be a caller input")
			} else {
				presence = strconv.Quote(n.Value.Input) + " in inputs"
			}
		}
		switch n.Value.Kind {
		case "input":
			return "inputs[" + strconv.Quote(n.Value.Input) + "]", presence, nil
		case "item":
			if n.Value.ResultPath == "" {
				return "item", presence, nil
			}
			path, err := ProgramResultPath(n.Value.ResultPath)
			if err != nil {
				return "", "", err
			}
			return "_at(item, " + path + ")", presence, nil
		case "item_result":
			if n.Value.ResultPath == "" {
				return "item", presence, nil
			}
			path, err := ProgramResultPath(n.Value.ResultPath)
			if err != nil {
				return "", "", err
			}
			return "_at(item, " + path + ")", presence, nil
		case "selected_result":
			if n.Value.Step < 1 || n.Value.Input == "" {
				return "", "", fmt.Errorf("incomplete selected result binding")
			}
			collection := "result_" + strconv.Itoa(n.Value.Step)
			if n.Value.CollectionPath != "" {
				path, err := ProgramResultPath(n.Value.CollectionPath)
				if err != nil {
					return "", "", err
				}
				collection = "_at(" + collection + ", " + path + ")"
			}
			predicate, err := ProgramResultPath(n.Value.PredicatePath)
			if err != nil {
				return "", "", err
			}
			selected := "_one(" + collection + ", " + predicate + ", inputs[" + strconv.Quote(n.Value.Input) + "])"
			if n.Value.ResultPath == "" {
				return selected, presence, nil
			}
			itemPath, err := ProgramResultPath(n.Value.ResultPath)
			if err != nil {
				return "", "", err
			}
			return "_at(" + selected + ", " + itemPath + ")", presence, nil
		case "indexed_result":
			if n.Value.Step < 1 || n.Value.Input == "" {
				return "", "", fmt.Errorf("incomplete indexed result binding")
			}
			selected := "_indexed(result_" + strconv.Itoa(n.Value.Step) + ", inputs[" + strconv.Quote(n.Value.Input) + "])"
			if n.Value.ResultPath == "" {
				return selected, presence, nil
			}
			path, err := ProgramResultPath(n.Value.ResultPath)
			if err != nil {
				return "", "", err
			}
			return "_at(" + selected + ", " + path + ")", presence, nil
		case "collection_index", "collection_index_item":
			if n.Value.Step < 1 || n.Value.Input == "" {
				return "", "", fmt.Errorf("incomplete collection index binding")
			}
			collection := "result_" + strconv.Itoa(n.Value.Step)
			if n.Value.CollectionPath != "" {
				path, err := ProgramResultPath(n.Value.CollectionPath)
				if err != nil {
					return "", "", err
				}
				collection = "_at(" + collection + ", " + path + ")"
			}
			index := "inputs[" + strconv.Quote(n.Value.Input) + "]"
			if n.Value.Kind == "collection_index_item" {
				index = "item"
			}
			selected := "_indexed(" + collection + ", " + index + ")"
			if n.Value.ResultPath == "" {
				return selected, presence, nil
			}
			path, err := ProgramResultPath(n.Value.ResultPath)
			if err != nil {
				return "", "", err
			}
			return "_at(" + selected + ", " + path + ")", presence, nil
		case "selector":
			return strconv.Quote(n.Value.Selector), presence, nil
		case "iteration_result":
			path, err := ProgramResultPath(n.Value.ResultPath)
			if err != nil {
				return "", "", err
			}
			return fmt.Sprintf("_at(_r_%d, %s)", n.Value.Step, path), presence, nil
		case "result":
			path, err := ProgramResultPath(n.Value.ResultPath)
			if err != nil {
				return "", "", err
			}
			return fmt.Sprintf("_at(result_%d, %s)", n.Value.Step, path), presence, nil
		default:
			return "", "", fmt.Errorf("unknown binding kind %q", n.Value.Kind)
		}
	}
	keys := make([]string, 0, len(n.Children))
	for k := range n.Children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fields []string
	var presences []string
	requiredChild := false
	for _, k := range keys {
		value, presence, err := RenderProgramTree(n.Children[k])
		if err != nil {
			return "", "", err
		}
		if presence == "True" {
			fields = append(fields, strconv.Quote(k)+": "+value)
			requiredChild = true
		} else {
			fields = append(fields, "**({"+strconv.Quote(k)+": "+value+"} if "+presence+" else {})")
			presences = append(presences, "("+presence+")")
		}
	}
	expr := "{" + strings.Join(fields, ", ") + "}"
	if n.Encoded {
		expr = "json.dumps(" + expr + ", separators=(',', ':'))"
	}
	if requiredChild || len(presences) == 0 {
		return expr, "True", nil
	}
	return expr, strings.Join(presences, " or "), nil
}

// ProgramResultPath parses only the structured JSON paths the reader records.
// A path that needs a dynamic list selection cannot be compiled as index 0.
func ProgramResultPath(path string) (string, error) {
	var parts []string
	for path != "" {
		if strings.HasPrefix(path, ".[") {
			path = path[1:]
		}
		switch {
		case strings.HasPrefix(path, "."):
			path = path[1:]
			i := strings.IndexAny(path, ".[")
			if i < 0 {
				i = len(path)
			}
			if i == 0 {
				return "", fmt.Errorf("invalid result path %q", path)
			}
			parts = append(parts, strconv.Quote(path[:i]))
			path = path[i:]
		case strings.HasPrefix(path, "["):
			end := strings.IndexByte(path, ']')
			if end < 0 {
				return "", fmt.Errorf("unclosed result path %q", path)
			}
			token := path[1:end]
			if strings.HasPrefix(token, "\"") {
				var key string
				if json.Unmarshal([]byte(token), &key) != nil {
					return "", fmt.Errorf("invalid result key %q", token)
				}
				parts = append(parts, strconv.Quote(key))
			} else {
				i, err := strconv.Atoi(token)
				if err != nil || i < 0 {
					return "", fmt.Errorf("invalid result index %q", token)
				}
				parts = append(parts, strconv.Itoa(i))
			}
			path = path[end+1:]
		default:
			return "", fmt.Errorf("unsupported result path %q", path)
		}
	}
	if len(parts) == 0 {
		return "", errors.New("empty result path")
	}
	return "[" + strings.Join(parts, ", ") + "]", nil
}
