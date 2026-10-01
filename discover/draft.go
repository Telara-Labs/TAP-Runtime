package discover

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

	"gitlab.com/telara-labs/tap-runtime/discover/util"

	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
	"gopkg.in/yaml.v3"
)

// A draft turns a routine the finder qualified into a TAP package: a
// primitive.yaml, the main.sh that replays the steps, and a README. Every
// part is read off the recorded occurrences: what varied between runs is an
// input, what never varied is written in. Nothing is guessed. A step whose
// content was decided afresh each run (the body of an edit) is written as a
// human step rather than invented, and every effect is "write" (the runner
// asks for approval) unless the user marks the step read-only.

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

// DraftStep is one step as the review shows it.
type DraftStep struct {
	N      int    `json:"n"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Line   string `json:"line"`
	Effect string `json:"effect,omitempty"`
	Note   string `json:"note,omitempty"`
}

// DraftInput is one argument of the drafted primitive: $Position in main.sh.
type DraftInput struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Raw inputs are passed as JSON (a number, a boolean), not as a string.
	Raw bool `json:"raw,omitempty"`
	// Example is one recorded value, redacted, for the person reviewing on
	// this machine. It is never written into a generated file.
	Example string `json:"-"`
	// Sensitive inputs are credentials: the caller supplies them from its
	// own configuration and no recorded value is kept.
	Sensitive bool `json:"sensitive,omitempty"`
	// DerivedFrom is the step (1-based) whose output held this value in most
	// runs: the program must take it from there, not from the caller.
	DerivedFrom int `json:"derived_from,omitempty"`
	// Extract, when set, is how the program takes this value from step
	// DerivedFrom's output: a grep pattern for the text before it and the
	// value itself. The caller does not supply it and it has no Position.
	Extract string `json:"extract,omitempty"`
	// Binding says how Extract reads the value: "json_path" (a jq path into
	// a JSON result) or "text_anchor" (the text before it).
	Binding string `json:"binding,omitempty"`
	// List marks a caller-given list the program loops over (a JSON array).
	List bool `json:"list,omitempty"`
	// Position is the argument number the caller passes it as ($1, $2...);
	// 0 for an extracted value.
	Position int    `json:"position"`
	From     string `json:"from"`
	// strip is what Extract's match starts with, removed to leave the value.
	strip string
	// pos is the template position of the step that takes it.
	pos int
}

// Draft is a drafted package and what the review needs to show it.
type Draft struct {
	Name      string            `json:"name"`
	Publisher string            `json:"publisher"`
	Inputs    []DraftInput      `json:"inputs"`
	Steps     []DraftStep       `json:"steps"`
	Files     map[string][]byte `json:"-"`
	// Blocked lists every place a generated file still looks like it holds a
	// credential. While it is non-empty the draft cannot be saved, packaged
	// or published.
	Blocked []string `json:"blocked,omitempty"`
	// Problems is what manifest.PublishProblems reports; empty means the
	// package passes the same checks the registry runs before accepting it.
	Problems   []string `json:"problems"`
	HumanSteps int      `json:"human_steps"`
	// Derived counts inputs an earlier step's output supplied in the
	// recorded runs that the draft could not extract itself: they need
	// authoring. Extracted counts those it takes from that output.
	Derived   int `json:"derived"`
	Extracted int `json:"extracted"`
	// FixedSteps counts steps that pin something: a subcommand, an MCP tool,
	// a constant argument or browser object. None means every command and
	// argument varied: exploration, not a procedure.
	FixedSteps int `json:"fixed_steps"`
	// FixedShare is how much of the routine is fixed: each replayed step's
	// tool or command plus every argument that never changed, over that plus
	// the inputs. A procedure is mostly fixed; an investigation, where the
	// agent chose most values as it went, is mostly inputs.
	FixedShare float64 `json:"fixed_share"`
	values     []map[int]string
	// listLoop names the steps drafted as a loop over a caller list.
	listLoop map[string]bool
	// humanPos are template positions drafted as human steps.
	humanPos map[int]bool
	// priorLoop names steps that loop over a list an earlier result held.
	priorLoop map[string]bool
	// firstRun is the first drafted run's steps, for evidence lookups.
	firstRun []Step
	posStep  map[int]int
	// RuntimeUnsupported names steps the TAP guest runtime would not run
	// as recorded (a built-in that ignores file arguments, a file read the
	// manifest cannot declare). They block a structurally complete draft.
	RuntimeUnsupported []string `json:"runtime_unsupported,omitempty"`
}

// inputValues is input n's value in each drafted run (by run index).
func (d *Draft) inputValues(n int) map[int]string {
	if n < 0 || n >= len(d.values) {
		return nil
	}
	return d.values[n]
}

// DraftOptions are the review's answers.
type DraftOptions struct {
	Publisher string
	// ReadOnly lists step numbers (1-based) the user confirmed change nothing.
	ReadOnly map[int]bool
	// loops are steps each run made once per item of a list the request
	// gave: the varying argument and, per occurrence, its values in order.
	loops map[string]loopSpec
}

type loopSpec struct {
	key    string
	values [][]string // per occurrence (index into occ)
	// source is where the list came from: "caller" (the request gave it)
	// or "prior_output" (an earlier step's result held every item).
	source string
}

// inResult reports whether a value appears in a step's recorded result.
func inResult(v string, st Step) bool {
	if len(v) < 4 || strings.ContainsAny(v, " \n") {
		return false
	}
	for _, id := range st.OutIDs {
		if id == v {
			return true
		}
	}
	for _, t := range st.OutTokens {
		if t == v {
			return true
		}
	}
	return strings.Contains(st.Output, v)
}

// Draft builds the package for Candidates[idx].
func (r *Report) Draft(idx int, opt DraftOptions) (*Draft, error) {
	if idx < 0 || idx >= len(r.Candidates) || r.corpus == nil {
		return nil, errors.New("no such candidate in this run")
	}
	c := r.Candidates[idx]
	if len(c.items) == 0 {
		return nil, errors.New("this candidate cannot be drafted")
	}
	var occ [][]Step
	sessions := make([]int, 0, len(c.sessionSet))
	for s := range c.sessionSet {
		sessions = append(sessions, s)
	}
	sort.Ints(sessions)
	for _, s := range sessions {
		idxs := matchAt(r.seqs[s], c.items, r.window)
		if idxs == nil {
			continue
		}
		steps := make([]Step, len(idxs))
		for i, j := range idxs {
			steps[i] = r.corpus[s].Steps[j]
		}
		occ = append(occ, steps)
	}
	if len(occ) == 0 {
		return nil, errors.New("no occurrence of this candidate could be read back")
	}
	return buildDraft(c, occ, opt), nil
}

// slotPlan is one argument position of one step: a fixed value, or an input.
type slotPlan struct {
	slot  Slot
	fixed bool
	input int // index into inputs when not fixed
}

type drafter struct {
	opt      DraftOptions
	occ      [][]Step
	inputs   []DraftInput
	vectors  []map[int]string // per input: occurrence -> value
	names    map[string]bool
	mf       manifest.Manifest
	cmds     map[string]bool
	files    map[string]bool
	origins  map[string]bool
	tools    map[string]bool
	lines    []string
	steps    []DraftStep
	humanCnt int
	fixedCnt int
	fixedArg int // arguments that had the same value in every run
	// posStep maps a pattern position to the draft step that replays it.
	posStep    map[int]int
	derivedCnt int
	extracted  int
	listLoop   map[string]bool
	priorLoop  map[string]bool
	usesJSON   bool
	usesGrep   bool
	// unsupported names steps the runtime cannot run as recorded.
	unsupported []string
}

// guestReadsStdinOnly are the guest shell's own built-ins that read only
// standard input and ignore file arguments (tap-runtime guest-sh): a
// recorded "wc -l file" would count nothing there.
var guestReadsStdinOnly = map[string]bool{"wc": true, "head": true}

// checkGuest records a step the guest runtime would not run as recorded.
func (d *drafter) checkGuest(prog string, plans []slotPlan, n int) {
	if prog == "cat" {
		for _, p := range plans {
			if !p.slot.Sub && p.slot.Type != SlotFlag {
				d.fileAccess(p, n)
			}
		}
		return
	}
	if !guestReadsStdinOnly[prog] {
		return
	}
	for _, p := range plans {
		// "wc -l file" parses as the flag -l taking "file"; only a number
		// after a flag (head -n 20) is an option's value.
		if p.slot.Sub || p.slot.Type == SlotFlag || p.slot.Type == SlotNumber {
			continue
		}
		d.unsupported = append(d.unsupported, fmt.Sprintf("guest_builtin_ignores_files:step%d:%s", n, prog))
		return
	}
}

// fileAccess declares a fixed file a step reads, so the host lets the
// guest open it; a path that varies cannot be declared ahead of time.
func (d *drafter) fileAccess(p slotPlan, n int) {
	if !p.fixed {
		d.unsupported = append(d.unsupported, fmt.Sprintf("file_access_undeclared:step%d:%s", n, d.inputs[p.input].Name))
		return
	}
	if !d.files[p.slot.Value] {
		d.files[p.slot.Value] = true
		d.mf.Files = append(d.mf.Files, manifest.File{Path: p.slot.Value, Access: "read"})
	}
}

// ref is a placeholder for input k in a generated line. finish replaces it
// with the argument ("${3}") or, for a value taken from an earlier step's
// output, that shell variable.
func (d *drafter) ref(k int) string { return "\x01" + util.Itoa(k) + "\x01" }

func buildDraft(c Candidate, occ [][]Step, opt DraftOptions) *Draft {
	if opt.Publisher == "" {
		opt.Publisher = DefaultPublisher
	}
	d := &drafter{opt: opt, occ: occ, listLoop: map[string]bool{}, priorLoop: map[string]bool{}, posStep: map[int]int{}, names: map[string]bool{}, cmds: map[string]bool{}, files: map[string]bool{}, origins: map[string]bool{}, tools: map[string]bool{}}
	for i := 0; i < len(c.items); {
		label := c.Steps[i].Label
		d.posStep[i] = len(d.steps) + 1
		if strings.HasPrefix(label, "js:") {
			// Consecutive browser calls were one script: they stay one call.
			j := i
			for j < len(c.items) && strings.HasPrefix(c.Steps[j].Label, "js:") {
				d.posStep[j] = len(d.steps) + 1
				j++
			}
			d.browser(i, j)
			i = j
			continue
		}
		if strings.HasPrefix(label, "sh:") && d.occ[0][i].Compound {
			// One recorded command line (a pipeline, a chain, a heredoc)
			// that holds this step and possibly the next ones.
			j := i + 1
			for j < len(c.items) && sameCall(d.occ, i, j) {
				d.posStep[j] = len(d.steps) + 1
				j++
			}
			d.compound(i, j)
			i = j
			continue
		}
		d.step(i, label)
		i++
	}

	d.finish()
	name := draftName(c)
	desc := fmt.Sprintf("Unvalidated draft (never executed). Recurring routine found by tap discover in %d sessions over %d weeks (%s). Steps: %s.",
		c.Sessions, c.Weeks, clientList(c.ByClient), labelsOf(c))
	props := map[string]any{}
	var required []string
	for _, in := range d.inputs {
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
	d.mf.APIVersion, d.mf.Kind = manifest.APIVersion, "Primitive"
	d.mf.Metadata = manifest.Metadata{Publisher: opt.Publisher, Name: name, Version: "0.1.0", Description: desc,
		OutputDescription: "What the steps print, in order."}
	d.mf.Interface = &manifest.Interface{InputSchema: in, OutputSchema: map[string]any{"type": "object"}}
	d.mf.Execution = manifest.Execution{Runtime: manifest.RuntimeWasm, Entrypoint: "main.sh"}
	d.mf.Provenance = &manifest.Provenance{Source: "main.sh", Toolchain: "interpreter obtained by the runner, pinned by sha256", Build: "none: the entrypoint is the source"}

	yml, _ := yaml.Marshal(&d.mf)
	var problems []string
	if m, err := manifest.Parse(yml); err != nil {
		problems = []string{err.Error()}
	} else {
		problems = m.PublishProblems()
	}

	var sh strings.Builder
	fmt.Fprintf(&sh, "# %s\n# Drafted by telara tap discover. Review every line before running or publishing.\n", desc)
	for _, in := range d.inputs {
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
	if d.usesJSON {
		sh.WriteString(jsonHelpers + "\n")
	}
	for _, l := range d.lines {
		sh.WriteString(l)
		sh.WriteByte('\n')
	}

	files := map[string][]byte{
		"primitive.yaml": yml,
		"main.sh":        []byte(sh.String()),
		"README.md":      []byte(draftReadme(name, desc, d)),
	}
	return &Draft{
		Blocked: scanArtifacts(files),
		Name:    name, Publisher: opt.Publisher, Inputs: d.inputs, Steps: d.steps,
		RuntimeUnsupported: d.unsupported,
		listLoop:           d.listLoop, priorLoop: d.priorLoop, humanPos: d.humanPositions(), firstRun: occ[0], posStep: d.posStep,
		Problems: problems, HumanSteps: d.humanCnt, Derived: d.derivedCnt, Extracted: d.extracted, FixedSteps: d.fixedCnt, FixedShare: fixedShare(d), values: d.vectors,
		Files: files,
	}
}

func (d *drafter) effect(n int) string {
	if d.opt.ReadOnly[n] {
		return "read"
	}
	return "write"
}

// derived reports a slot computed from another (a URL's host, a path's base
// name). Derived slots help spot what is fixed; they are never arguments.
func derived(key string) bool {
	return strings.Contains(key, ".") && !strings.HasPrefix(key, "-")
}

// plan decides, for step i, which arguments are fixed and which are inputs.
// It uses the occurrences with the most common skeleton and argument keys,
// and compares values key by key, so arguments given in a different order
// still line up.
func (d *drafter) plan(i int, stepName string) []slotPlan {
	sig := func(st Step) string {
		var ks []string
		for _, sl := range st.Slots {
			if !derived(sl.Key) {
				ks = append(ks, sl.Key)
			}
		}
		sort.Strings(ks)
		return st.Skeleton + "#" + strings.Join(ks, ",")
	}
	count := map[string]int{}
	for _, o := range d.occ {
		count[sig(o[i])]++
	}
	modal, best := "", -1
	for k, v := range count {
		if v > best || (v == best && k < modal) {
			modal, best = k, v
		}
	}
	var reps []int
	for j, o := range d.occ {
		if sig(o[i]) == modal {
			reps = append(reps, j)
		}
	}
	valueOf := func(st Step, key string) string {
		for _, sl := range st.Slots {
			if sl.Key == key {
				return sl.Value
			}
		}
		return ""
	}
	first := d.occ[reps[0]][i]
	var plans []slotPlan
	for _, sl := range first.Slots {
		if derived(sl.Key) {
			continue
		}
		p := slotPlan{slot: sl}
		if sl.Sub {
			p.fixed = true
			plans = append(plans, p)
			continue
		}
		vec := map[int]string{}
		same := true
		for _, j := range reps {
			v := valueOf(d.occ[j][i], sl.Key)
			vec[j] = v
			if v != sl.Value {
				same = false
			}
		}
		// A value that looks like a credential in any recorded run is never
		// written, however constant: it becomes a credential input.
		sensitive := false
		for _, j := range reps {
			for _, other := range d.occ[j][i].Slots {
				if other.Key == sl.Key && sensitiveSlot(d.occ[j][i].Label, other) {
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
		if t, ok := argSchema(sl)["type"].(string); ok && sl.Raw {
			sl.Type = t
		}
		p.slot = sl
		switch {
		case sensitive:
			p.input = d.input(stepName, sl, vec, true, i)
		case same && len(reps) > 1:
			p.fixed = true
			d.fixedArg++
		case sl.Key == "recv":
			// A JS variable the author named differently each run is not an
			// input anyone would pass; the browser step names it itself.
			p.input = -1
		default:
			p.input = d.input(stepName, sl, vec, false, i)
		}
		plans = append(plans, p)
	}
	return plans
}

// input returns the input for a varying slot, reusing an earlier input that
// held the same value in every occurrence both appear in (a path passed to
// gofmt and then to go test is one input, not two).
func (d *drafter) input(stepName string, sl Slot, vec map[int]string, sensitive bool, pos int) int {
	for n, other := range d.vectors {
		if d.inputs[n].Sensitive != sensitive {
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
	case len(key) > 1 && key[0] == 'p' && isDigits(key[1:]):
		base += "_arg" + key[1:]
	case key == "recv":
		base += "_object"
	default:
		base += "_" + strings.TrimLeft(key, "-")
	}
	name := sanitizeName(base)
	for k := 2; d.names[name]; k++ {
		name = fmt.Sprintf("%s_%d", sanitizeName(base), k)
	}
	d.names[name] = true
	in := DraftInput{Name: name, Type: sl.Type, Raw: sl.Raw, Example: Redact(sl.Value), From: stepName, pos: pos}
	if !sensitive {
		if h := d.derivedFrom(pos, vec); h >= 0 {
			in.DerivedFrom = d.posStep[h]
			if pat, strip, binding, ok := d.extraction(h, vec); ok && d.capturable(in.DerivedFrom) {
				in.Extract, in.strip, in.Binding = pat, strip, binding
				d.extracted++
			} else {
				d.derivedCnt++
			}
		}
	}
	if sensitive {
		in.Sensitive, in.Type, in.Example = true, SlotSecret, ""
	}
	d.inputs = append(d.inputs, in)
	d.vectors = append(d.vectors, vec)
	return len(d.inputs) - 1
}

func (d *drafter) step(i int, label string) {
	n := len(d.steps) + 1
	switch {
	case strings.HasPrefix(label, "sh:"):
		d.command(i, n, label)
	case strings.HasPrefix(label, "mcp:"):
		d.tool(i, n, strings.TrimPrefix(label, "mcp:"), label)
	case strings.HasPrefix(label, "patch:"):
		d.human(i, n, label, "file")
	default:
		d.builtin(i, n, label)
	}
}

func (d *drafter) command(i, n int, label string) {
	fields := strings.Fields(strings.TrimPrefix(label, "sh:"))
	prog := fields[0]
	stepName := strings.Join(fields, "_")
	plans := d.plan(i, stepName)
	// A step run once per item of a list the request gave becomes a loop
	// over a list input.
	spec, isLoop := d.opt.loops[label]
	loopIn := -1
	if isLoop && spec.source == "prior_output" {
		// The list came from an earlier result; binding a collection out of
		// a result is not drafted, so this step is marked for authoring.
		d.priorLoop[label] = true
		isLoop = false
	}
	if isLoop {
		for _, p := range plans {
			if p.slot.Key == spec.key && !p.fixed && p.input >= 0 {
				loopIn = p.input
			}
		}
	}
	if loopIn >= 0 {
		in := &d.inputs[loopIn]
		in.List, in.Raw, in.Type, in.Example = true, true, "list", ""
		vec := map[int]string{}
		for j, vs := range spec.values {
			if vs == nil {
				continue
			}
			b, _ := json.Marshal(vs)
			vec[j] = string(b)
		}
		d.vectors[loopIn] = vec
		d.listLoop[label] = true
	}
	d.checkGuest(prog, plans, n)
	words := []string{prog}
	var globals []string
	subSeen, sub := false, ""
	for k, p := range plans {
		switch {
		case p.slot.Sub:
			subSeen, sub = true, p.slot.Value
			words = append(words, p.slot.Value)
		case p.fixed:
			words = append(words, shellQuote(p.slot.Value))
		case p.input == loopIn:
			words = append(words, `"$item"`)
		default:
			words = append(words, `"`+d.ref(p.input)+`"`)
		}
		// Flags before the subcommand are the program's global flags; the
		// word keyed "<flag>=" after one is its value.
		if !subSeen && len(fields) > 1 && p.slot.Type == SlotFlag {
			g := p.slot.Value
			if k+1 < len(plans) && plans[k+1].slot.Key == p.slot.Key+"=" {
				if plans[k+1].fixed {
					g += " " + plans[k+1].slot.Value
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
	eff := d.effect(n)
	key := prog + "\x00" + strings.Join(globals, "\x00") + "\x00" + strings.Join(args, "\x00") + "\x00" + eff
	if !d.cmds[key] {
		d.cmds[key] = true
		d.mf.Commands = append(d.mf.Commands, manifest.Command{Command: prog, Globals: globals, Args: args, Effect: eff})
	}
	line := strings.Join(words, " ")
	if loopIn >= 0 {
		// Each item on its own line from the JSON array; an empty list runs
		// nothing; a malformed list fails the pipeline (pipefail).
		line = fmt.Sprintf(`printf '%%s' "%s" | jq -r '.[]' | while IFS= read -r item; do %s; done`, d.ref(loopIn), line)
		d.lines = append(d.lines, fmt.Sprintf("# %d. %s (once per item of the list; an empty list runs nothing)", n, label), line)
	} else {
		d.lines = append(d.lines, fmt.Sprintf("# %d. %s", n, label), line)
	}
	fixed := len(fields) > 1
	for _, p := range plans {
		if p.fixed && !p.slot.Sub && p.slot.Type != SlotFlag && p.slot.Type != SlotNumber {
			fixed = true
		}
	}
	if fixed {
		d.fixedCnt++
	}
	d.steps = append(d.steps, DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: eff})
}

var nonAlias = regexp.MustCompile(`[^a-z0-9_]+`)

// jsIdentifier matches an argument that is only a variable (url1, row.href):
// a value the recorded script computed, not one written into the call.
var jsIdentifier = regexp.MustCompile(`^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$`)

func (d *drafter) tool(i, n int, tool, label string) {
	alias := strings.Trim(nonAlias.ReplaceAllString(strings.ToLower(tool), "_"), "_")
	plans := d.plan(i, alias)
	props := map[string]any{}
	var keys []string
	var pre []string
	var obj jsonObject
	for _, p := range plans {
		keys = append(keys, p.slot.Key)
		props[p.slot.Key] = argSchema(p.slot)
		if p.fixed {
			obj.fixed(p.slot.Key, jsonLiteral(p.slot))
			continue
		}
		v := fmt.Sprintf("a%d_%d", n, p.input+1)
		pre = append(pre, d.jsonAssign(v, p.input))
		obj.variable(p.slot.Key, v)
	}
	sort.Strings(keys)
	args := map[string]any{"type": "object", "properties": props, "required": anySlice(keys)}
	result := map[string]any{"type": "object"}
	question := fmt.Sprintf("Run %s with %s, as the recorded sessions did.", tool, orNone(keys))
	capLabel := d.opt.Publisher + "/" + capName(tool) + "@1"
	eff := d.effect(n)
	if !d.tools[alias] {
		d.tools[alias] = true
		d.mf.Capabilities = append(d.mf.Capabilities, manifest.Capability{Label: capLabel, ID: manifest.CapabilityID(question, args, result), Question: question, Args: args, Result: result})
		d.mf.Tools = append(d.mf.Tools, manifest.Tool{Alias: alias, Capability: capLabel, Effect: eff})
	}
	line := fmt.Sprintf(`tap call %s %s`, alias, obj.word())
	d.lines = append(d.lines, fmt.Sprintf("# %d. %s", n, label))
	d.lines = append(d.lines, pre...)
	d.lines = append(d.lines, line)
	d.fixedCnt++ // an MCP tool names one action in one system
	d.steps = append(d.steps, DraftStep{N: n, Kind: KindTool, Label: label, Line: line, Effect: eff})
}

// browser writes steps i..j-1 (awaited calls of one script) as one call to
// the browser tool, rebuilding the script with this run's inputs.
func (d *drafter) browser(i, j int) {
	n := len(d.steps) + 1
	var labels, pre []string
	// The script is built as one shell word: fixed JavaScript in single
	// quotes, each input spliced in as a JSON value (a JS literal).
	var code strings.Builder
	varyingObject, browserFixed := false, false
	var scriptVars []string
	for k := i; k < j; k++ {
		method := strings.TrimPrefix(d.occ[0][k].Label, "js:")
		labels = append(labels, method)
		recv, arg := "tab", ""
		for _, p := range d.plan(k, strings.ReplaceAll(method, ".", "_")) {
			switch {
			case p.slot.Key == "recv" && p.fixed:
				recv = p.slot.Value
			case p.slot.Key == "recv":
				varyingObject = true
			case p.slot.Key != "0":
			case p.fixed && p.slot.Raw:
				browserFixed = true
				if jsIdentifier.MatchString(p.slot.Value) {
					scriptVars = append(scriptVars, p.slot.Value)
				}
				arg = shellSingle(p.slot.Value)
			case p.fixed:
				browserFixed = true
				b, _ := json.Marshal(p.slot.Value)
				arg = shellSingle(string(b))
			default:
				v := fmt.Sprintf("a%d_%d", n, p.input+1)
				if p.slot.Raw {
					if jsIdentifier.MatchString(p.slot.Value) {
						scriptVars = append(scriptVars, p.slot.Value)
					}
					// A JavaScript expression the caller supplies, as is.
					pre = append(pre, fmt.Sprintf(`%s="%s"`, v, d.ref(p.input)))
				} else {
					d.usesJSON = true
					pre = append(pre, fmt.Sprintf(`%s=$(json_str "%s")`, v, d.ref(p.input)))
				}
				arg = `"$` + v + `"`
			}
		}
		code.WriteString(shellSingle("await "+recv+"."+method+"(") + arg + shellSingle("); "))
	}
	question := "Run a browser script in the user's open browser session and return what it reports."
	args := map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}}, "required": []any{"code"}}
	result := map[string]any{"type": "object"}
	capLabel := d.opt.Publisher + "/browser.script@1"
	eff := d.effect(n)
	if !d.tools["browser"] {
		d.tools["browser"] = true
		d.mf.Capabilities = append(d.mf.Capabilities, manifest.Capability{Label: capLabel, ID: manifest.CapabilityID(question, args, result), Question: question, Args: args, Result: result})
		d.mf.Tools = append(d.mf.Tools, manifest.Tool{Alias: "browser", Capability: capLabel, Effect: eff})
	}
	cv := fmt.Sprintf("code%d", n)
	pre = append(pre, cv+"="+code.String())
	d.usesJSON = true
	pre = append(pre, fmt.Sprintf(`%s_json=$(json_str "$%s")`, cv, cv))
	var obj jsonObject
	obj.variable("code", cv+"_json")
	line := fmt.Sprintf(`tap call browser %s`, obj.word())
	var notes []string
	if varyingObject {
		notes = append(notes, "Some calls were made on a variable the author named differently each run; the draft calls it tab. Open or select that tab first.")
	}
	if len(scriptVars) > 0 {
		// goto(url1): the value was a variable the original script computed
		// before the call. Extracting the call cannot recover it.
		notes = append(notes, "Uses values the original script computed before these calls ("+strings.Join(scriptVars, ", ")+"); this step needs the rest of that script before it can run.")
		d.humanCnt++
	}
	note := strings.Join(notes, " ")
	label := "browser: " + strings.Join(labels, " → ")
	d.lines = append(d.lines, fmt.Sprintf("# %d. %s", n, label))
	if note != "" {
		d.lines = append(d.lines, "# "+note)
	}
	d.lines = append(d.lines, pre...)
	d.lines = append(d.lines, line)
	if browserFixed {
		d.fixedCnt++
	}
	d.steps = append(d.steps, DraftStep{N: n, Kind: KindBrowser, Label: label, Line: line, Effect: eff, Note: note})
}

func (d *drafter) human(i, n int, label, fileKey string) {
	target := ""
	for _, p := range d.plan(i, "edit") {
		if p.slot.Key == fileKey && p.fixed {
			target = p.slot.Value
		}
	}
	note := "The change was different every run, so it is not replayed. Make it by hand, or give it to the agent as this step."
	line := fmt.Sprintf(`echo "HUMAN STEP %d: %s%s" >&2`, n, label, map[bool]string{true: " " + target, false: ""}[target != ""])
	if target != "" && !d.files[target] {
		d.files[target] = true
		d.mf.Files = append(d.mf.Files, manifest.File{Path: target, Access: "write"})
	}
	d.humanCnt++
	d.lines = append(d.lines, fmt.Sprintf("# %d. %s", n, label), "# "+note, line)
	d.steps = append(d.steps, DraftStep{N: n, Kind: KindHuman, Label: label, Line: line, Note: note})
}

// Agent-builtin tools have no TAP equivalent to bind. Reading a file and
// fetching a page have one-line replays; editing is judgement (a human
// step); the rest is the agent's own bookkeeping.
// replayable reports whether a primitive can run a step with this label
// itself: a host command, an MCP tool, a browser call, a file read or a page
// fetch. Edits decided per run and the agent's bookkeeping cannot.
func replayable(label string) bool {
	for _, p := range []string{"sh:", "mcp:", "js:"} {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return readTools[label] || fetchTools[label]
}

var (
	readTools  = map[string]bool{"Read": true, "read_file": true, "read_file_v2": true}
	editTools  = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "edit_file": true, "edit_file_v2": true, "search_replace": true, "apply_patch": true}
	fetchTools = map[string]bool{"WebFetch": true}
)

func (d *drafter) builtin(i, n int, label string) {
	switch {
	case readTools[label]:
		for _, p := range d.plan(i, "read") {
			if p.slot.Type != SlotPath {
				continue
			}
			arg := shellQuote(p.slot.Value)
			if !p.fixed {
				arg = `"` + d.ref(p.input) + `"`
			}
			d.fileAccess(p, n)
			key := "cat\x00\x00*\x00read"
			if !d.cmds[key] {
				d.cmds[key] = true
				d.mf.Commands = append(d.mf.Commands, manifest.Command{Command: "cat", Args: []string{"*"}, Effect: d.effect(n)})
			}
			line := "cat " + arg
			d.lines = append(d.lines, fmt.Sprintf("# %d. %s (read a file)", n, label), line)
			if p.fixed {
				d.fixedCnt++
			}
			d.steps = append(d.steps, DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: d.effect(n)})
			return
		}
	case editTools[label]:
		key := "file_path"
		if label == "edit_file_v2" {
			key = "relativeWorkspacePath"
		}
		d.human(i, n, label, key)
		return
	case fetchTools[label]:
		for _, p := range d.plan(i, "fetch") {
			if p.slot.Key != "url" {
				continue
			}
			u, err := url.Parse(p.slot.Value)
			if err != nil || u.Host == "" {
				break
			}
			origin := u.Scheme + "://" + u.Host
			if !d.origins[origin] {
				d.origins[origin] = true
				d.mf.Fetch = append(d.mf.Fetch, manifest.Fetch{Origin: origin})
			}
			arg := shellQuote(p.slot.Value)
			if !p.fixed {
				arg = `"` + d.ref(p.input) + `"`
			}
			line := "tap fetch " + arg
			note := ""
			if !p.fixed {
				note = "Only " + origin + " is declared; a URL on another site will be refused."
			}
			d.lines = append(d.lines, fmt.Sprintf("# %d. %s", n, label), line)
			d.steps = append(d.steps, DraftStep{N: n, Kind: KindFetch, Label: label, Line: line, Effect: "read", Note: note})
			return
		}
	}
	note := "The agent's own bookkeeping (planning, searching its tools, typing into a terminal it opened): nothing to replay."
	d.lines = append(d.lines, fmt.Sprintf("# %d. %s: not replayed. %s", n, label, note))
	d.steps = append(d.steps, DraftStep{N: n, Kind: KindSkipped, Label: label, Note: note})
}

func draftReadme(name, desc string, d *drafter) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n**Status: unvalidated.** This package has never been executed. Its steps were drafted from recorded sessions; passing publish checks is not validation. Run it on fresh inputs with an independent check of the result before relying on it.\n\n%s\n\nDrafted by `telara tap discover` from recorded sessions on this machine. Review it before you run or publish it.\n\n", name, desc)
	b.WriteString("## Inputs\n\n")
	if len(d.inputs) == 0 {
		b.WriteString("None: every value was the same in every recorded run.\n")
	}
	for _, in := range d.inputs {
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
	for _, s := range d.steps {
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
	if d.humanCnt > 0 {
		fmt.Fprintf(&b, "\n%d step(s) are human steps: the content differed every run, so the draft stops there instead of guessing.\n", d.humanCnt)
	}
	b.WriteString("\nEffects are `write` (the runner asks before each) unless a step was marked read-only in review.\n")
	return b.String()
}

var nameWord = regexp.MustCompile(`[a-z0-9]+`)

// draftName joins the steps' words (git add commit push), keeping the first
// time each appears, into a manifest name.
func draftName(c Candidate) string {
	seen := map[string]bool{}
	var words []string
	for _, s := range c.Steps {
		l := strings.ToLower(s.Label)
		for _, p := range []string{"sh:", "mcp:", "js:", "patch:", "telara_"} {
			l = strings.ReplaceAll(l, p, "")
		}
		for _, w := range nameWord.FindAllString(l, -1) {
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

func clientList(m map[string]int) string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// argSchema is the JSON schema of one recorded tool argument: its recorded
// JSON type, so the contract asks for what the tool was actually sent.
func argSchema(sl Slot) map[string]any {
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

// jsonLiteral writes a fixed argument as the JSON it was recorded as.
func jsonLiteral(sl Slot) string {
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

var jqIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func jqKey(k string) string {
	if jqIdent.MatchString(k) {
		return k
	}
	return strconv.Quote(k)
}

var capChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

// capName writes a tool's name as the provider.resource.verb capability the
// runner binds (tap-runtime bind.Candidates): the first word is the
// provider, the first word that is a known verb (else the last) the verb,
// and the words between the resource (the provider again when none are
// left). gmail_search_emails -> gmail.emails.search.
func capName(tool string) string {
	n := strings.Trim(capChars.ReplaceAllString(strings.ToLower(tool), "_"), "_.-")
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
		if readVerbs[w] || writeVerbs[w] {
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

var safeShell = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if safeShell.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var nonName = regexp.MustCompile(`[^a-z0-9_]+`)

func sanitizeName(s string) string {
	n := strings.Trim(nonName.ReplaceAllString(strings.ToLower(s), "_"), "_")
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "in_" + n
	}
	return n
}

func isDigits(s string) bool {
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

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return truncateUTF8(s, n) + "…"
	}
	return s
}

func orNone(keys []string) string {
	if len(keys) == 0 {
		return "no arguments"
	}
	return strings.Join(keys, ", ")
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// jqCall is the jq command that builds a JSON argument, quoted for bash:
// the program goes in single quotes, so a single quote inside it (common in
// a CSS selector or a message) is closed, escaped and reopened.

func fixedShare(d *drafter) float64 {
	fixed := d.fixedArg
	for _, s := range d.steps {
		if s.Kind != KindSkipped && s.Kind != KindHuman {
			fixed++
		}
	}
	if fixed+len(d.inputs) == 0 {
		return 0
	}
	return float64(fixed) / float64(fixed+len(d.inputs))
}

// ErrBlocked is returned when a draft still holds something credential-shaped.
var ErrBlocked = errors.New("the draft still contains credential-shaped values; it cannot be saved or published until they are removed")

// Artifacts returns the draft's files, or ErrBlocked. Everything that writes
// or sends a draft goes through it.
func (d *Draft) Artifacts() (map[string][]byte, error) {
	if len(d.Blocked) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrBlocked, strings.Join(d.Blocked, "; "))
	}
	return d.Files, nil
}

// sameCall reports that steps i and j came from the same recorded call in
// every occurrence.
func sameCall(occ [][]Step, i, j int) bool {
	for _, o := range occ {
		if o[j].Raw == "" || o[j].Call != o[i].Call {
			return false
		}
	}
	return true
}

func slotValue(st Step, key string) string {
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
func (d *drafter) compound(i, j int) {
	n := len(d.steps) + 1
	var labels []string
	for k := i; k < j; k++ {
		labels = append(labels, d.occ[0][k].Label)
	}
	label := strings.Join(labels, " + ")
	human := func(why string) {
		note := "Recorded as one command line, but " + why + ". Write this step by hand."
		line := fmt.Sprintf(`echo "HUMAN STEP %d: %s" >&2`, n, label)
		d.humanCnt++
		d.lines = append(d.lines, fmt.Sprintf("# %d. %s", n, label), "# "+note, line)
		d.steps = append(d.steps, DraftStep{N: n, Kind: KindHuman, Label: label, Line: line, Note: note})
	}
	type cut struct {
		raw   string
		words []span
	}
	byShape := map[string][]int{}
	cuts := make([]cut, len(d.occ))
	for o := range d.occ {
		raw := d.occ[o][i].Raw
		ws := wordSpans(raw)
		cuts[o] = cut{raw, ws}
		byShape[shapeOf(raw, ws)] = append(byShape[shapeOf(raw, ws)], o)
	}
	best := ""
	for k, os := range byShape {
		if len(os) > len(byShape[best]) || (len(os) == len(byShape[best]) && k < best) {
			best = k
		}
	}
	reps := byShape[best]
	if 2*len(reps) < len(d.occ) {
		human(fmt.Sprintf("only %d of %d runs share its structure", len(reps), len(d.occ)))
		return
	}
	// A line is the routine's only when separate sessions wrote it: runs that
	// agree only within one session are one piece of work repeated there.
	sessions := map[string]bool{}
	for _, o := range reps {
		sessions[d.occ[o][i].Session] = true
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
			v := cuts[o].words[p].text
			vec[o] = v
			if v != w.text {
				same = false
			}
			if secretShape(v) != "" {
				sensitive = true
			}
		}
		if p > 0 && sensitiveName.MatchString(strings.TrimSuffix(first.words[p-1].text, "=")) {
			sensitive = true
		}
		if same && !sensitive {
			continue
		}
		if w.body {
			human("its heredoc body differs between runs")
			return
		}
		name := prog + "_arg" + util.Itoa(p)
		if p > 0 && strings.HasPrefix(first.words[p-1].text, "-") {
			name = prog + "_" + strings.TrimLeft(first.words[p-1].text, "-")
		}
		in := d.input(name, Slot{Key: "w" + util.Itoa(p), Type: typeOf(word{Text: w.text, Quoted: w.quoted}), Value: w.text}, vec, sensitive, i)
		out.WriteString(first.raw[last:w.s])
		out.WriteString(`"` + d.ref(in) + `"`)
		last = w.e
	}
	out.WriteString(first.raw[last:])
	line := out.String()

	eff := d.effect(n)
	for _, ws := range simpleCommands(line) {
		prog := ws[0].Text
		if !manifestCommand.MatchString(prog) {
			continue
		}
		key := prog + "\x00\x00*\x00" + eff
		if !d.cmds[key] {
			d.cmds[key] = true
			d.mf.Commands = append(d.mf.Commands, manifest.Command{Command: prog, Args: []string{"*"}, Effect: eff})
		}
	}
	note := ""
	if len(reps) < len(d.occ) {
		note = fmt.Sprintf("%d of %d runs used a command line of exactly this structure.", len(reps), len(d.occ))
	}
	d.fixedCnt++
	d.lines = append(d.lines, fmt.Sprintf("# %d. %s (one recorded command line)", n, label), line)
	d.steps = append(d.steps, DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: eff, Note: note})
}

var manifestCommand = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]{0,63}$`)

// derivedFrom returns the position of the earlier step whose output held
// this value in at least half the runs, or -1.
func (d *drafter) derivedFrom(pos int, vec map[int]string) int {
	count := map[int]int{}
	n := 0
	for j, v := range vec {
		if v == "" || j >= len(d.occ) {
			continue
		}
		n++
	found:
		for h := 0; h < pos && h < len(d.occ[j]); h++ {
			for _, id := range d.occ[j][h].OutIDs {
				if id == v {
					count[h]++
					break found
				}
			}
			// A name that is not identifier-shaped (a pod, a branch) is
			// still taken from a result when the result shows it.
			if inResult(v, d.occ[j][h]) {
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
func (d *drafter) extraction(h int, vec map[int]string) (pattern, strip, binding string, ok bool) {
	// A JSON result is read by its path. When every run found the value at
	// one and the same path, bind that path. When runs found it at
	// different paths (the first result one time, the second another) the
	// choice was the agent's: bind nothing, and do not fall back to text.
	paths := map[string]bool{}
	located, total := 0, 0
	for j, v := range vec {
		if v == "" || j >= len(d.occ) || h >= len(d.occ[j]) {
			continue
		}
		st := d.occ[j][h]
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
		if v == "" || j >= len(d.occ) || h >= len(d.occ[j]) {
			continue
		}
		st := d.occ[j][h]
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
	return breQuote(common) + class, common, "text_anchor", true
}

// breQuote escapes s for a basic regular expression.
func breQuote(s string) string {
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
func (d *drafter) capturable(n int) bool {
	if n < 1 || n > len(d.steps) {
		return false
	}
	switch d.steps[n-1].Kind {
	case KindCommand, KindTool, KindBrowser, KindFetch:
		return true
	}
	return false
}

// finish numbers the caller's arguments, resolves every input placeholder,
// and makes each step whose output supplies a later value keep that output
// and take the value from it.
func (d *drafter) finish() {
	pos := 0
	refs := make([]string, len(d.inputs))
	byStep := map[int][]int{}
	for k := range d.inputs {
		in := &d.inputs[k]
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
	for i := range d.steps {
		d.steps[i].Line = resolve(d.steps[i].Line)
	}
	var out []string
	step := 0
	for _, l := range d.lines {
		l = resolve(l)
		if strings.HasPrefix(l, "# ") {
			if n, err := strconv.Atoi(strings.SplitN(l[2:], ".", 2)[0]); err == nil {
				step = n
			}
		}
		ks := byStep[step]
		if len(ks) == 0 || step < 1 || l != d.steps[step-1].Line {
			out = append(out, l)
			continue
		}
		v := "out" + util.Itoa(step)
		out = append(out, v+"=$("+l+")", `printf '%s\n' "$`+v+`"`)
		for _, k := range ks {
			in := d.inputs[k]
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
				d.usesGrep = true
				m := "m_" + in.Name
				out = append(out,
					fmt.Sprintf(`%s=$(printf '%%s\n' "$%s" | grep -o -e '%s' || true)`, m, v, in.Extract),
					fmt.Sprintf(`[ -n "$%s" ] && [ "$(( $(printf '%%s\n' "$%s" | wc -l) ))" = 1 ] || %s`, m, m, fail("did not hold exactly one")),
					fmt.Sprintf(`%s=${%s#%s}`, in.Name, m, shellQuote(in.strip)))
			}
			// Whatever was read must look like an identifier before any
			// later step uses it.
			d.usesGrep = true
			out = append(out, fmt.Sprintf(`printf '%%s' "$%s" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._:/@+=-]*$' || %s`, in.Name, fail("held no identifier-shaped")))
		}
		delete(byStep, step)
	}
	d.lines = out
	// grep is a host command in the guest: declare it (read-only) when the
	// program uses it to read a value out of a result.
	if d.usesGrep && !d.cmds["grep\x00\x00*\x00read"] {
		d.cmds["grep\x00\x00*\x00read"] = true
		d.mf.Commands = append(d.mf.Commands, manifest.Command{Command: "grep", Args: []string{"*"}, Effect: "read"})
	}
}

// humanPositions are the template positions whose draft step is a human
// step.
func (d *drafter) humanPositions() map[int]bool {
	out := map[int]bool{}
	for pos, n := range d.posStep {
		if n >= 1 && n <= len(d.steps) && d.steps[n-1].Kind == KindHuman {
			out[pos] = true
		}
	}
	return out
}

// The guest shell's jq accepts only -r and a filter over stdin, and "tap
// call" takes one JSON object (tap-runtime guest-sh). A draft therefore
// builds a call's arguments itself: each value is encoded into a variable
// first (so a failure stops the program), then one JSON object is written
// as a single shell word.

// jsonHelpers are defined once at the top of a draft that needs them.
const jsonHelpers = `json_str() { s=$1; s=${s//\\/\\\\}; s=${s//\"/\\\"}; s=${s//$'\n'/\\n}; s=${s//$'\t'/\\t}; s=${s//$'\r'/\\r}; printf '"%s"' "$s"; }
json_raw() { printf '%s' "$1" | jq .; }`

// jsonAssign encodes input k into shell variable v: a JSON string, or for a
// value recorded as JSON, the value checked by jq (a malformed one stops
// the program).
func (d *drafter) jsonAssign(v string, k int) string {
	d.usesJSON = true
	if d.inputs[k].Raw {
		return fmt.Sprintf(`%s=$(json_raw "%s")`, v, d.ref(k))
	}
	return fmt.Sprintf(`%s=$(json_str "%s")`, v, d.ref(k))
}

// jsonObject collects the members of a JSON object built in the shell.
type jsonObject struct{ parts []string }

func (o *jsonObject) fixed(key, literal string) {
	o.parts = append(o.parts, "'"+strings.ReplaceAll(jsonKeyText(key)+":"+literal, "'", `'\''`)+"'")
}

func (o *jsonObject) variable(key, v string) {
	o.parts = append(o.parts, "'"+strings.ReplaceAll(jsonKeyText(key)+":", "'", `'\''`)+"'"+`"$`+v+`"`)
}

// word is the object as one shell word: quoted fragments joined with ','.
func (o *jsonObject) word() string {
	if len(o.parts) == 0 {
		return "'{}'"
	}
	return "'{'" + strings.Join(o.parts, "','") + "'}'"
}

func jsonKeyText(k string) string {
	b, _ := json.Marshal(k)
	return string(b)
}

// shellSingle quotes s as one single-quoted shell word.
func shellSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
