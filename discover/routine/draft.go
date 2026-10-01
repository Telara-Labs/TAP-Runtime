package routine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
	"gitlab.com/telara-labs/tap-runtime/discover/util"
	"gopkg.in/yaml.v3"
)

// DefaultPublisher names a draft that has not been given a namespace yet.
const DefaultPublisher = "local.draft"

// Step kinds.
const (
	KindCommand = "command" // a host program, declared in commands[]
	KindTool    = "tool"    // an MCP tool, declared in tools[] and capabilities[]
	KindBrowser = "browser" // awaited calls in a browser script, one tool call
	KindFetch   = "fetch"   // a page fetch, declared in fetch[]
	KindHuman   = "human"   // content decided per run: written as a marked stop
	KindSkipped = "skipped" // the agent's own bookkeeping: nothing to replay
)

// inputValues is input n's value in each drafted run (by run index).
func DraftInputValues(d *model.Draft, n int) map[int]string {
	if n < 0 || n >= len(d.Values) {
		return nil
	}
	return d.Values[n]
}

// Draft builds the package for Candidates[idx].
func ReportDraft(r *model.Report, idx int, opt model.DraftOptions) (*model.Draft, error) {
	if idx < 0 || idx >= len(r.Candidates) || r.Corpus == nil {
		return nil, errors.New("no such candidate in this run")
	}
	c := r.Candidates[idx]
	if len(c.Items) == 0 {
		return nil, errors.New("this candidate cannot be drafted")
	}
	var occ [][]trace.Step
	sessions := make([]int, 0, len(c.SessionSet))
	for s := range c.SessionSet {
		sessions = append(sessions, s)
	}
	sort.Ints(sessions)
	for _, s := range sessions {
		idxs := trace.MatchAt(r.Seqs[s], c.Items, r.Window)
		if idxs == nil {
			continue
		}
		steps := make([]trace.Step, len(idxs))
		for i, j := range idxs {
			steps[i] = r.Corpus[s].Steps[j]
		}
		occ = append(occ, steps)
	}
	if len(occ) == 0 {
		return nil, errors.New("no occurrence of this candidate could be read back")
	}
	return BuildDraft(c, occ, opt), nil
}

// SlotPlan is one argument position of one step: a fixed value, or an input.
type SlotPlan struct {
	Slot  trace.Slot `json:"-"`
	Fixed bool       `json:"-"`
	Input int        `json:"-"` // index into inputs when not fixed
}

type Drafter struct {
	Opt      model.DraftOptions `json:"-"`
	Occ      [][]trace.Step     `json:"-"`
	Inputs   []model.DraftInput `json:"-"`
	Vectors  []map[int]string   `json:"-"` // per input: occurrence -> value
	Names    map[string]bool    `json:"-"`
	Mf       manifest.Manifest  `json:"-"`
	Cmds     map[string]bool    `json:"-"`
	Files    map[string]bool    `json:"-"`
	Origins  map[string]bool    `json:"-"`
	Tools    map[string]bool    `json:"-"`
	Lines    []string           `json:"-"`
	Steps    []model.DraftStep  `json:"-"`
	HumanCnt int                `json:"-"`
	FixedCnt int                `json:"-"`
	FixedArg int                `json:"-"` // arguments that had the same value in every run
	// posStep maps a pattern position to the draft step that replays it.
	PosStep    map[int]int     `json:"-"`
	DerivedCnt int             `json:"-"`
	Extracted  int             `json:"-"`
	ListLoop   map[string]bool `json:"-"`
	PriorLoop  map[string]bool `json:"-"`
	UsesJSON   bool            `json:"-"`
	UsesGrep   bool            `json:"-"`
	// unsupported names steps the runtime cannot run as recorded.
	Unsupported []string `json:"-"`
}

// GuestReadsStdinOnly are the guest shell's own built-ins that read only
// standard input and ignore file arguments (tap-runtime guest-sh): a
// recorded "wc -l file" would count nothing there.
var GuestReadsStdinOnly = map[string]bool{"wc": true, "head": true}

// checkGuest records a step the guest runtime would not run as recorded.
func (d *Drafter) CheckGuest(prog string, plans []SlotPlan, n int) {
	if prog == "cat" {
		for _, p := range plans {
			if !p.Slot.Sub && p.Slot.Type != trace.SlotFlag {
				d.FileAccess(p, n)
			}
		}
		return
	}
	if !GuestReadsStdinOnly[prog] {
		return
	}
	for _, p := range plans {
		// "wc -l file" parses as the flag -l taking "file"; only a number
		// after a flag (head -n 20) is an option's value.
		if p.Slot.Sub || p.Slot.Type == trace.SlotFlag || p.Slot.Type == trace.SlotNumber {
			continue
		}
		d.Unsupported = append(d.Unsupported, fmt.Sprintf("guest_builtin_ignores_files:step%d:%s", n, prog))
		return
	}
}

// fileAccess declares a fixed file a step reads, so the host lets the
// guest open it; a path that varies cannot be declared ahead of time.
func (d *Drafter) FileAccess(p SlotPlan, n int) {
	if !p.Fixed {
		d.Unsupported = append(d.Unsupported, fmt.Sprintf("file_access_undeclared:step%d:%s", n, d.Inputs[p.Input].Name))
		return
	}
	if !d.Files[p.Slot.Value] {
		d.Files[p.Slot.Value] = true
		d.Mf.Files = append(d.Mf.Files, manifest.File{Path: p.Slot.Value, Access: "read"})
	}
}

// ref is a placeholder for input k in a generated line. finish replaces it
// with the argument ("${3}") or, for a value taken from an earlier step's
// output, that shell variable.
func (d *Drafter) Ref(k int) string { return "\x01" + util.Itoa(k) + "\x01" }

func BuildDraft(c model.Candidate, occ [][]trace.Step, opt model.DraftOptions) *model.Draft {
	if opt.Publisher == "" {
		opt.Publisher = DefaultPublisher
	}
	d := &Drafter{Opt: opt, Occ: occ, ListLoop: map[string]bool{}, PriorLoop: map[string]bool{}, PosStep: map[int]int{}, Names: map[string]bool{}, Cmds: map[string]bool{}, Files: map[string]bool{}, Origins: map[string]bool{}, Tools: map[string]bool{}}
	for i := 0; i < len(c.Items); {
		label := c.Steps[i].Label
		d.PosStep[i] = len(d.Steps) + 1
		if strings.HasPrefix(label, "js:") {
			// Consecutive browser calls were one script: they stay one call.
			j := i
			for j < len(c.Items) && strings.HasPrefix(c.Steps[j].Label, "js:") {
				d.PosStep[j] = len(d.Steps) + 1
				j++
			}
			d.Browser(i, j)
			i = j
			continue
		}
		if strings.HasPrefix(label, "sh:") && d.Occ[0][i].Compound {
			// One recorded command line (a pipeline, a chain, a heredoc)
			// that holds this step and possibly the next ones.
			j := i + 1
			for j < len(c.Items) && SameCall(d.Occ, i, j) {
				d.PosStep[j] = len(d.Steps) + 1
				j++
			}
			d.Compound(i, j)
			i = j
			continue
		}
		d.Step(i, label)
		i++
	}

	d.Finish()
	name := DraftName(c)
	desc := fmt.Sprintf("Unvalidated draft (never executed). Recurring routine found by tap discover in %d sessions over %d weeks (%s). Steps: %s.",
		c.Sessions, c.Weeks, ClientList(c.ByClient), model.LabelsOf(c))
	props := map[string]any{}
	var required []string
	for _, in := range d.Inputs {
		if in.Extract != "" {
			continue
		}
		props[in.Name] = map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Argument %d ($%d): %s, from %s.", in.Position, in.Position, in.Type, in.From),
		}
		required = append(required, in.Name)
	}
	in := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		in["required"] = required
	}
	d.Mf.APIVersion, d.Mf.Kind = manifest.APIVersion, "Primitive"
	d.Mf.Metadata = manifest.Metadata{Publisher: opt.Publisher, Name: name, Version: "0.1.0", Description: desc,
		OutputDescription: "What the steps print, in order."}
	d.Mf.Interface = &manifest.Interface{InputSchema: in, OutputSchema: map[string]any{"type": "object"}}
	d.Mf.Execution = manifest.Execution{Runtime: manifest.RuntimeWasm, Entrypoint: "main.sh"}
	d.Mf.Provenance = &manifest.Provenance{Source: "main.sh", Toolchain: "interpreter obtained by the runner, pinned by sha256", Build: "none: the entrypoint is the source"}

	yml, _ := yaml.Marshal(&d.Mf)
	var problems []string
	if m, err := manifest.Parse(yml); err != nil {
		problems = []string{err.Error()}
	} else {
		problems = m.PublishProblems()
	}

	var sh strings.Builder
	fmt.Fprintf(&sh, "# %s\n# Drafted by telara tap discover. Review every line before running or publishing.\n", desc)
	for _, in := range d.Inputs {
		if in.Sensitive {
			fmt.Fprintf(&sh, "# $%d  %s: a credential; supply it from your own configuration (no recorded value is kept)\n", in.Position, in.Name)
		} else if in.Extract != "" {
			fmt.Fprintf(&sh, "# %s (%s): taken from step %d's output\n", in.Name, in.Type, in.DerivedFrom)
		} else if in.DerivedFrom > 0 {
			fmt.Fprintf(&sh, "# $%d  %s (%s): in the recorded runs this came from step %d's output; take it from there (authoring needed)\n", in.Position, in.Name, in.Type, in.DerivedFrom)
		} else {
			fmt.Fprintf(&sh, "# $%d  %s (%s)\n", in.Position, in.Name, in.Type)
		}
	}
	// A failing step stops the program, including one inside a pipeline, so
	// a check that fails never lets a later write run.
	sh.WriteString("set -eo pipefail\n")
	if d.UsesJSON {
		sh.WriteString(JsonHelpers + "\n")
	}
	for _, l := range d.Lines {
		sh.WriteString(l)
		sh.WriteByte('\n')
	}

	files := map[string][]byte{
		"primitive.yaml": yml,
		"main.sh":        []byte(sh.String()),
		"README.md":      []byte(DraftReadme(name, desc, d)),
	}
	return &model.Draft{
		Blocked: redact.ScanArtifacts(files),
		Name:    name, Publisher: opt.Publisher, Inputs: d.Inputs, Steps: d.Steps,
		RuntimeUnsupported: d.Unsupported,
		ListLoop:           d.ListLoop, PriorLoop: d.PriorLoop, HumanPos: d.HumanPositions(), FirstRun: occ[0], PosStep: d.PosStep,
		Problems: problems, HumanSteps: d.HumanCnt, Derived: d.DerivedCnt, Extracted: d.Extracted, FixedSteps: d.FixedCnt, FixedShare: FixedShare(d), Values: d.Vectors,
		Files: files,
	}
}

func (d *Drafter) Effect(n int) string {
	if d.Opt.ReadOnly[n] {
		return "read"
	}
	return "write"
}

// plan decides, for step i, which arguments are fixed and which are inputs.
// It uses the occurrences with the most common skeleton and argument keys,
// and compares values key by key, so arguments given in a different order
// still line up.
func (d *Drafter) Plan(i int, stepName string) []SlotPlan {
	sig := func(st trace.Step) string {
		var ks []string
		for _, sl := range st.Slots {
			if !trace.Derived(sl.Key) {
				ks = append(ks, sl.Key)
			}
		}
		sort.Strings(ks)
		return st.Skeleton + "#" + strings.Join(ks, ",")
	}
	count := map[string]int{}
	for _, o := range d.Occ {
		count[sig(o[i])]++
	}
	modal, best := "", -1
	for k, v := range count {
		if v > best || (v == best && k < modal) {
			modal, best = k, v
		}
	}
	var reps []int
	for j, o := range d.Occ {
		if sig(o[i]) == modal {
			reps = append(reps, j)
		}
	}
	valueOf := func(st trace.Step, key string) string {
		for _, sl := range st.Slots {
			if sl.Key == key {
				return sl.Value
			}
		}
		return ""
	}
	first := d.Occ[reps[0]][i]
	var plans []SlotPlan
	for _, sl := range first.Slots {
		if trace.Derived(sl.Key) {
			continue
		}
		p := SlotPlan{Slot: sl}
		if sl.Sub {
			p.Fixed = true
			plans = append(plans, p)
			continue
		}
		vec := map[int]string{}
		same := true
		for _, j := range reps {
			v := valueOf(d.Occ[j][i], sl.Key)
			vec[j] = v
			if v != sl.Value {
				same = false
			}
		}
		// A value that looks like a credential in any recorded run is never
		// written, however constant: it becomes a credential input.
		sensitive := false
		for _, j := range reps {
			for _, other := range d.Occ[j][i].Slots {
				if other.Key == sl.Key && redact.SensitiveSlot(d.Occ[j][i].Label, other) {
					sensitive = true
				}
				// A tool argument some run sent as JSON (an object, an
				// array, a boolean) is JSON, even where another client
				// recorded it as text.
				if other.Key == sl.Key && other.Raw && !sl.Raw && json.Valid([]byte(other.Value)) {
					sl.Raw, sl.Value = true, other.Value
				}
			}
		}
		if t, ok := ArgSchema(sl)["type"].(string); ok && sl.Raw {
			sl.Type = t
		}
		p.Slot = sl
		switch {
		case sensitive:
			p.Input = d.Input(stepName, sl, vec, true, i)
		case same && len(reps) > 1:
			p.Fixed = true
			d.FixedArg++
		case sl.Key == "recv":
			// A JS variable the author named differently each run is not an
			// input anyone would pass; the browser step names it itself.
			p.Input = -1
		default:
			p.Input = d.Input(stepName, sl, vec, false, i)
		}
		plans = append(plans, p)
	}
	return plans
}

// input returns the input for a varying slot, reusing an earlier input that
// held the same value in every occurrence both appear in (a path passed to
// gofmt and then to go test is one input, not two).
func (d *Drafter) Input(stepName string, sl trace.Slot, vec map[int]string, sensitive bool, pos int) int {
	for n, other := range d.Vectors {
		if d.Inputs[n].Sensitive != sensitive {
			continue
		}
		common, agree := 0, true
		for j, v := range vec {
			if w, ok := other[j]; ok {
				common++
				if v != w {
					agree = false
					break
				}
			}
		}
		if agree && common >= 2 {
			return n
		}
	}
	base := stepName
	key := strings.SplitN(sl.Key, "#", 2)[0]
	switch {
	case strings.HasSuffix(key, "="):
		base += "_" + strings.TrimLeft(strings.TrimSuffix(key, "="), "-")
	case len(key) > 1 && key[0] == 'p' && IsDigits(key[1:]):
		base += "_arg" + key[1:]
	case key == "recv":
		base += "_object"
	default:
		base += "_" + strings.TrimLeft(key, "-")
	}
	name := SanitizeName(base)
	for k := 2; d.Names[name]; k++ {
		name = fmt.Sprintf("%s_%d", SanitizeName(base), k)
	}
	d.Names[name] = true
	in := model.DraftInput{Name: name, Type: sl.Type, Raw: sl.Raw, Example: redact.Redact(sl.Value), From: stepName, Pos: pos}
	if !sensitive {
		if h := d.DerivedFrom(pos, vec); h >= 0 {
			in.DerivedFrom = d.PosStep[h]
			if pat, strip, binding, ok := d.Extraction(h, vec); ok && d.Capturable(in.DerivedFrom) {
				in.Extract, in.Strip, in.Binding = pat, strip, binding
				d.Extracted++
			} else {
				d.DerivedCnt++
			}
		}
	}
	if sensitive {
		in.Sensitive, in.Type, in.Example = true, trace.SlotSecret, ""
	}
	d.Inputs = append(d.Inputs, in)
	d.Vectors = append(d.Vectors, vec)
	return len(d.Inputs) - 1
}

func (d *Drafter) Step(i int, label string) {
	n := len(d.Steps) + 1
	switch {
	case strings.HasPrefix(label, "sh:"):
		d.Command(i, n, label)
	case strings.HasPrefix(label, "mcp:"):
		d.Tool(i, n, strings.TrimPrefix(label, "mcp:"), label)
	case strings.HasPrefix(label, "patch:"):
		d.Human(i, n, label, "file")
	default:
		d.Builtin(i, n, label)
	}
}

func (d *Drafter) Command(i, n int, label string) {
	fields := strings.Fields(strings.TrimPrefix(label, "sh:"))
	prog := fields[0]
	stepName := strings.Join(fields, "_")
	plans := d.Plan(i, stepName)
	// A step run once per item of a list the request gave becomes a loop
	// over a list input.
	spec, isLoop := d.Opt.Loops[label]
	loopIn := -1
	if isLoop && spec.Source == "prior_output" {
		// The list came from an earlier result; binding a collection out of
		// a result is not drafted, so this step is marked for authoring.
		d.PriorLoop[label] = true
		isLoop = false
	}
	if isLoop {
		for _, p := range plans {
			if p.Slot.Key == spec.Key && !p.Fixed && p.Input >= 0 {
				loopIn = p.Input
			}
		}
	}
	if loopIn >= 0 {
		in := &d.Inputs[loopIn]
		in.List, in.Raw, in.Type, in.Example = true, true, "list", ""
		vec := map[int]string{}
		for j, vs := range spec.Values {
			if vs == nil {
				continue
			}
			b, _ := json.Marshal(vs)
			vec[j] = string(b)
		}
		d.Vectors[loopIn] = vec
		d.ListLoop[label] = true
	}
	d.CheckGuest(prog, plans, n)
	words := []string{prog}
	var globals []string
	subSeen, sub := false, ""
	for k, p := range plans {
		switch {
		case p.Slot.Sub:
			subSeen, sub = true, p.Slot.Value
			words = append(words, p.Slot.Value)
		case p.Fixed:
			words = append(words, ShellQuote(p.Slot.Value))
		case p.Input == loopIn:
			words = append(words, `"$item"`)
		default:
			words = append(words, `"`+d.Ref(p.Input)+`"`)
		}
		// Flags before the subcommand are the program's global flags; the
		// word keyed "<flag>=" after one is its value.
		if !subSeen && len(fields) > 1 && p.Slot.Type == trace.SlotFlag {
			g := p.Slot.Value
			if k+1 < len(plans) && plans[k+1].Slot.Key == p.Slot.Key+"=" {
				if plans[k+1].Fixed {
					g += " " + plans[k+1].Slot.Value
				} else {
					g += " <any>"
				}
			}
			globals = append(globals, g)
		}
	}
	args := []string{"*"}
	if sub != "" {
		args = []string{sub, "*"}
	}
	eff := d.Effect(n)
	key := prog + "\x00" + strings.Join(globals, "\x00") + "\x00" + strings.Join(args, "\x00") + "\x00" + eff
	if !d.Cmds[key] {
		d.Cmds[key] = true
		d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: prog, Globals: globals, Args: args, Effect: eff})
	}
	line := strings.Join(words, " ")
	if loopIn >= 0 {
		// Each item on its own line from the JSON array; an empty list runs
		// nothing; a malformed list fails the pipeline (pipefail).
		line = fmt.Sprintf(`printf '%%s' "%s" | jq -r '.[]' | while IFS= read -r item; do %s; done`, d.Ref(loopIn), line)
		d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s (once per item of the list; an empty list runs nothing)", n, label), line)
	} else {
		d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), line)
	}
	fixed := len(fields) > 1
	for _, p := range plans {
		if p.Fixed && !p.Slot.Sub && p.Slot.Type != trace.SlotFlag && p.Slot.Type != trace.SlotNumber {
			fixed = true
		}
	}
	if fixed {
		d.FixedCnt++
	}
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: eff})
}

var NonAlias = regexp.MustCompile(`[^a-z0-9_]+`)

// JsIdentifier matches an argument that is only a variable (url1, row.href):
// a value the recorded script computed, not one written into the call.
var JsIdentifier = regexp.MustCompile(`^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$`)

func (d *Drafter) Tool(i, n int, tool, label string) {
	alias := strings.Trim(NonAlias.ReplaceAllString(strings.ToLower(tool), "_"), "_")
	plans := d.Plan(i, alias)
	props := map[string]any{}
	var keys []string
	var pre []string
	var obj JsonObject
	for _, p := range plans {
		keys = append(keys, p.Slot.Key)
		props[p.Slot.Key] = ArgSchema(p.Slot)
		if p.Fixed {
			obj.Fixed(p.Slot.Key, JsonLiteral(p.Slot))
			continue
		}
		v := fmt.Sprintf("a%d_%d", n, p.Input+1)
		pre = append(pre, d.JsonAssign(v, p.Input))
		obj.Variable(p.Slot.Key, v)
	}
	sort.Strings(keys)
	args := map[string]any{"type": "object", "properties": props, "required": AnySlice(keys)}
	result := map[string]any{"type": "object"}
	question := fmt.Sprintf("Run %s with %s, as the recorded sessions did.", tool, OrNone(keys))
	capLabel := d.Opt.Publisher + "/" + CapName(tool) + "@1"
	eff := d.Effect(n)
	if !d.Tools[alias] {
		d.Tools[alias] = true
		d.Mf.Capabilities = append(d.Mf.Capabilities, manifest.Capability{Label: capLabel, ID: manifest.CapabilityID(question, args, result), Question: question, Args: args, Result: result})
		d.Mf.Tools = append(d.Mf.Tools, manifest.Tool{Alias: alias, Capability: capLabel, Effect: eff})
	}
	line := fmt.Sprintf(`tap call %s %s`, alias, obj.Word())
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label))
	d.Lines = append(d.Lines, pre...)
	d.Lines = append(d.Lines, line)
	d.FixedCnt++ // an MCP tool names one action in one system
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindTool, Label: label, Line: line, Effect: eff})
}

// browser writes steps i..j-1 (awaited calls of one script) as one call to
// the browser tool, rebuilding the script with this run's inputs.
func (d *Drafter) Browser(i, j int) {
	n := len(d.Steps) + 1
	var labels, pre []string
	// The script is built as one shell word: fixed JavaScript in single
	// quotes, each input spliced in as a JSON value (a JS literal).
	var code strings.Builder
	varyingObject, browserFixed := false, false
	var scriptVars []string
	for k := i; k < j; k++ {
		method := strings.TrimPrefix(d.Occ[0][k].Label, "js:")
		labels = append(labels, method)
		recv, arg := "tab", ""
		for _, p := range d.Plan(k, strings.ReplaceAll(method, ".", "_")) {
			switch {
			case p.Slot.Key == "recv" && p.Fixed:
				recv = p.Slot.Value
			case p.Slot.Key == "recv":
				varyingObject = true
			case p.Slot.Key != "0":
			case p.Fixed && p.Slot.Raw:
				browserFixed = true
				if JsIdentifier.MatchString(p.Slot.Value) {
					scriptVars = append(scriptVars, p.Slot.Value)
				}
				arg = ShellSingle(p.Slot.Value)
			case p.Fixed:
				browserFixed = true
				b, _ := json.Marshal(p.Slot.Value)
				arg = ShellSingle(string(b))
			default:
				v := fmt.Sprintf("a%d_%d", n, p.Input+1)
				if p.Slot.Raw {
					if JsIdentifier.MatchString(p.Slot.Value) {
						scriptVars = append(scriptVars, p.Slot.Value)
					}
					// A JavaScript expression the caller supplies, as is.
					pre = append(pre, fmt.Sprintf(`%s="%s"`, v, d.Ref(p.Input)))
				} else {
					d.UsesJSON = true
					pre = append(pre, fmt.Sprintf(`%s=$(json_str "%s")`, v, d.Ref(p.Input)))
				}
				arg = `"$` + v + `"`
			}
		}
		code.WriteString(ShellSingle("await "+recv+"."+method+"(") + arg + ShellSingle("); "))
	}
	question := "Run a browser script in the user's open browser session and return what it reports."
	args := map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}}, "required": []any{"code"}}
	result := map[string]any{"type": "object"}
	capLabel := d.Opt.Publisher + "/browser.script@1"
	eff := d.Effect(n)
	if !d.Tools["browser"] {
		d.Tools["browser"] = true
		d.Mf.Capabilities = append(d.Mf.Capabilities, manifest.Capability{Label: capLabel, ID: manifest.CapabilityID(question, args, result), Question: question, Args: args, Result: result})
		d.Mf.Tools = append(d.Mf.Tools, manifest.Tool{Alias: "browser", Capability: capLabel, Effect: eff})
	}
	cv := fmt.Sprintf("code%d", n)
	pre = append(pre, cv+"="+code.String())
	d.UsesJSON = true
	pre = append(pre, fmt.Sprintf(`%s_json=$(json_str "$%s")`, cv, cv))
	var obj JsonObject
	obj.Variable("code", cv+"_json")
	line := fmt.Sprintf(`tap call browser %s`, obj.Word())
	var notes []string
	if varyingObject {
		notes = append(notes, "Some calls were made on a variable the author named differently each run; the draft calls it tab. Open or select that tab first.")
	}
	if len(scriptVars) > 0 {
		// goto(url1): the value was a variable the original script computed
		// before the call. Extracting the call cannot recover it.
		notes = append(notes, "Uses values the original script computed before these calls ("+strings.Join(scriptVars, ", ")+"); this step needs the rest of that script before it can run.")
		d.HumanCnt++
	}
	note := strings.Join(notes, " ")
	label := "browser: " + strings.Join(labels, " → ")
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label))
	if note != "" {
		d.Lines = append(d.Lines, "# "+note)
	}
	d.Lines = append(d.Lines, pre...)
	d.Lines = append(d.Lines, line)
	if browserFixed {
		d.FixedCnt++
	}
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindBrowser, Label: label, Line: line, Effect: eff, Note: note})
}

func (d *Drafter) Human(i, n int, label, fileKey string) {
	target := ""
	for _, p := range d.Plan(i, "edit") {
		if p.Slot.Key == fileKey && p.Fixed {
			target = p.Slot.Value
		}
	}
	note := "The change was different every run, so it is not replayed. Make it by hand, or give it to the agent as this step."
	line := fmt.Sprintf(`echo "HUMAN STEP %d: %s%s" >&2`, n, label, map[bool]string{true: " " + target, false: ""}[target != ""])
	if target != "" && !d.Files[target] {
		d.Files[target] = true
		d.Mf.Files = append(d.Mf.Files, manifest.File{Path: target, Access: "write"})
	}
	d.HumanCnt++
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), "# "+note, line)
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindHuman, Label: label, Line: line, Note: note})
}

func (d *Drafter) Builtin(i, n int, label string) {
	switch {
	case trace.ReadTools[label]:
		for _, p := range d.Plan(i, "read") {
			if p.Slot.Type != trace.SlotPath {
				continue
			}
			arg := ShellQuote(p.Slot.Value)
			if !p.Fixed {
				arg = `"` + d.Ref(p.Input) + `"`
			}
			d.FileAccess(p, n)
			key := "cat\x00\x00*\x00read"
			if !d.Cmds[key] {
				d.Cmds[key] = true
				d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: "cat", Args: []string{"*"}, Effect: d.Effect(n)})
			}
			line := "cat " + arg
			d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s (read a file)", n, label), line)
			if p.Fixed {
				d.FixedCnt++
			}
			d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: d.Effect(n)})
			return
		}
	case trace.EditTools[label]:
		key := "file_path"
		if label == "edit_file_v2" {
			key = "relativeWorkspacePath"
		}
		d.Human(i, n, label, key)
		return
	case trace.FetchTools[label]:
		for _, p := range d.Plan(i, "fetch") {
			if p.Slot.Key != "url" {
				continue
			}
			u, err := url.Parse(p.Slot.Value)
			if err != nil || u.Host == "" {
				break
			}
			origin := u.Scheme + "://" + u.Host
			if !d.Origins[origin] {
				d.Origins[origin] = true
				d.Mf.Fetch = append(d.Mf.Fetch, manifest.Fetch{Origin: origin})
			}
			arg := ShellQuote(p.Slot.Value)
			if !p.Fixed {
				arg = `"` + d.Ref(p.Input) + `"`
			}
			line := "tap fetch " + arg
			note := ""
			if !p.Fixed {
				note = "Only " + origin + " is declared; a URL on another site will be refused."
			}
			d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), line)
			d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindFetch, Label: label, Line: line, Effect: "read", Note: note})
			return
		}
	}
	note := "The agent's own bookkeeping (planning, searching its tools, typing into a terminal it opened): nothing to replay."
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s: not replayed. %s", n, label, note))
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindSkipped, Label: label, Note: note})
}

func DraftReadme(name, desc string, d *Drafter) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n**Status: unvalidated.** This package has never been executed. Its steps were drafted from recorded sessions; passing publish checks is not validation. Run it on fresh inputs with an independent check of the result before relying on it.\n\n%s\n\nDrafted by `telara tap discover` from recorded sessions on this machine. Review it before you run or publish it.\n\n", name, desc)
	b.WriteString("## Inputs\n\n")
	if len(d.Inputs) == 0 {
		b.WriteString("None: every value was the same in every recorded run.\n")
	}
	for _, in := range d.Inputs {
		if in.Sensitive {
			fmt.Fprintf(&b, "- `$%d` **%s**: a credential, from %s. Supply it from your own configuration; its recorded value was not kept.\n", in.Position, in.Name, in.From)
		} else if in.Extract != "" {
			fmt.Fprintf(&b, "- **%s** (%s), from %s: not an argument. In every recorded run step %d's output held it after the same text, so the program takes it from there (%s `%s`, exactly one match or it stops).\n", in.Name, in.Type, in.From, in.DerivedFrom, in.Binding, in.Extract)
		} else if in.DerivedFrom > 0 {
			fmt.Fprintf(&b, "- `$%d` **%s** (%s), from %s: in the recorded runs step %d's output supplied it. Take it from there; the caller should not have to.\n", in.Position, in.Name, in.Type, in.From, in.DerivedFrom)
		} else {
			fmt.Fprintf(&b, "- `$%d` **%s** (%s), from %s.\n", in.Position, in.Name, in.Type, in.From)
		}
	}
	b.WriteString("\n## Steps\n\n")
	for _, s := range d.Steps {
		eff := ""
		if s.Effect != "" {
			eff = " (" + s.Effect + ")"
		}
		fmt.Fprintf(&b, "%d. **%s** %s%s\n", s.N, s.Kind, s.Label, eff)
		if s.Line != "" {
			fmt.Fprintf(&b, "   ```\n   %s\n   ```\n", s.Line)
		}
		if s.Note != "" {
			fmt.Fprintf(&b, "   %s\n", s.Note)
		}
	}
	if d.HumanCnt > 0 {
		fmt.Fprintf(&b, "\n%d step(s) are human steps: the content differed every run, so the draft stops there instead of guessing.\n", d.HumanCnt)
	}
	b.WriteString("\nEffects are `write` (the runner asks before each) unless a step was marked read-only in review.\n")
	return b.String()
}

var NameWord = regexp.MustCompile(`[a-z0-9]+`)

// DraftName joins the steps' words (git add commit push), keeping the first
// time each appears, into a manifest name.
func DraftName(c model.Candidate) string {
	seen := map[string]bool{}
	var words []string
	for _, s := range c.Steps {
		l := strings.ToLower(s.Label)
		for _, p := range []string{"sh:", "mcp:", "js:", "patch:", "telara_"} {
			l = strings.ReplaceAll(l, p, "")
		}
		for _, w := range NameWord.FindAllString(l, -1) {
			if !seen[w] {
				seen[w] = true
				words = append(words, w)
			}
		}
	}
	name := strings.Join(words, "-")
	if len(name) > 48 {
		name = strings.TrimRight(name[:48], "-")
	}
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "routine-" + name
	}
	return name
}

func ClientList(m map[string]int) string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// ArgSchema is the JSON schema of one recorded tool argument: its recorded
// JSON type, so the contract asks for what the tool was actually sent.
func ArgSchema(sl trace.Slot) map[string]any {
	if !sl.Raw {
		return map[string]any{"type": "string"}
	}
	var v any
	if json.Unmarshal([]byte(sl.Value), &v) == nil {
		switch v.(type) {
		case float64:
			return map[string]any{"type": "number"}
		case bool:
			return map[string]any{"type": "boolean"}
		case []any:
			return map[string]any{"type": "array"}
		case map[string]any:
			return map[string]any{"type": "object"}
		}
	}
	return map[string]any{}
}

// JsonLiteral writes a fixed argument as the JSON it was recorded as.
func JsonLiteral(sl trace.Slot) string {
	if sl.Raw && json.Valid([]byte(sl.Value)) {
		var c bytes.Buffer
		if json.Compact(&c, []byte(sl.Value)) == nil {
			return c.String()
		}
		return sl.Value
	}
	b, _ := json.Marshal(sl.Value)
	return string(b)
}

var JqIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func JqKey(k string) string {
	if JqIdent.MatchString(k) {
		return k
	}
	return strconv.Quote(k)
}

var CapChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

// CapName writes a tool's name as the provider.resource.verb capability the
// runner binds (tap-runtime bind.Candidates): the first word is the
// provider, the first word that is a known verb (else the last) the verb,
// and the words between the resource (the provider again when none are
// left). gmail_search_emails -> gmail.emails.search.
func CapName(tool string) string {
	n := strings.Trim(CapChars.ReplaceAllString(strings.ToLower(tool), "_"), "_.-")
	words := strings.FieldsFunc(n, func(r rune) bool { return r == '_' || r == '.' || r == '-' })
	if len(words) == 0 {
		return "tool.tool.run"
	}
	if words[0][0] < 'a' || words[0][0] > 'z' {
		words[0] = "t" + words[0]
	}
	provider := words[0]
	rest := words[1:]
	verbAt := -1
	for k, w := range rest {
		if trace.ReadVerbs[w] || trace.WriteVerbs[w] {
			verbAt = k
			break
		}
	}
	if verbAt < 0 {
		verbAt = len(rest) - 1
	}
	if verbAt < 0 {
		return provider + "." + provider + ".run"
	}
	verb := rest[verbAt]
	var res []string
	res = append(res, rest[:verbAt]...)
	res = append(res, rest[verbAt+1:]...)
	resource := strings.Join(res, "_")
	if resource == "" {
		resource = provider
	}
	return provider + "." + resource + "." + verb
}

var SafeShell = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func ShellQuote(s string) string {
	if SafeShell.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var NonName = regexp.MustCompile(`[^a-z0-9_]+`)

func SanitizeName(s string) string {
	n := strings.Trim(NonName.ReplaceAllString(strings.ToLower(s), "_"), "_")
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "in_" + n
	}
	return n
}

func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func OrNone(keys []string) string {
	if len(keys) == 0 {
		return "no arguments"
	}
	return strings.Join(keys, ", ")
}

func AnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func FixedShare(d *Drafter) float64 {
	fixed := d.FixedArg
	for _, s := range d.Steps {
		if s.Kind != KindSkipped && s.Kind != KindHuman {
			fixed++
		}
	}
	if fixed+len(d.Inputs) == 0 {
		return 0
	}
	return float64(fixed) / float64(fixed+len(d.Inputs))
}

// SameCall reports that steps i and j came from the same recorded call in
// every occurrence.
func SameCall(occ [][]trace.Step, i, j int) bool {
	for _, o := range occ {
		if o[j].Raw == "" || o[j].Call != o[i].Call {
			return false
		}
	}
	return true
}

func SlotValue(st trace.Step, key string) string {
	for _, sl := range st.Slots {
		if sl.Key == key {
			return sl.Value
		}
	}
	return ""
}

// compound drafts steps i..j-1, which every run made as one command line,
// as that line. Each run's line is cut into words at their exact positions;
// runs whose lines have the same structure (the same text between the same
// number of words) are compared word by word, and a word that differs
// becomes an input: a cd target, a redirect target and an environment value
// included. A heredoc body that differs cannot be an input. If fewer than
// half the runs share one structure, or a body differs, the step is written
// as needing authoring rather than guessed.
func (d *Drafter) Compound(i, j int) {
	n := len(d.Steps) + 1
	var labels []string
	for k := i; k < j; k++ {
		labels = append(labels, d.Occ[0][k].Label)
	}
	label := strings.Join(labels, " + ")
	human := func(why string) {
		note := "Recorded as one command line, but " + why + ". Write this step by hand."
		line := fmt.Sprintf(`echo "HUMAN STEP %d: %s" >&2`, n, label)
		d.HumanCnt++
		d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), "# "+note, line)
		d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindHuman, Label: label, Line: line, Note: note})
	}
	type cut struct {
		raw   string
		words []shellparse.Span
	}
	byShape := map[string][]int{}
	cuts := make([]cut, len(d.Occ))
	for o := range d.Occ {
		raw := d.Occ[o][i].Raw
		ws := shellparse.WordSpans(raw)
		cuts[o] = cut{raw, ws}
		byShape[shellparse.ShapeOf(raw, ws)] = append(byShape[shellparse.ShapeOf(raw, ws)], o)
	}
	best := ""
	for k, os := range byShape {
		if len(os) > len(byShape[best]) || (len(os) == len(byShape[best]) && k < best) {
			best = k
		}
	}
	reps := byShape[best]
	if 2*len(reps) < len(d.Occ) {
		human(fmt.Sprintf("only %d of %d runs share its structure", len(reps), len(d.Occ)))
		return
	}
	// A line is the routine's only when separate sessions wrote it: runs that
	// agree only within one session are one piece of work repeated there.
	sessions := map[string]bool{}
	for _, o := range reps {
		sessions[d.Occ[o][i].Session] = true
	}
	if len(sessions) < 2 {
		human(fmt.Sprintf("the %d runs that share its structure all come from one session", len(reps)))
		return
	}
	first := cuts[reps[0]]
	prog := strings.ReplaceAll(strings.TrimPrefix(labels[0], "sh:"), " ", "_")
	var out strings.Builder
	last := 0
	for p, w := range first.words {
		vec := map[int]string{}
		same := true
		sensitive := false
		for _, o := range reps {
			v := cuts[o].words[p].Text
			vec[o] = v
			if v != w.Text {
				same = false
			}
			if redact.SecretShape(v) != "" {
				sensitive = true
			}
		}
		if p > 0 && redact.SensitiveName.MatchString(strings.TrimSuffix(first.words[p-1].Text, "=")) {
			sensitive = true
		}
		if same && !sensitive {
			continue
		}
		if w.Body {
			human("its heredoc body differs between runs")
			return
		}
		name := prog + "_arg" + util.Itoa(p)
		if p > 0 && strings.HasPrefix(first.words[p-1].Text, "-") {
			name = prog + "_" + strings.TrimLeft(first.words[p-1].Text, "-")
		}
		in := d.Input(name, trace.Slot{Key: "w" + util.Itoa(p), Type: trace.TypeOf(shellparse.Word{Text: w.Text, Quoted: w.Quoted}), Value: w.Text}, vec, sensitive, i)
		out.WriteString(first.raw[last:w.S])
		out.WriteString(`"` + d.Ref(in) + `"`)
		last = w.E
	}
	out.WriteString(first.raw[last:])
	line := out.String()

	eff := d.Effect(n)
	for _, ws := range shellparse.SimpleCommands(line) {
		prog := ws[0].Text
		if !ManifestCommand.MatchString(prog) {
			continue
		}
		key := prog + "\x00\x00*\x00" + eff
		if !d.Cmds[key] {
			d.Cmds[key] = true
			d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: prog, Args: []string{"*"}, Effect: eff})
		}
	}
	note := ""
	if len(reps) < len(d.Occ) {
		note = fmt.Sprintf("%d of %d runs used a command line of exactly this structure.", len(reps), len(d.Occ))
	}
	d.FixedCnt++
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s (one recorded command line)", n, label), line)
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: eff, Note: note})
}

var ManifestCommand = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]{0,63}$`)

// derivedFrom returns the position of the earlier step whose output held
// this value in at least half the runs, or -1.
func (d *Drafter) DerivedFrom(pos int, vec map[int]string) int {
	count := map[int]int{}
	n := 0
	for j, v := range vec {
		if v == "" || j >= len(d.Occ) {
			continue
		}
		n++
	found:
		for h := 0; h < pos && h < len(d.Occ[j]); h++ {
			for _, id := range d.Occ[j][h].OutIDs {
				if id == v {
					count[h]++
					break found
				}
			}
			// A name that is not identifier-shaped (a pod, a branch) is
			// still taken from a result when the result shows it.
			if trace.InResult(v, d.Occ[j][h]) {
				count[h]++
				break found
			}
		}
	}
	best, bestN := -1, 0
	for h, c := range count {
		if c > bestN || (c == bestN && h < best) {
			best, bestN = h, c
		}
	}
	if n == 0 || 2*bestN < n {
		return -1
	}
	return best
}

// extraction finds how to take a value back out of step h's output: the text
// just before it, common to every run where step h's output held it, and the
// character that ended it. It returns a grep -o pattern matching that text
// and the value, and the text to strip from the match.
func (d *Drafter) Extraction(h int, vec map[int]string) (pattern, strip, binding string, ok bool) {
	// A JSON result is read by its path. When every run found the value at
	// one and the same path, bind that path. When runs found it at
	// different paths (the first result one time, the second another) the
	// choice was the agent's: bind nothing, and do not fall back to text.
	paths := map[string]bool{}
	located, total := 0, 0
	for j, v := range vec {
		if v == "" || j >= len(d.Occ) || h >= len(d.Occ[j]) {
			continue
		}
		st := d.Occ[j][h]
		for k, id := range st.OutIDs {
			if id != v {
				continue
			}
			total++
			if k < len(st.OutPaths) && st.OutPaths[k] != "" && st.OutPaths[k] != "*" {
				paths[st.OutPaths[k]] = true
				located++
			}
			break
		}
	}
	if located >= 2 && len(paths) > 1 {
		return "", "", "", false
	}
	if located >= 2 && located == total && len(paths) == 1 {
		for p := range paths {
			if !strings.ContainsAny(p, "'\\") {
				return p, "", "json_path", true
			}
		}
	}
	var befores []string
	after, first := "", true
	for j, v := range vec {
		if v == "" || j >= len(d.Occ) || h >= len(d.Occ[j]) {
			continue
		}
		st := d.Occ[j][h]
		for k, id := range st.OutIDs {
			if id != v || k >= len(st.OutCtx) {
				continue
			}
			b, a, _ := strings.Cut(st.OutCtx[k], "\x00")
			if !first && a != after {
				return "", "", "", false
			}
			after, first = a, false
			befores = append(befores, b)
			break
		}
	}
	if len(befores) < 2 {
		return "", "", "", false
	}
	common := befores[0]
	for _, b := range befores[1:] {
		n := 0
		for n < len(common) && n < len(b) && common[len(common)-1-n] == b[len(b)-1-n] {
			n++
		}
		common = common[len(common)-n:]
	}
	for len(common) > 0 && !utf8.RuneStart(common[0]) {
		common = common[1:]
	}
	// The anchor must say something ("Task ID: `", "\"id\":\"") and fit in
	// a single-quoted shell word; the value must end at a delimiter. Text
	// with a backslash was recorded escaped (a JSON-encoded result) and will
	// not read the same in the live output.
	if len(strings.TrimSpace(common)) < 3 || strings.ContainsAny(common+after, "'\n\\") {
		return "", "", "", false
	}
	if after != "" {
		if r, _ := utf8.DecodeRuneInString(after); unicode.IsLetter(r) || unicode.IsDigit(r) {
			return "", "", "", false
		}
	}
	class := "[^[:space:]]*"
	if after != "" {
		class = "[^" + after + "[:space:]]*"
		if after == "]" {
			class = "[^][:space:]]*"
		}
	}
	return BreQuote(common) + class, common, "text_anchor", true
}

// BreQuote escapes s for a basic regular expression.
func BreQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.*[]^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// capturable reports whether draft step n runs something whose output a later
// step can read: a command, a tool call, a browser script or a fetch.
func (d *Drafter) Capturable(n int) bool {
	if n < 1 || n > len(d.Steps) {
		return false
	}
	switch d.Steps[n-1].Kind {
	case KindCommand, KindTool, KindBrowser, KindFetch:
		return true
	}
	return false
}

// finish numbers the caller's arguments, resolves every input placeholder,
// and makes each step whose output supplies a later value keep that output
// and take the value from it.
func (d *Drafter) Finish() {
	pos := 0
	refs := make([]string, len(d.Inputs))
	byStep := map[int][]int{}
	for k := range d.Inputs {
		in := &d.Inputs[k]
		if in.Extract != "" {
			refs[k] = "${" + in.Name + "}"
			byStep[in.DerivedFrom] = append(byStep[in.DerivedFrom], k)
			continue
		}
		pos++
		in.Position = pos
		refs[k] = "${" + util.Itoa(pos) + "}"
	}
	resolve := func(l string) string {
		if !strings.Contains(l, "\x01") {
			return l
		}
		parts := strings.Split(l, "\x01")
		for i := 1; i < len(parts); i += 2 {
			if k, err := strconv.Atoi(parts[i]); err == nil && k < len(refs) {
				parts[i] = refs[k]
			}
		}
		return strings.Join(parts, "")
	}
	for i := range d.Steps {
		d.Steps[i].Line = resolve(d.Steps[i].Line)
	}
	var out []string
	step := 0
	for _, l := range d.Lines {
		l = resolve(l)
		if strings.HasPrefix(l, "# ") {
			if n, err := strconv.Atoi(strings.SplitN(l[2:], ".", 2)[0]); err == nil {
				step = n
			}
		}
		ks := byStep[step]
		if len(ks) == 0 || step < 1 || l != d.Steps[step-1].Line {
			out = append(out, l)
			continue
		}
		v := "out" + util.Itoa(step)
		out = append(out, v+"=$("+l+")", `printf '%s\n' "$`+v+`"`)
		for _, k := range ks {
			in := d.Inputs[k]
			fail := func(why string) string {
				return fmt.Sprintf(`{ echo "step %d's output %s %s" >&2; exit 1; }`, step, why, in.Name)
			}
			if in.Binding == "json_path" {
				// The guest's jq takes -r and a filter only: an absent value
				// prints "null" (or nothing), which stops the program.
				out = append(out,
					fmt.Sprintf(`%s=$(printf '%%s\n' "$%s" | jq -r '%s')`, in.Name, v, in.Extract),
					fmt.Sprintf(`[ -n "$%s" ] && [ "$%s" != null ] || %s`, in.Name, in.Name, fail("has no")))
			} else {
				// The anchor must occur exactly once: a missing, repeated or
				// quoted anchor never selects a value by accident.
				d.UsesGrep = true
				m := "m_" + in.Name
				out = append(out,
					fmt.Sprintf(`%s=$(printf '%%s\n' "$%s" | grep -o -e '%s' || true)`, m, v, in.Extract),
					fmt.Sprintf(`[ -n "$%s" ] && [ "$(( $(printf '%%s\n' "$%s" | wc -l) ))" = 1 ] || %s`, m, m, fail("did not hold exactly one")),
					fmt.Sprintf(`%s=${%s#%s}`, in.Name, m, ShellQuote(in.Strip)))
			}
			// Whatever was read must look like an identifier before any
			// later step uses it.
			d.UsesGrep = true
			out = append(out, fmt.Sprintf(`printf '%%s' "$%s" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._:/@+=-]*$' || %s`, in.Name, fail("held no identifier-shaped")))
		}
		delete(byStep, step)
	}
	d.Lines = out
	// grep is a host command in the guest: declare it (read-only) when the
	// program uses it to read a value out of a result.
	if d.UsesGrep && !d.Cmds["grep\x00\x00*\x00read"] {
		d.Cmds["grep\x00\x00*\x00read"] = true
		d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: "grep", Args: []string{"*"}, Effect: "read"})
	}
}

// humanPositions are the template positions whose draft step is a human
// step.
func (d *Drafter) HumanPositions() map[int]bool {
	out := map[int]bool{}
	for pos, n := range d.PosStep {
		if n >= 1 && n <= len(d.Steps) && d.Steps[n-1].Kind == KindHuman {
			out[pos] = true
		}
	}
	return out
}

// JsonHelpers are defined once at the top of a draft that needs them.
const JsonHelpers = `json_str() { s=$1; s=${s//\\/\\\\}; s=${s//\"/\\\"}; s=${s//$'\n'/\\n}; s=${s//$'\t'/\\t}; s=${s//$'\r'/\\r}; printf '"%s"' "$s"; }
json_raw() { printf '%s' "$1" | jq .; }`

// jsonAssign encodes input k into shell variable v: a JSON string, or for a
// value recorded as JSON, the value checked by jq (a malformed one stops
// the program).
func (d *Drafter) JsonAssign(v string, k int) string {
	d.UsesJSON = true
	if d.Inputs[k].Raw {
		return fmt.Sprintf(`%s=$(json_raw "%s")`, v, d.Ref(k))
	}
	return fmt.Sprintf(`%s=$(json_str "%s")`, v, d.Ref(k))
}

// JsonObject collects the members of a JSON object built in the shell.
type JsonObject struct {
	Parts []string `json:"-"`
}

func (o *JsonObject) Fixed(key, literal string) {
	o.Parts = append(o.Parts, "'"+strings.ReplaceAll(JsonKeyText(key)+":"+literal, "'", `'\''`)+"'")
}

func (o *JsonObject) Variable(key, v string) {
	o.Parts = append(o.Parts, "'"+strings.ReplaceAll(JsonKeyText(key)+":", "'", `'\''`)+"'"+`"$`+v+`"`)
}

// word is the object as one shell word: quoted fragments joined with ','.
func (o *JsonObject) Word() string {
	if len(o.Parts) == 0 {
		return "'{}'"
	}
	return "'{'" + strings.Join(o.Parts, "','") + "'}'"
}

func JsonKeyText(k string) string {
	b, _ := json.Marshal(k)
	return string(b)
}

// ShellSingle quotes s as one single-quoted shell word.
func ShellSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
