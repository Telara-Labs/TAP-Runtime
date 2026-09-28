package compile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"telara.dev/tap/internal/diag"
	"telara.dev/tap/internal/model"
)

// Node id suffixes/sentinels the compiler synthesizes. Step ids are validated
// unique, so these suffixes cannot collide with an authored id.
const (
	endNodeID   = "__end__"
	entryNodeID = "__entry__"
	gateSuffix  = "__gate"
	skipSuffix  = "__skip"
)

// bannedNodeTools always fail at execution (09 §6): the compiler refuses them
// even though they pass static validation.
var bannedNodeTools = []string{"document_parse", "ocr"}

// writeVerbPrefixes mirrors validate's heuristic so tool nodes can carry a
// coarse effect_class (B1 proto node field 19) without a catalog round-trip.
var writeVerbPrefixes = []string{
	"create_", "update_", "delete_", "cancel_", "retry_", "trigger_", "merge_", "close_",
	"transition_", "post_", "send_", "upload_", "invite_", "assign_", "move_", "copy_",
	"archive_", "append_", "remove_", "protect_", "unprotect_", "start_", "add_", "set_",
	"accept_", "decline_", "fork_", "watch_", "unwatch_", "join_", "reindex_",
}

// Compile turns a loaded (and separately validated) package into a
// WorkflowDefinition. It assumes structural validity (the CLI runs
// validate.Validate first and bails on errors); the findings it returns are
// compiler-contract issues (banned node types, unreachable joins from the
// liveness simulation, constructs the current engine cannot execute). A
// non-nil error means the definition could not be produced at all.
func Compile(pkg *model.Package) (*Definition, diag.Findings, error) {
	if pkg.Workflow == nil {
		return nil, nil, fmt.Errorf("workflow.yaml did not load: %v", pkg.WorkflowErr)
	}
	b := &builder{
		pkg:      pkg,
		w:        pkg.Workflow,
		stepIDs:  map[string]bool{},
		isWhen:   map[string]bool{},
		edgeSeen: map[string]bool{},
	}
	def, findings, err := b.build()
	if err != nil {
		return nil, findings, err
	}
	// Liveness simulation over branch combinations BEFORE the caller emits
	// (09 §6): the engine's static validator only checks reachability, while
	// the executor hard-fails an unreachable join as WORKFLOW_DEADLOCK at run
	// time.
	findings = append(findings, simulateLiveness(def)...)
	return def, findings, nil
}

type builder struct {
	pkg *model.Package
	w   *model.Workflow

	stepIDs  map[string]bool
	isWhen   map[string]bool
	dataDeps map[string][]string
	condDeps map[string][]string
	afterDep map[string][]string
	consumer map[string][]string // producer -> consumer step ids

	def      *Definition
	findings diag.Findings
	edgeSeen map[string]bool
}

func (b *builder) build() (*Definition, diag.Findings, error) {
	m := b.pkg.Manifest
	b.def = &Definition{
		Name:        m.Metadata.FullName(),
		Description: m.Metadata.Description,
		Metadata: map[string]string{
			"tap_source":      m.Metadata.FullName(),
			"tap_api_version": b.w.APIVersion,
			"compiled_by":     "tap-compile",
		},
	}
	if m.Effects.Class != "" {
		b.def.Metadata["effect_class"] = m.Effects.Class
	}
	if m.Metadata.Version != "" {
		b.def.Metadata["primitive_version"] = m.Metadata.Version
	}

	for _, s := range b.w.Steps {
		b.stepIDs[s.ID] = true
		if s.HasWhen {
			b.isWhen[s.ID] = true
		}
	}
	b.analyzeDeps()

	// 1) Emit one (or, for when-steps, three) node(s) per step.
	for _, s := range b.w.Steps {
		if err := b.emitStepNodes(s); err != nil {
			return nil, b.findings, err
		}
	}

	// 2) Emit the terminal END node (§6: always).
	b.def.Nodes = append(b.def.Nodes, Node{ID: endNodeID, Type: NodeEnd, Name: "end"})

	// 3) Producer-centric edges: every step routes to entryOf(consumer), a
	//    when-step routes through its gate/skip diamond, and leaves route to
	//    END.
	for _, s := range b.w.Steps {
		b.emitStepEdges(s)
	}

	// 4) entry_node_id (§6): a single authored root becomes the entry; zero or
	//    multiple roots get a synthetic entry noop so the engine's single-entry
	//    reachability check (validator.go validateReachability) always covers
	//    every node.
	b.setEntry()

	// 5) Inputs / Outputs.
	b.emitInputs()
	b.emitOutputs()

	// Stable ordering for reproducible golden output.
	sort.SliceStable(b.def.Edges, func(i, j int) bool {
		a, c := b.def.Edges[i], b.def.Edges[j]
		if a.FromNodeID != c.FromNodeID {
			return a.FromNodeID < c.FromNodeID
		}
		if a.ToNodeID != c.ToNodeID {
			return a.ToNodeID < c.ToNodeID
		}
		return a.ConditionExpression < c.ConditionExpression
	})

	return b.def, b.findings, nil
}

// analyzeDeps computes, per step: data-dependency steps (from: steps.X in
// params/input), condition-dependency steps (when:/branch when: LHS), after:
// deps, and the inverse consumer map.
func (b *builder) analyzeDeps() {
	b.dataDeps = map[string][]string{}
	b.condDeps = map[string][]string{}
	b.afterDep = map[string][]string{}
	b.consumer = map[string][]string{}

	for _, s := range b.w.Steps {
		data := map[string]bool{}
		// Data deps: scan the whole step raw EXCEPT the when/after keys, which
		// are condition/ordering deps handled below.
		for _, r := range b.stepFromRefs(s) {
			segs := model.PathSegments(r)
			if len(segs) >= 2 && segs[0] == "steps" && b.stepIDs[segs[1]] {
				data[segs[1]] = true
			}
		}
		cond := map[string]bool{}
		for _, c := range b.conditionRefs(s) {
			segs := model.PathSegments(c)
			if len(segs) >= 2 && segs[0] == "steps" && b.stepIDs[segs[1]] {
				cond[segs[1]] = true
			}
		}
		b.dataDeps[s.ID] = keysOf(data)
		b.condDeps[s.ID] = keysOf(cond)
		b.afterDep[s.ID] = append([]string{}, s.After...)
	}

	// Inverse consumer map (union of data/cond/after producers).
	for _, s := range b.w.Steps {
		seen := map[string]bool{}
		add := func(dep string) {
			if dep == s.ID || seen[dep] || !b.stepIDs[dep] {
				return
			}
			seen[dep] = true
			b.consumer[dep] = append(b.consumer[dep], s.ID)
		}
		for _, d := range b.dataDeps[s.ID] {
			add(d)
		}
		for _, d := range b.condDeps[s.ID] {
			add(d)
		}
		for _, d := range b.afterDep[s.ID] {
			add(d)
		}
	}
}

// stepFromRefs returns every from: path referenced by a step's data surface
// (params/input), excluding its when condition (scanned separately).
func (b *builder) stepFromRefs(s model.Step) []string {
	var out []string
	collect := func(v interface{}) { out = append(out, fromRefsIn(v)...) }
	switch {
	case s.API != nil:
		collect(s.API.Params)
	case s.Browser != nil:
		collect(s.Browser.URL)
	case s.Transform != nil:
		collect(s.Transform.Params)
	case s.Reason != nil:
		collect(s.Reason.Input)
	case s.Branch != nil:
		// branch conditions are handled via conditionRefs
	}
	return out
}

// conditionRefs returns the step ids referenced by a step's when: condition or,
// for a branch step, each case's when.
func (b *builder) conditionRefs(s model.Step) []string {
	var out []string
	if s.HasWhen {
		out = append(out, conditionLHS(s.When)...)
	}
	if s.Branch != nil {
		for _, c := range s.Branch.Cases {
			if !c.Default {
				out = append(out, conditionLHS(c.When)...)
			}
		}
	}
	return out
}

// entryOf returns the node id an edge INTO step id should target: a when-step's
// gate (so all of the step's deps and its condition converge before the gate
// selects), otherwise the step's own node.
func (b *builder) entryOf(stepID string) string {
	if b.isWhen[stepID] {
		return stepID + gateSuffix
	}
	return stepID
}

// targetsOf returns the entry node ids a step's output should flow to: each
// consumer's entry, or END when the step is a leaf.
func (b *builder) targetsOf(stepID string) []string {
	cons := b.consumer[stepID]
	if len(cons) == 0 {
		return []string{endNodeID}
	}
	var out []string
	for _, c := range cons {
		out = append(out, b.entryOf(c))
	}
	sort.Strings(out)
	return out
}

func (b *builder) addEdge(e Edge) {
	key := e.FromNodeID + "|" + e.ToNodeID + "|" + e.ConditionExpression + "|" + fmt.Sprint(e.DefaultEdge)
	if b.edgeSeen[key] {
		return
	}
	b.edgeSeen[key] = true
	b.def.Edges = append(b.def.Edges, e)
}

func (b *builder) emitStepEdges(s model.Step) {
	targets := b.targetsOf(s.ID)
	if b.isWhen[s.ID] {
		gate := s.ID + gateSuffix
		skip := s.ID + skipSuffix
		// gate -> step (conditional) ; gate -> skip (default)
		b.addEdge(Edge{FromNodeID: gate, ToNodeID: s.ID, ConditionExpression: RewriteCondition(s.When)})
		b.addEdge(Edge{FromNodeID: gate, ToNodeID: skip, DefaultEdge: true})
		for _, t := range targets {
			b.addEdge(Edge{FromNodeID: s.ID, ToNodeID: t})
			b.addEdge(Edge{FromNodeID: skip, ToNodeID: t})
		}
		return
	}
	for _, t := range targets {
		b.addEdge(Edge{FromNodeID: s.ID, ToNodeID: t})
	}
}

func (b *builder) setEntry() {
	indeg := map[string]bool{}
	for _, e := range b.def.Edges {
		indeg[e.ToNodeID] = true
	}
	var roots []string
	for _, n := range b.def.Nodes {
		if n.ID == endNodeID {
			continue
		}
		if !indeg[n.ID] {
			roots = append(roots, n.ID)
		}
	}
	sort.Strings(roots)
	if len(roots) == 1 {
		b.def.EntryNodeID = roots[0]
		return
	}
	// Zero or multiple roots: synthesize a single entry so the engine's
	// single-entry reachability holds for every node.
	b.def.Nodes = append([]Node{{ID: entryNodeID, Type: NodeNoop, Name: "entry"}}, b.def.Nodes...)
	for _, r := range roots {
		b.addEdge(Edge{FromNodeID: entryNodeID, ToNodeID: r})
	}
	b.def.EntryNodeID = entryNodeID
}

func (b *builder) emitInputs() {
	// b.w.InputOrder derives from a YAML map (yaml.v3 loses source order), so
	// sort by name for reproducible golden output.
	names := append([]string{}, b.w.InputOrder...)
	sort.Strings(names)
	for _, name := range names {
		ps := b.w.Inputs[name]
		in := Input{
			Name:        name,
			Type:        ps.Type,
			Description: ps.Description,
			Required:    ps.Required,
		}
		if ps.HasDefault {
			in.DefaultValue = ps.Default
		}
		b.def.Inputs = append(b.def.Inputs, in)
	}
}

func (b *builder) emitOutputs() {
	names := append([]string{}, b.w.OutputOrder...)
	sort.Strings(names)
	for _, name := range names {
		v := b.w.Outputs[name]
		out := Output{Name: name}
		if from, ok := model.ParseFrom(v); ok {
			out.SourceExpression = RewriteExpression(from)
		} else {
			b.findings = append(b.findings, diag.Warn(diag.ClassSchema, "literal-output",
				"workflow.yaml#outputs."+name,
				fmt.Sprintf("output %q is a literal, not a {from:} reference; the WorkflowOutput carries no literal slot so it is emitted empty", name),
				"express outputs as {from: steps.<id>.<field>}"))
		}
		b.def.Outputs = append(b.def.Outputs, out)
	}
}

// emitStepNodes appends the primary node for a step (and, for a when-step, its
// gate branch + skip noop).
func (b *builder) emitStepNodes(s model.Step) error {
	node, err := b.compileNode(s)
	if err != nil {
		return err
	}
	b.def.Nodes = append(b.def.Nodes, node)
	if b.isWhen[s.ID] {
		gate := Node{
			ID:   s.ID + gateSuffix,
			Type: NodeBranch,
			Name: s.ID + " gate",
			Branch: &BranchConfig{
				ConditionExpression: RewriteCondition(s.When),
				DefaultNodeID:       s.ID + skipSuffix,
			},
			Metadata: map[string]string{"gated_step": s.ID, "when": s.When},
		}
		skip := Node{
			ID:       s.ID + skipSuffix,
			Type:     NodeNoop,
			Name:     s.ID + " skipped",
			Metadata: map[string]string{"bypass_of": s.ID},
		}
		b.def.Nodes = append(b.def.Nodes, gate, skip)
	}
	return nil
}

func (b *builder) compileNode(s model.Step) (Node, error) {
	n := Node{ID: s.ID, Name: s.ID}
	if s.Retry != nil {
		n.RetryPolicy = compileRetry(s.Retry)
	}
	if s.Single {
		setMeta(&n, "single_select", "true")
	}
	switch {
	case s.API != nil:
		return b.compileAPINode(n, s)
	case s.Browser != nil:
		return b.compileBrowserNode(n, s)
	case s.Transform != nil:
		return b.compileTransformNode(n, s)
	case s.Reason != nil:
		return b.compileReasonNode(n, s)
	case s.Branch != nil:
		return b.compileBranchNode(n, s)
	case s.Approval != nil:
		n.Type = NodeApproval
		n.Approval = &ApprovalConfig{Prompt: model.StringVal(s.Approval, "prompt")}
		if n.Approval.Prompt == "" {
			n.Approval.Prompt = "Approve step " + s.ID
		}
		return n, nil
	case s.Primitive != nil:
		return b.compilePrimitiveNode(n, s)
	case s.Code != nil:
		// code: is absorbed into T1 (Starlark, tool-fused) per §7 C1; without a
		// program surface in the model projection we cannot inline it yet.
		return n, fmt.Errorf("step %q: code: steps are not yet compilable (engine work item C1/T1)", s.ID)
	}
	return n, fmt.Errorf("step %q has no recognized step type", s.ID)
}

func (b *builder) compileAPINode(n Node, s model.Step) (Node, error) {
	if isBannedTool(s.API.Tool) {
		return n, fmt.Errorf("step %q uses banned node type %q which validates but always fails at execution (09 §6)", s.ID, s.API.Tool)
	}
	n.Type = NodeTool
	plain, fileRefs := model.FileRefParams(s.API.Params)
	statics, bindings := splitParams(plain)
	for _, fr := range sortedKeys(fileRefsMap(fileRefs)) {
		val, err := loadDataFile(b.pkg.Dir, fileRefs[fr])
		if err != nil {
			return n, fmt.Errorf("step %q: %w", s.ID, err)
		}
		if statics == nil {
			statics = map[string]interface{}{}
		}
		statics[fr] = val
	}
	n.Tool = &ToolConfig{
		IntegrationType:  s.API.Integration,
		ToolName:         s.API.Tool, // UNPREFIXED
		StaticParameters: statics,
	}
	n.InputBindings = sortBindings(bindings)
	if s.API.Credential != "" {
		setMeta(&n, "credential_slot", s.API.Credential)
	}
	if s.API.Paginate != "" {
		setMeta(&n, "paginate", s.API.Paginate)
	}
	n.EffectClass = toolEffectClass(s.API.Tool)
	return n, nil
}

func (b *builder) compileBrowserNode(n Node, s model.Step) (Node, error) {
	n.Type = NodeTool
	action := s.Browser.Action
	if action == "" {
		action = "extract"
	}
	statics := map[string]interface{}{}
	if s.Browser.OnDrift != "" {
		statics["on_drift"] = s.Browser.OnDrift
	}
	if s.Browser.Origin.Slot != "" {
		statics["origin"] = map[string]interface{}{"slot": s.Browser.Origin.Slot}
		setMeta(&n, "origin_slot", s.Browser.Origin.Slot)
	} else if s.Browser.Origin.Literal != "" {
		statics["origin"] = s.Browser.Origin.Literal
	}
	if s.Browser.PlanFrom != "" {
		plan, err := loadDataFile(b.pkg.Dir, s.Browser.PlanFrom)
		if err != nil {
			return n, fmt.Errorf("step %q: %w", s.ID, err)
		}
		statics["plan"] = plan
	}
	if b.pkg.Manifest.Requirements.Browser != nil && b.pkg.Manifest.Requirements.Browser.Session != "" {
		setMeta(&n, "session", b.pkg.Manifest.Requirements.Browser.Session)
	}
	if len(statics) == 0 {
		statics = nil
	}
	n.Tool = &ToolConfig{IntegrationType: "browser", ToolName: action, StaticParameters: statics}
	if from, ok := model.ParseFrom(s.Browser.URL); ok {
		n.InputBindings = []Binding{{Name: "url", TargetPath: "url", SourceExpression: RewriteExpression(from), Required: true}}
	} else if s.Browser.URL != nil {
		n.InputBindings = []Binding{{Name: "url", TargetPath: "url", LiteralValue: s.Browser.URL}}
	}
	n.EffectClass = "read"
	return n, nil
}

func (b *builder) compileTransformNode(n Node, s model.Step) (Node, error) {
	n.Type = NodeTransform
	program := s.Transform.Expression
	if s.Transform.ExpressionFrom != "" {
		p, err := loadProgram(b.pkg.Dir, s.Transform.ExpressionFrom)
		if err != nil {
			return n, fmt.Errorf("step %q: %w", s.ID, err)
		}
		program = p
	}
	n.Transform = &TransformConfig{Language: s.Transform.Language, Program: program}
	if n.Transform.Language == "" {
		n.Transform.Language = "starlark"
	}

	plain, fileRefs := model.FileRefParams(s.Transform.Params)
	statics, bindings := splitParams(plain)
	// Emit statics as literal-value bindings so a non-tool node receives them
	// as globals via buildWorkflowNodeInput (Tool.StaticParameters is only read
	// for tool nodes). Loaded *_from files are literal bindings too.
	var lits []Binding
	for _, k := range sortedKeys(statics) {
		lits = append(lits, Binding{Name: k, TargetPath: k, LiteralValue: statics[k]})
	}
	for _, fr := range sortedKeys(fileRefsMap(fileRefs)) {
		val, err := loadDataFile(b.pkg.Dir, fileRefs[fr])
		if err != nil {
			return n, fmt.Errorf("step %q: %w", s.ID, err)
		}
		lits = append(lits, Binding{Name: fr, TargetPath: fr, LiteralValue: val})
	}
	n.InputBindings = append(sortBindings(lits), sortBindings(bindings)...)
	n.EffectClass = "read"
	return n, nil
}

func (b *builder) compileReasonNode(n Node, s model.Step) (Node, error) {
	n.Type = NodeModel
	purpose := s.Reason.Purpose
	mc := &ModelConfig{
		Prompt:        "Perform the workflow reasoning task: " + purpose,
		Task:          purpose,
		OutputSchema:  s.Reason.OutputSchema,
		MaxIterations: 1,
	}
	if len(s.Reason.Capabilities) > 0 {
		mc.RoutingProfile = strings.Join(s.Reason.Capabilities, ",")
	}
	lease := map[string]interface{}{}
	if s.Reason.MaxTokens > 0 {
		lease["max_tokens"] = s.Reason.MaxTokens
	}
	if len(s.Reason.Capabilities) > 0 {
		lease["capabilities"] = s.Reason.Capabilities
	}
	if len(s.Reason.DataClasses) > 0 {
		lease["data_classes"] = s.Reason.DataClasses
	}
	if s.Reason.InputSchema != nil {
		lease["input_schema"] = s.Reason.InputSchema
	}
	if s.Reason.OutputSchema != nil {
		lease["output_schema"] = s.Reason.OutputSchema
	}
	if len(lease) > 0 {
		if raw, err := json.Marshal(lease); err == nil {
			mc.LeaseConfigJSON = string(raw)
		}
	}
	n.Model = mc
	if from, ok := model.ParseFrom(s.Reason.Input); ok {
		n.InputBindings = []Binding{{Name: "input", TargetPath: "input", SourceExpression: RewriteExpression(from)}}
	} else if s.Reason.Input != nil {
		n.InputBindings = []Binding{{Name: "input", TargetPath: "input", LiteralValue: s.Reason.Input}}
	}
	n.EffectClass = "read"
	return n, nil
}

// compileBranchNode compiles an explicit branch: step. The engine branch is
// multi-select, so the compiler relies on the author's cases being structurally
// mutually exclusive (single LHS path, distinct literal RHS) and flags any pair
// that could both fire; it emits one conditional edge per case plus exactly one
// default. Edge targets come from each case's `then:` (branchCaseKeys allows
// it). Untested by the golden corpus (no seed uses branch:).
func (b *builder) compileBranchNode(n Node, s model.Step) (Node, error) {
	n.Type = NodeBranch
	var defaultThen string
	var lhsSeen string
	rhsSeen := map[string]bool{}
	for _, c := range s.Branch.Cases {
		then := model.StringVal(c.Raw, "then")
		if c.Default {
			defaultThen = then
			continue
		}
		if lhs, rhs, ok := splitCondLiteral(c.When); ok {
			if lhsSeen == "" {
				lhsSeen = lhs
			} else if lhsSeen != lhs {
				b.findings = append(b.findings, diag.Warn(diag.ClassSchema, "branch-not-provably-exclusive",
					"workflow.yaml#steps."+s.ID+".branch",
					fmt.Sprintf("branch cases compare different left-hand paths (%q vs %q); the executor multi-selects, so mutual exclusivity is not provable", lhsSeen, lhs), ""))
			}
			if rhsSeen[rhs] {
				b.findings = append(b.findings, diag.Error(diag.ClassSchema, "branch-overlapping-case",
					"workflow.yaml#steps."+s.ID+".branch",
					fmt.Sprintf("two branch cases share the same literal %q; they are not mutually exclusive", rhs), ""))
			}
			rhsSeen[rhs] = true
		}
	}
	n.Branch = &BranchConfig{DefaultNodeID: defaultThen}
	setMeta(&n, "branch_step", s.ID)
	return n, nil
}

func (b *builder) compilePrimitiveNode(n Node, s model.Step) (Node, error) {
	// §6: subflows are inlined at compile time (the executor performs no
	// registry lookup). Without a resolvable local path for the referenced
	// primitive, the compiler cannot inline here; it emits a subflow reference
	// and a finding so the gap is explicit rather than silent.
	n.Type = NodeSubflow
	n.Subflow = &SubflowConfig{WorkflowDefinitionID: s.Primitive.Name}
	b.findings = append(b.findings, diag.Warn(diag.ClassSchema, "primitive-not-inlined",
		"workflow.yaml#steps."+s.ID+".primitive",
		fmt.Sprintf("primitive %q referenced; compile-time inlining needs a resolvable package path (registry integration), emitted as a subflow reference", s.Primitive.Name),
		"provide the referenced primitive package locally for inlining"))
	return n, nil
}

func compileRetry(r *model.RetrySpec) *RetryPolicy {
	rp := &RetryPolicy{
		MaxAttempts:           r.MaxAttempts,
		InitialBackoffSeconds: r.InitialBackoffSeconds,
	}
	if r.RetryOnKeyPresent && len(r.RetryOn) > 0 {
		rp.RetryOn = append([]string{}, r.RetryOn...)
	} else {
		// §6: retry_on is ALWAYS non-empty (an empty list retries every error).
		rp.RetryOn = append([]string{}, DefaultRetryOn...)
	}
	return rp
}

func toolEffectClass(tool string) string {
	for _, p := range writeVerbPrefixes {
		if strings.HasPrefix(tool, p) {
			return "write"
		}
	}
	return "read"
}

func isBannedTool(tool string) bool {
	for _, b := range bannedNodeTools {
		if tool == b || strings.Contains(tool, b) {
			return true
		}
	}
	return false
}

// ---- small helpers ----

func setMeta(n *Node, k, v string) {
	if n.Metadata == nil {
		n.Metadata = map[string]string{}
	}
	n.Metadata[k] = v
}

func sortBindings(bs []Binding) []Binding {
	if len(bs) == 0 {
		return nil
	}
	out := append([]Binding{}, bs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].TargetPath < out[j].TargetPath })
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fileRefsMap(fr map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(fr))
	for k := range fr {
		out[k] = struct{}{}
	}
	return out
}

// fromRefsIn recursively collects every {from: <path>} string under v.
func fromRefsIn(v interface{}) []string {
	var out []string
	switch tv := v.(type) {
	case map[string]interface{}:
		if from, ok := model.ParseFrom(tv); ok {
			return []string{from}
		}
		for _, k := range sortedKeys(tv) {
			out = append(out, fromRefsIn(tv[k])...)
		}
	case []interface{}:
		for _, e := range tv {
			out = append(out, fromRefsIn(e)...)
		}
	}
	return out
}

// conditionLHS returns the from-path(s) on the left of a when condition (the
// comparison LHS, or the whole path for bare truthiness).
func conditionLHS(cond string) []string {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return nil
	}
	for _, op := range []string{"==", "!="} {
		if idx := strings.Index(cond, op); idx >= 0 {
			return []string{strings.TrimSpace(cond[:idx])}
		}
	}
	return []string{cond}
}

func splitCondLiteral(cond string) (lhs, rhs string, ok bool) {
	cond = strings.TrimSpace(cond)
	for _, op := range []string{"==", "!="} {
		if idx := strings.Index(cond, op); idx >= 0 {
			return strings.TrimSpace(cond[:idx]), strings.TrimSpace(cond[idx+len(op):]), true
		}
	}
	return "", "", false
}
