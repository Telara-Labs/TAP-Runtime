// Package codegen turns recurring logic into an exact, reviewable TAP program
// without calling a model: the program graph, its variants, and the generated package.
package codegen

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
	"gitlab.com/telara-labs/tap-runtime/discover/pyparse"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/routine"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
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
		inputTypes[input.Name] = input.Type
		inputSpecs[input.Name] = input
	}
	for i, step := range g.Steps {
		indexedInputs := map[string]bool{}
		for _, arg := range step.Args {
			v := arg.Value
			if v.Kind == "collection_index" || v.Kind == "collection_index_item" {
				input, ok := inputSpecs[v.Input]
				if v.Step < 1 || v.Step > i || !ok || input.Type != "integer" || input.Optional ||
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
		if in.List {
			schema = map[string]any{"type": "array", "items": map[string]any{"type": typ}}
			if len(in.Fields) > 0 {
				itemProps := map[string]any{}
				itemRequired := make([]string, 0, len(in.Fields))
				for _, field := range in.Fields {
					itemProps[field.Name] = map[string]any{"type": ProgramJSONType(field.Type)}
					itemRequired = append(itemRequired, field.Name)
				}
				sort.Strings(itemRequired)
				schema = map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": itemProps, "required": itemRequired, "additionalProperties": false}}
			}
		}
		props[in.Name] = schema
		if !in.Optional {
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
		if !in.Optional {
			code.WriteString("if " + q + " not in inputs:\n    raise ValueError('missing input ' + " + q + ")\n")
		}
		guard := ""
		if in.Optional {
			guard = q + " in inputs and "
		}
		if in.List {
			code.WriteString("if " + guard + "not isinstance(inputs[" + q + "], list):\n    raise ValueError('input ' + " + q + " + ' must be a list')\n")
			code.WriteString("if " + guard + "not all(_typed(item, " + strconv.Quote(ProgramJSONType(in.Type)) + ") for item in inputs[" + q + "]):\n    raise ValueError('input ' + " + q + " + ' has an item of the wrong type')\n")
			for _, field := range in.Fields {
				fq := strconv.Quote(field.Name)
				code.WriteString("if " + guard + "not all(" + fq + " in item and _typed(item[" + fq + "], " + strconv.Quote(ProgramJSONType(field.Type)) + ") for item in inputs[" + q + "]):\n    raise ValueError('input ' + " + q + " + ' has a missing or mistyped field ' + " + fq + ")\n")
			}
		} else {
			code.WriteString("if " + guard + "not _typed(inputs[" + q + "], " + strconv.Quote(ProgramJSONType(in.Type)) + "):\n    raise ValueError('input ' + " + q + " + ' has the wrong type')\n")
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
	code.WriteString("outputs = {}\n")
	for i, st := range g.Steps {
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
			code.WriteString(resultName + " = []\n")
			code.WriteString("for item in inputs[" + strconv.Quote(st.Loop) + "]:\n")
			code.WriteString("    " + resultName + ".append(" + call + ")\n")
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
	}
	code.WriteString("print(json.dumps(outputs, sort_keys=True))\n")
	if problems := m.RunProblems(); len(problems) > 0 {
		return nil, fmt.Errorf("generated manifest is not runnable: %s", strings.Join(problems, "; "))
	}
	var readme strings.Builder
	fmt.Fprintf(&readme, "# %s\n\nGenerated privately from %d disjoint execution(s) in %d session(s), represented by %d local span(s), including overlaps. Review the exact code and declared tool effects before accepting.\n\n", name, g.Executions, g.Sessions, len(g.Sources))
	readme.WriteString("## Inputs\n\n")
	for _, in := range g.Inputs {
		kind := in.Type
		if in.List {
			kind = "list<" + kind + ">"
		}
		if in.Optional {
			kind += " (optional; omitted when absent)"
		}
		fmt.Fprintf(&readme, "- `%s`: %s, from %s\n", in.Name, kind, in.Source)
		for _, field := range in.Fields {
			fmt.Fprintf(&readme, "  - `%s`: %s -> tool argument `%s`\n", field.Name, field.Type, strings.Join(field.Path, "."))
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
	switch st.Command {
	case "git", "gh", "kubectl", "docker", "helm", "tap":
		if len(patterns) == 0 || patterns[0] == "*" {
			return "", nil, errors.New("command operation selector is not fixed")
		}
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
			if n.Value.Kind != "input" {
				return "", "", fmt.Errorf("optional argument must be a caller input")
			}
			presence = strconv.Quote(n.Value.Input) + " in inputs"
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
		nodes := retrieval.BuildSpanNodes(s, p.Request, calls, byCall)
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
						graph.Problems = append(graph.Problems, fmt.Sprintf("step %d stage %d has unknown effect", step+1, j+1))
					}
					ps.Pipeline = append(ps.Pipeline, ProgramCommand{Name: words[0], Effect: effect, Connector: stage.Connector})
				}
			}
		} else if first.Node.Call.MCPServer != "" && first.Node.Call.MCPTool != "" {
			ps.Binding = &ProgramToolBinding{Server: first.Node.Call.MCPServer, Tool: first.Node.Call.MCPTool}
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
								if j >= len(op.Node.Steps) || trace.StepEffect(op.Node.Steps[j]) != ps.Pipeline[j].Effect {
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
			case trace.OperationSelector(first.Node.Call, path):
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

//go:embed inline_file_replace_ast.py
var InlineFileReplaceAST []byte

// This parser reads code as data. It never executes a recorded script and
// returns no recorded path or replacement text to the compiler.
func StrictInlineFileReplacePy(body string) bool {
	if len(body) > 16<<10 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", string(InlineFileReplaceAST))
	cmd.Stdin = strings.NewReader(body)
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return false
	}
	return out.String() == "yes\n"
}

func SynthesizeInlineFileReplace(g *ProgramGraph, traces []ObservedTrace) {
	shape := traces[0].Span.CodeShape
	sessions := map[string]bool{}
	embedded := false
	for _, tr := range traces {
		if tr.Span.CodeShape != shape || len(tr.Groups) != 1 || len(tr.Groups[0]) != 1 {
			g.Problems = append(g.Problems, "inline code shapes or source call boundaries differ")
			return
		}
		body, isEmbedded, ok := pyparse.InlinePythonBody(tr.Groups[0][0].Node.Call.Command)
		if !ok || !pyparse.StrictInlineFileReplace(body) {
			g.Problems = append(g.Problems, "inline Python is not a proved same-file four-statement read/replace/write")
			return
		}
		sessions[tr.Span.Client+"\x00"+tr.Span.Session] = true
		embedded = embedded || isEmbedded
	}
	if len(sessions) < 2 {
		g.Problems = append(g.Problems, "inline file transform has fewer than two independent sessions")
		return
	}
	g.InlineFileReplace = &InlineFileReplace{Embedded: embedded}
	g.Inputs = []ProgramInput{
		{Name: "file_path", Type: "string", Source: "caller; same path is read and written"},
		{Name: "old", Type: "string", Source: "caller; nonempty search text"},
		{Name: "new", Type: "string", Source: "caller; replacement text"},
	}
	g.Steps = []ProgramStep{
		{Role: "read file", Tool: "tap.read", Effect: "read", Args: []ProgramArg{{Path: []string{"path"}, Value: ProgramValue{Kind: "input", Input: "file_path"}}}},
		{Role: "replace text", Tool: "python.str.replace", Effect: "none", Args: []ProgramArg{{Path: []string{"old"}, Value: ProgramValue{Kind: "input", Input: "old"}}, {Path: []string{"new"}, Value: ProgramValue{Kind: "input", Input: "new"}}}},
		{Role: "write same file", Tool: "tap.write", Effect: "write", Args: []ProgramArg{{Path: []string{"path"}, Value: ProgramValue{Kind: "input", Input: "file_path"}}}},
	}
}

func GenerateInlineFileReplace(g *ProgramGraph) (*GeneratedPackage, error) {
	if len(g.Steps) != 3 || len(g.Inputs) != 3 || g.InlineFileReplace == nil {
		return nil, fmt.Errorf("invalid inline file replacement graph")
	}
	name := "discovered-" + strings.TrimPrefix(g.CandidateID, "lc_")
	if !pack.SkillName.MatchString(name) {
		return nil, fmt.Errorf("candidate %q cannot name a package", g.CandidateID)
	}
	m := &manifest.Manifest{APIVersion: manifest.APIVersion, Kind: "Primitive",
		Metadata:  manifest.Metadata{Publisher: "local.discover", Name: name, Version: "0.1.0", Description: "Replace text in a caller-selected file under the invocation directory."},
		Execution: manifest.Execution{Runtime: manifest.RuntimeWasm, Entrypoint: "main.py"},
		Files:     []manifest.File{{Path: ".", Access: "write"}},
		Interface: &manifest.Interface{InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"file_path": map[string]any{"type": "string"}, "old": map[string]any{"type": "string"}, "new": map[string]any{"type": "string"}},
			"required": []string{"file_path", "old", "new"}, "additionalProperties": false}, OutputSchema: map[string]any{"type": "object"}},
	}
	code := `import json
import sys

if len(sys.argv) != 2:
    raise ValueError('pass one JSON object of typed inputs')
inputs = json.loads(sys.argv[1])
if not isinstance(inputs, dict) or set(inputs) != {'file_path', 'old', 'new'}:
    raise ValueError('file_path, old and new are required')
if not all(isinstance(inputs[key], str) for key in ('file_path', 'old', 'new')):
    raise ValueError('all inputs must be strings')
if not inputs['file_path'] or not inputs['old']:
    raise ValueError('file_path and old must be nonempty')
before = tap.read(inputs['file_path'])
if '\ufffd' in before:
    raise ValueError('file is not valid UTF-8')
after = before.replace(inputs['old'], inputs['new'])
tap.write(inputs['file_path'], after)
print(json.dumps({'path': inputs['file_path'], 'replacements': before.count(inputs['old'])}))
`
	readme := "# Discovered file replacement\n\nThis exact inner Python pattern was observed in " + fmt.Sprint(g.Executions) + " execution(s) across " + fmt.Sprint(g.Sessions) + " sessions. It reads one caller-selected text file, replaces every occurrence of nonempty `old` with `new`, and writes the same file. The runtime grants this package file write reach under the invocation working directory (`files: .`); each write still needs runtime approval. It cannot read or write a path outside that directory.\n\nInputs: `file_path`, `old`, `new` (strings). Output: path and replacement count. This code does not run shell prefixes, suffixes, tests, git operations, or any other surrounding commands. It is a generic text replacement; source recurrence alone does not prove user-task usefulness or savings. Review the exact code and reach before private acceptance.\n"
	if g.InlineFileReplace.Embedded {
		readme += "\nEvery supporting snippet was embedded in a larger shell call; only the inner file transform is generated.\n"
	}
	files := map[string][]byte{"primitive.yaml": m.YAML(), "main.py": []byte(code), "README.md": []byte(readme)}
	_, digest, err := pack.PackFiles(files, func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	return &GeneratedPackage{Graph: g, Manifest: m, Files: files, Digest: digest}, nil
}

// GroupProgramVariants separates incompatible invocation contracts inside one
// recurring logic family, then merges argument-presence variants only when
// graph synthesis and code generation both succeed for their combined spans.
// Values never enter identity. Different tool bindings remain separate;
// optional arguments do not make one process look like several primitives.
func GroupProgramVariants(c model.LogicCandidate, proposals []model.SpanProposal, sessions []trace.Session) ([]model.LogicCandidate, error) {
	bySpan := map[string]model.SpanProposal{}
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
	type bucket struct {
		candidate model.LogicCandidate
		sessions  map[string]bool
		core      string
	}
	buckets := map[string]*bucket{}
	for _, id := range c.Members {
		p, ok := bySpan[id]
		if !ok {
			return nil, fmt.Errorf("candidate %s names absent span %s", c.ID, id)
		}
		s, ok := bySession[p.Client+"\x00"+p.Session]
		if !ok {
			return nil, fmt.Errorf("source for span %s is unavailable", id)
		}
		var requestCalls []trace.Call
		for _, call := range s.Calls {
			if call.Request == p.Request {
				requestCalls = append(requestCalls, call)
			}
		}
		var steps []string
		var coreSteps []string
		for i, ordinal := range p.Calls {
			if ordinal < 1 || ordinal > len(requestCalls) || i >= len(p.CallHashes) {
				return nil, fmt.Errorf("span %s call %d is unavailable", id, ordinal)
			}
			call := requestCalls[ordinal-1]
			if retrieval.SpanCallHash(call) != p.CallHashes[i] {
				return nil, fmt.Errorf("span %s source call %d changed", id, ordinal)
			}
			sig := ProgramCallSignature(call)
			if len(steps) == 0 || steps[len(steps)-1] != sig {
				steps = append(steps, sig)
			}
			core := ProgramCallCoreSignature(call)
			if len(coreSteps) == 0 || coreSteps[len(coreSteps)-1] != core {
				coreSteps = append(coreSteps, core)
			}
		}
		if len(steps) == 0 {
			continue
		}
		if p.CodeShape != "" {
			// A broad call-order motif is only retrieval evidence. Keep the
			// exact code syntax shape separate until AST and data-flow proof.
			steps = []string{"inline_python:" + p.CodeShape}
			coreSteps = append([]string(nil), steps...)
		}
		signature := strings.Join(steps, " -> ")
		b := buckets[signature]
		if b == nil {
			sum := sha256.Sum256([]byte(signature))
			variant := c
			variant.ID = c.ID + "-v" + hex.EncodeToString(sum[:4])
			variant.Key = signature
			variant.Members = nil
			variant.Executions = 0
			variant.Sessions = 0
			variant.Proposals = 0
			variant.Example = p
			b = &bucket{candidate: variant, sessions: map[string]bool{}, core: strings.Join(coreSteps, " -> ")}
			buckets[signature] = b
		}
		b.candidate.Members = append(b.candidate.Members, id)
		b.candidate.Proposals++
		b.candidate.Executions++
		b.sessions[p.Client+"\x00"+p.Session] = true
	}
	byCore := map[string][]*bucket{}
	for _, b := range buckets {
		b.candidate.Sessions = len(b.sessions)
		sort.Strings(b.candidate.Members)
		byCore[b.core] = append(byCore[b.core], b)
	}
	var out []model.LogicCandidate
	for core, group := range byCore {
		sort.Slice(group, func(i, j int) bool {
			if group[i].candidate.Sessions != group[j].candidate.Sessions {
				return group[i].candidate.Sessions > group[j].candidate.Sessions
			}
			return group[i].candidate.ID < group[j].candidate.ID
		})
		var merged []model.LogicCandidate
		var mergedShapeKeys [][]string
		for _, b := range group {
			joined := false
			for i := range merged {
				probe := merged[i]
				probe.Members = append(append([]string(nil), probe.Members...), b.candidate.Members...)
				sort.Strings(probe.Members)
				graph, err := SynthesizeProgramGraph(probe, proposals, sessions)
				if err != nil {
					return nil, err
				}
				if len(graph.Problems) > 0 {
					continue
				}
				if _, err := GenerateProgramPackage(graph); err != nil {
					continue
				}
				probe.Proposals += b.candidate.Proposals
				probe.Executions += b.candidate.Executions
				sessionSet := map[string]bool{}
				for _, id := range probe.Members {
					p := bySpan[id]
					sessionSet[p.Client+"\x00"+p.Session] = true
				}
				probe.Sessions = len(sessionSet)
				merged[i] = probe
				mergedShapeKeys[i] = append(mergedShapeKeys[i], b.candidate.Key)
				joined = true
				break
			}
			if !joined {
				merged = append(merged, b.candidate)
				mergedShapeKeys = append(mergedShapeKeys, []string{b.candidate.Key})
			}
		}
		for i, v := range merged {
			if len(mergedShapeKeys[i]) > 1 {
				sort.Strings(mergedShapeKeys[i])
				sum := sha256.Sum256([]byte(strings.Join(mergedShapeKeys[i], "|")))
				v.ID = c.ID + "-m" + hex.EncodeToString(sum[:4])
				v.Key = fmt.Sprintf("%s [optional fields aligned across %d shapes]", core, len(mergedShapeKeys[i]))
			}
			v.Executions = VariantIndependentExecutions(v.Members, bySpan)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Executions != out[j].Executions {
			return out[i].Executions > out[j].Executions
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func VariantIndependentExecutions(members []string, bySpan map[string]model.SpanProposal) int {
	spans := make([]model.SpanProposal, 0, len(members))
	for _, id := range members {
		spans = append(spans, bySpan[id])
	}
	sort.Slice(spans, func(i, j int) bool {
		a, b := spans[i], spans[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Request != b.Request {
			return a.Request < b.Request
		}
		if len(a.Calls) != len(b.Calls) {
			return len(a.Calls) > len(b.Calls)
		}
		return a.ID < b.ID
	})
	used := map[string]bool{}
	count := 0
	for _, p := range spans {
		overlap := false
		for _, call := range p.Calls {
			if used[retrieval.LogicCallID(p, call)] {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		count++
		for _, call := range p.Calls {
			used[retrieval.LogicCallID(p, call)] = true
		}
	}
	return count
}

func ProgramCallSignature(call trace.Call) string {
	var fields []string
	for path, field := range trace.ObservedArgs(call) {
		value := path + ":" + field.TypeName
		if field.JsonString {
			value += ":json_string"
		}
		if trace.OperationSelector(call, path) {
			value += "=" + field.Value
		}
		fields = append(fields, value)
	}
	sort.Strings(fields)
	return ProgramCallToolIdentity(call) + "@" + call.MCPServer + "/" + call.MCPTool + "(" + strings.Join(fields, ",") + ")"
}

func ProgramCallCoreSignature(call trace.Call) string {
	var selectors []string
	for path, field := range trace.ObservedArgs(call) {
		if trace.OperationSelector(call, path) {
			selectors = append(selectors, path+"="+field.Value)
		}
	}
	sort.Strings(selectors)
	return ProgramCallToolIdentity(call) + "@" + call.MCPServer + "/" + call.MCPTool + "(" + strings.Join(selectors, ",") + ")"
}

func ProgramCallToolIdentity(call trace.Call) string {
	if call.Tool == "shell" {
		if plan, err := shellparse.ProgramShellPlan(call.Command); err == nil {
			var names []string
			for _, stage := range plan {
				if stage.Connector != "" {
					names = append(names, stage.Connector)
				}
				names = append(names, stage.Words[0])
			}
			return "shell:" + strings.Join(names, ":")
		}
	}
	return call.Tool
}
