package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pyparse"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

type SpanNode struct {
	Call    trace.Call        `json:"-"`
	Ordinal int               `json:"-"`
	Steps   []trace.Step      `json:"-"`
	Label   string            `json:"-"`
	Effect  string            `json:"-"`
	Inputs  []model.SpanInput `json:"-"`
	Deps    []int             `json:"-"`
	Anchors []string          `json:"-"`
	// Choices is the corpus evidence that decides which plain-word
	// arguments choose the operation; nil means none (every one does).
	Choices *trace.Choices `json:"-"`
}

// SpanSelectorSlot reports an argument that is part of the node's operation:
// on a shell step that may write, a flag's plain-word value (the authority
// it acts under is never shared); on a tool call, a plain-word value the
// corpus evidence calls a choice. Never decided by a name.
func SpanSelectorSlot(call trace.Call, ch *trace.Choices, st trace.Step, sl trace.Slot) bool {
	if sl.Sub || trace.Derived(sl.Key) || !trace.PlainChoiceValue(sl.Value) {
		return false
	}
	if strings.HasPrefix(st.Label, "sh:") {
		return strings.HasSuffix(sl.Key, "=") && trace.StepEffect(st) != "read"
	}
	id, ok := trace.ArgIdentity(call, sl.Key)
	return ok && ch.Choice(id)
}

// SelectSpanProposals extracts bounded pieces of work without requiring the
// whole request to be short, multi-call, or to name an object literally. It
// makes no LLM call and does not change the older recommendation score.
func SelectSpanProposals(ss []trace.Session) []model.SpanProposal {
	cp := append([]trace.Session(nil), ss...)
	for i := range cp {
		cp[i].Calls = append([]trace.Call(nil), cp[i].Calls...)
	}
	trace.DropCopiedCalls(cp)
	norm := trace.Normalize(cp)
	choices := trace.NewChoices(cp)
	normBySession := make(map[string]*trace.NormSession, len(norm))
	for i := range norm {
		normBySession[norm[i].Client+"\x00"+norm[i].ID] = &norm[i]
	}
	type requestNodes struct {
		session int
		request int
		nodes   []SpanNode
	}
	var work []requestNodes
	pairSessions := map[string]map[string]bool{}
	for si := range cp {
		s := &cp[si]
		ns := normBySession[s.Client+"\x00"+s.ID]
		if ns == nil {
			continue
		}
		byCall := map[int][]trace.Step{}
		for _, st := range ns.Steps {
			byCall[st.Call] = append(byCall[st.Call], st)
		}
		byReq := map[int][]int{}
		for ci := range s.Calls {
			byReq[s.Calls[ci].Request] = append(byReq[s.Calls[ci].Request], ci)
		}
		reqs := make([]int, 0, len(byReq))
		for r := range byReq {
			reqs = append(reqs, r)
		}
		sort.Ints(reqs)
		for _, r := range reqs {
			if r >= len(s.Requests) || strings.TrimSpace(s.Requests[r]) == "" || trace.IsHarness(s.Requests[r]) {
				continue
			}
			nodes := BuildSpanNodes(*s, r, byReq[r], byCall, choices)
			if len(nodes) == 0 {
				continue
			}
			work = append(work, requestNodes{session: si, request: r, nodes: nodes})
			for i := 1; i < len(nodes); i++ {
				key := SpanAdjacentPairKey(nodes[i-1], nodes[i])
				if key == "" {
					continue
				}
				if pairSessions[key] == nil {
					pairSessions[key] = map[string]bool{}
				}
				pairSessions[key][s.Client+"\x00"+s.ID] = true
			}
		}
	}
	pairSupport := map[string]bool{}
	for key, sessions := range pairSessions {
		if len(sessions) >= 2 {
			pairSupport[key] = true
		}
	}
	var out []model.SpanProposal
	for _, item := range work {
		out = append(out, ProposalsForRequest(cp[item.session], item.request, item.nodes, pairSupport)...)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Request != b.Request {
			return a.Request < b.Request
		}
		return a.ID < b.ID
	})
	return out
}

func BuildSpanNodes(s trace.Session, req int, calls []int, byCall map[int][]trace.Step, ch *trace.Choices) []SpanNode {
	var nodes []SpanNode
	for ordinal, ci := range calls {
		steps := byCall[ci]
		if len(steps) == 0 {
			continue
		}
		work := false
		effect := "unknown"
		var labels []string
		for _, st := range steps {
			work = true
			label := st.Label
			for _, sl := range st.Slots {
				if !strings.HasPrefix(st.Label, "sh:") && SpanSelectorSlot(s.Calls[ci], ch, st, sl) {
					label += "#" + sl.Key + "=" + trace.OneLine(strings.ToLower(sl.Value), 40)
				}
			}
			labels = append(labels, label)
			switch trace.StepEffect(st) {
			case "write":
				effect = "write"
			case "read":
				if effect != "write" {
					effect = "read"
				}
			}
		}
		if !work {
			continue
		}
		n := SpanNode{Call: s.Calls[ci], Ordinal: ordinal + 1, Steps: steps,
			Label: strings.Join(labels, "+"), Effect: effect, Choices: ch}
		for _, st := range steps {
			for _, slot := range SpanExpandedSlots(st.Slots) {
				if !SpanVariable(slot) {
					continue
				}
				input := model.SpanInput{Key: slot.Key, Type: slot.Type, Source: "unknown"}
				v := slot.Value
				switch {
				case SpanInText(v, slot.Type, s.Requests[req]):
					input.Source = "caller"
				case SpanPriorRequest(v, slot.Type, s.Requests, req) >= 0:
					input.Source = "prior_request"
					input.FromRequest = SpanPriorRequest(v, slot.Type, s.Requests, req) + 1
				default:
					for j := len(nodes) - 1; j >= 0; j-- {
						if SpanResultHas(nodes[j].Call, v) {
							input.Source, input.FromCall = "prior_result", nodes[j].Ordinal
							n.Deps = AppendUniqueInt(n.Deps, j)
							break
						}
					}
					if input.Source == "unknown" {
						for ci := len(s.Calls) - 1; ci >= 0; ci-- {
							if s.Calls[ci].Request >= req {
								continue
							}
							if SpanResultHas(s.Calls[ci], v) {
								input.Source, input.FromRequest = "prior_result", s.Calls[ci].Request+1
								break
							}
						}
					}
				}
				n.Inputs = append(n.Inputs, input)
				if input.Source == "caller" || input.Source == "prior_request" {
					if slot.Type == trace.SlotID || slot.Type == trace.SlotPath || slot.Type == trace.SlotURL || slot.Type == trace.SlotNumber {
						n.Anchors = append(n.Anchors, slot.Type+"\x00"+v)
					}
				}
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// Tool gateways commonly put the real resource parameters inside a JSON
// string argument such as params={"job_id":123}. Flattening only the outer
// argument hides both caller inputs and result-derived dependencies.
func SpanExpandedSlots(slots []trace.Slot) []trace.Slot {
	out := append([]trace.Slot(nil), slots...)
	for _, sl := range slots {
		if !strings.HasPrefix(strings.TrimSpace(sl.Value), "{") {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(sl.Value), &obj) != nil {
			continue
		}
		SpanFlattenObject(&out, sl.Key, obj, 0)
	}
	return out
}

func SpanFlattenObject(out *[]trace.Slot, prefix string, obj map[string]json.RawMessage, depth int) {
	if depth >= 4 {
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Use slash so these new argument fields are not mistaken for the
		// normalizer's derived "path.basename" slots.
		key := prefix + "/" + k
		var nested map[string]json.RawMessage
		if json.Unmarshal(obj[k], &nested) == nil && nested != nil {
			SpanFlattenObject(out, key, nested, depth+1)
			continue
		}
		var v string
		if json.Unmarshal(obj[k], &v) != nil {
			v = string(obj[k])
		}
		if v == "" || strings.HasPrefix(v, "[") || v == "null" {
			continue
		}
		*out = append(*out, trace.Slot{Key: key, Type: trace.TypeOf(shellparse.Word{Text: v}), Value: v})
	}
}

func SpanVariable(s trace.Slot) bool {
	if s.Sub || s.Type == trace.SlotFlag || trace.Derived(s.Key) || s.Key == "recv" {
		return false
	}
	// Shell flags configure an observed command; their literal values are
	// not automatically inputs the eventual primitive must ask for.
	if strings.HasPrefix(s.Key, "-") {
		return false
	}
	v := strings.TrimSpace(s.Value)
	if len(v) < 3 {
		return false
	}
	switch s.Type {
	case trace.SlotWord:
		return strings.Contains(s.Key, "query") || strings.Contains(s.Key, "search") || strings.Contains(s.Key, "name")
	case trace.SlotNumber:
		return len(v) >= 5 || strings.Contains(strings.ToLower(s.Key), "id")
	default:
		return true
	}
}

func SpanInText(value, kind, text string) bool {
	if kind == trace.SlotPath || kind == trace.SlotURL {
		return trace.InRequest(value, text)
	}
	return ContainsItem(strings.ToLower(text), strings.ToLower(value))
}

func SpanPriorRequest(value, kind string, requests []string, before int) int {
	for r := before - 1; r >= 0; r-- {
		if !trace.IsHarness(requests[r]) && !history.IsClaudeContinuationSummary(requests[r]) && SpanInText(value, kind, requests[r]) {
			return r
		}
	}
	return -1
}

func SpanResultHas(c trace.Call, value string) bool {
	if c.Outcome == trace.OutcomeFailed || len(value) < 4 {
		return false
	}
	for i, id := range c.OutIDs {
		// A literal in a shell transcript can be source code, a command echo,
		// or a diagnostic. A uniquely located structured result is evidence
		// that this call actually produced the value used by a later call.
		if id == value && i < len(c.OutPaths) && c.OutPaths[i] != "" && c.OutPaths[i] != "*" {
			return true
		}
	}
	return false
}

func AppendUniqueInt(xs []int, n int) []int {
	for _, x := range xs {
		if x == n {
			return xs
		}
	}
	return append(xs, n)
}

func ProposalsForRequest(s trace.Session, req int, nodes []SpanNode, pairSupport map[string]bool) []model.SpanProposal {
	var sets [][]int
	authored := map[string]bool{}
	repeatedOrder := map[string]bool{}
	siblingChildren := map[int]bool{}
	byParentOperation := map[string][]int{}
	for i, n := range nodes {
		if len(n.Deps) == 1 {
			key := strconv.Itoa(n.Deps[0]) + "\x00" + n.Label
			byParentOperation[key] = append(byParentOperation[key], i)
		}
	}
	for _, children := range byParentOperation {
		if len(children) < 2 {
			continue
		}
		parent := nodes[children[0]].Deps[0]
		if strings.Contains(nodes[parent].Label, "tabs_context_mcp") {
			continue
		}
		set := append([]int{parent}, children...)
		sort.Ints(set)
		sets = append(sets, set)
		for _, child := range children {
			siblingChildren[child] = true
		}
	}
	// Result dependencies supply a causal slice even when the useful work is
	// surrounded by unrelated navigation or editing in a long request.
	for i := range nodes {
		if len(nodes[i].Deps) == 0 || siblingChildren[i] || SpanNavigationDependency(nodes, i) {
			continue
		}
		seen := map[int]bool{}
		var visit func(int)
		visit = func(j int) {
			if seen[j] {
				return
			}
			seen[j] = true
			for _, parent := range nodes[j].Deps {
				visit(parent)
			}
		}
		visit(i)
		var set []int
		for j := range seen {
			set = append(set, j)
		}
		sort.Ints(set)
		sets = append(sets, set)
	}
	// Separate result chains can be iterations of one program even when the
	// calls are interleaved (A1, A2, B1, B2). Retain each chain as evidence,
	// and also propose their disjoint union for loop synthesis.
	sets = append(sets, SpanRepeatedMotifSets(nodes, sets)...)
	// Fixed sequences can share a caller-supplied resource without consuming
	// each other's results (for example, comment then transition an issue).
	byAnchor := map[string][]int{}
	for i, n := range nodes {
		for _, anchor := range n.Anchors {
			kind, _, _ := strings.Cut(anchor, "\x00")
			// A shared file path mostly joins incidental source reads and edits.
			// Resource IDs, URLs and explicit numbers identify a caller's
			// bounded target more reliably for this independent sequence route.
			if kind != trace.SlotID && kind != trace.SlotURL && kind != trace.SlotNumber {
				continue
			}
			byAnchor[anchor] = AppendUniqueInt(byAnchor[anchor], i)
		}
	}
	for _, set := range byAnchor {
		// Re-reading one file or running one command repeatedly against the
		// same path is a common investigation trace, not a fixed sequence.
		if len(set) >= 2 && len(set) <= 8 && SpanDistinctOperations(nodes, set) >= 2 {
			sets = append(sets, set)
		}
	}
	// A request that explicitly names two adjacent command operations is
	// evidence for their sequence even when no result ID or shared resource
	// joins the calls. Keep this narrow: command names and subcommands must
	// appear in the user's request, and the calls must be direct literals.
	for i := 1; i < len(nodes); i++ {
		if SpanExplicitCommand(s.Requests[req], nodes[i-1]) && SpanExplicitCommand(s.Requests[req], nodes[i]) {
			sets = append(sets, []int{i - 1, i})
		}
		if pairSupport[SpanAdjacentPairKey(nodes[i-1], nodes[i])] && !SpanPairCovered(sets, i-1, i) {
			set := []int{i - 1, i}
			sets = append(sets, set)
			repeatedOrder[SpanSetKey(nodes, set)] = true
		}
	}
	for _, set := range SpanAuthoredProgramSets(nodes) {
		sets = append(sets, set)
		authored[SpanSetKey(nodes, set)] = true
	}
	inlineShapes := map[string]string{}
	inlineFamilies := map[string]string{}
	inlineScopes := map[string]string{}
	for i, n := range nodes {
		if n.Call.Tool != "shell" {
			continue
		}
		shape, family, embedded := pyparse.InlinePythonSnippet(n.Call.Command)
		if shape == "" {
			continue
		}
		set := []int{i}
		key := SpanSetKey(nodes, set)
		sets = append(sets, set)
		authored[key] = true
		inlineShapes[key] = shape
		inlineFamilies[key] = family
		inlineScopes[key] = "direct"
		if embedded {
			inlineScopes[key] = "embedded"
		}
	}
	covered := map[int]bool{}
	for _, set := range sets {
		for _, i := range set {
			covered[i] = true
		}
	}
	// A complete single call is eligible for review. It is still unassessed;
	// the caller must establish its output contract and business result.
	for i, n := range nodes {
		if covered[i] {
			continue
		}
		if !SpanDirectIntent(s.Requests, req, n) {
			continue
		}
		if n.Effect == "unknown" && n.Call.Output == "" {
			continue
		}
		if n.Effect == "read" && n.Call.Output == "" {
			continue
		}
		sets = append(sets, []int{i})
	}
	seenSets := map[string]bool{}
	var out []model.SpanProposal
	for _, set := range sets {
		key := SpanSetKey(nodes, set)
		if seenSets[key] {
			continue
		}
		seenSets[key] = true
		kind := "single_call"
		if authored[key] {
			kind = "authored_program"
		} else if repeatedOrder[key] {
			kind = "repeated_order"
		} else if len(set) > 1 {
			kind = "shared_input"
			for _, i := range set {
				if len(nodes[i].Deps) > 0 {
					kind = "result_chain"
					break
				}
			}
		}
		proposal := MakeSpanProposal(s, req, nodes, set, kind)
		proposal.CodeShape = inlineShapes[key]
		proposal.CodeFamily = inlineFamilies[key]
		proposal.CodeScope = inlineScopes[key]
		out = append(out, proposal)
	}
	return out
}

func SpanPairCovered(sets [][]int, first, second int) bool {
	for _, set := range sets {
		a, b := false, false
		for _, i := range set {
			a = a || i == first
			b = b || i == second
		}
		if a && b {
			return true
		}
	}
	return false
}

func SpanAdjacentPairKey(a, b SpanNode) string {
	// Two reads are an investigation; an unknown effect may write.
	if a.Effect == "read" && b.Effect == "read" || a.Call.Outcome == trace.OutcomeFailed || b.Call.Outcome == trace.OutcomeFailed {
		return ""
	}
	// Repeated order alone is not a process: an issue comment followed by a
	// local file read can recur in many sessions without a shared operation.
	// Require the two calls to act on the same concrete resource in each
	// execution. The value is used only to prove this within one execution;
	// it is absent from the cross-session key.
	if !SpanPairSharesResource(a, b) {
		return ""
	}
	first, second := LogicRole(SpanActionRole(a)), LogicRole(SpanActionRole(b))
	if first == second || first == "" || second == "" {
		return ""
	}
	return first + " -> " + second
}

func SpanPairSharesResource(a, b SpanNode) bool {
	values := map[string]bool{}
	for _, st := range a.Steps {
		for _, slot := range SpanExpandedSlots(st.Slots) {
			if !SpanVariable(slot) || !SpanPairResourceSlot(slot) || redact.SensitiveSlot(st.Label, slot) {
				continue
			}
			values[slot.Type+"\x00"+slot.Value] = true
		}
	}
	for _, st := range b.Steps {
		for _, slot := range SpanExpandedSlots(st.Slots) {
			if SpanVariable(slot) && SpanPairResourceSlot(slot) && !redact.SensitiveSlot(st.Label, slot) && values[slot.Type+"\x00"+slot.Value] {
				return true
			}
		}
	}
	return false
}

func SpanPairResourceSlot(slot trace.Slot) bool {
	if len(slot.Value) < 4 || slot.Value == "/dev/null" {
		return false
	}
	switch slot.Type {
	case trace.SlotID, trace.SlotURL, trace.SlotPath:
		return true
	}
	return false
}

func SpanExplicitCommand(request string, n SpanNode) bool {
	if n.Call.Tool != "shell" {
		return false
	}
	words, err := shellparse.ProgramShellWords(n.Call.Command)
	if err != nil || shellparse.ProgramCommandRunsCode(words[0], words[1:]) {
		return false
	}
	mentioned := SpanWords(request)
	if !mentioned[strings.ToLower(words[0])] || len(words) < 2 {
		return false
	}
	first := strings.ToLower(words[1])
	return trace.TypeOf(shellparse.Word{Text: first}) == trace.SlotWord && mentioned[first]
}

func SpanRepeatedMotifSets(nodes []SpanNode, sets [][]int) [][]int {
	byShape := map[string][][]int{}
	seen := map[string]bool{}
	for _, set := range sets {
		if len(set) < 2 {
			continue
		}
		identity := SpanSetKey(nodes, set)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		shape := SpanComposition(nodes, set).Key
		byShape[shape] = append(byShape[shape], set)
	}
	keys := make([]string, 0, len(byShape))
	for key := range byShape {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out [][]int
	for _, key := range keys {
		group := byShape[key]
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			if len(group[i]) != len(group[j]) {
				return len(group[i]) > len(group[j])
			}
			return SpanSetKey(nodes, group[i]) < SpanSetKey(nodes, group[j])
		})
		used := map[int]bool{}
		union := []int{}
		iterations := 0
		for _, set := range group {
			overlaps := false
			for _, node := range set {
				if used[node] {
					overlaps = true
					break
				}
			}
			if overlaps {
				continue
			}
			iterations++
			for _, node := range set {
				used[node] = true
				union = append(union, node)
			}
		}
		if iterations < 2 {
			continue
		}
		sort.Ints(union)
		out = append(out, union)
	}
	return out
}

func SpanNavigationDependency(nodes []SpanNode, i int) bool {
	if len(nodes[i].Deps) != 1 {
		return false
	}
	parent := nodes[nodes[i].Deps[0]].Label
	return strings.Contains(parent, "tabs_context_mcp")
}

func SpanSetKey(nodes []SpanNode, set []int) string {
	var parts []string
	for _, i := range set {
		parts = append(parts, strconv.Itoa(nodes[i].Ordinal))
	}
	return strings.Join(parts, ",")
}

var SpanScriptWriteRe = regexp.MustCompile(`(?:^|[;&\n])\s*(?:cat|tee)\s*>?\s*([^\s<>;&]+)\s*<<`)

// A program authored in the transcript and run twice with different calls is
// a bounded subprocedure even if the containing request was an investigation.
// Only literal script paths count; shell-variable expansion is unresolved.
func SpanAuthoredProgramSets(nodes []SpanNode) [][]int {
	writes := map[string]int{}
	for i, n := range nodes {
		if n.Call.Tool == "shell" {
			for _, m := range SpanScriptWriteRe.FindAllStringSubmatch(n.Call.Command, -1) {
				if SpanProgramPath(m[1]) {
					writes[m[1]] = i
				}
			}
		} else if n.Call.Tool == "Write" || n.Call.Tool == "write_file" || n.Call.Tool == "Edit" {
			path := n.Call.Args["file_path"]
			if path == "" {
				path = n.Call.Args["path"]
			}
			if SpanProgramPath(path) {
				writes[path] = i
			}
		}
	}
	var out [][]int
	for path, write := range writes {
		set := []int{write}
		for i := write + 1; i < len(nodes); i++ {
			if SpanRunsScript(nodes[i].Call, path) {
				set = append(set, i)
			}
		}
		if len(set) >= 3 {
			out = append(out, set)
		}
	}
	return out
}

func SpanProgramPath(path string) bool {
	if len(path) < 5 || strings.ContainsAny(path, "$*`\n") {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sh", ".py", ".go", ".js", ".ts", ".rb":
		return true
	}
	return false
}

func SpanRunsScript(c trace.Call, path string) bool {
	if c.Tool != "shell" {
		return false
	}
	for _, line := range strings.FieldsFunc(c.Command, func(r rune) bool { return r == '\n' || r == ';' }) {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && f[0] == "timeout" {
			f = f[2:]
		}
		if len(f) >= 2 && (f[0] == "bash" || f[0] == "sh" || f[0] == "python" || f[0] == "python3" || f[0] == "node" || f[0] == "ruby") {
			f = f[1:]
		}
		if len(f) >= 3 && f[0] == "go" && f[1] == "run" {
			f = f[2:]
		}
		if len(f) > 0 && f[0] == path {
			return true
		}
	}
	return false
}

func SpanDistinctOperations(nodes []SpanNode, set []int) int {
	seen := map[string]bool{}
	for _, i := range set {
		seen[nodes[i].Label] = true
	}
	return len(seen)
}

// SpanDirectIntent reports that the request names what the call acts on:
// the request, or the previous substantive request, literally contains one
// of the call's identifier, path or URL values, or names the command and its
// subcommand. This is literal grounding in the user's own text, not a
// vocabulary of verbs or products, and it is retrieval evidence, never a
// useful-procedure judgment.
func SpanDirectIntent(requests []string, req int, n SpanNode) bool {
	text := SpanContextText(requests, req)
	if SpanExplicitCommand(text, n) || SpanNamesOperation(text, n) {
		return true
	}
	for _, st := range n.Steps {
		for _, sl := range SpanExpandedSlots(st.Slots) {
			if sl.Sub || trace.Derived(sl.Key) || len(sl.Value) < 4 {
				continue
			}
			if sl.Type != trace.SlotID && sl.Type != trace.SlotPath && sl.Type != trace.SlotURL && sl.Type != trace.SlotNumber {
				continue
			}
			if trace.InRequest(sl.Value, text) {
				return true
			}
		}
	}
	return false
}

// SpanNamesOperation reports that the request uses one of the call's own
// operation words: a shell command's leading subcommand words, or an MCP
// tool's name after its first (provider) part, and the plain-word choices
// passed to it. The words come from the recorded call, never from a list;
// a request word matches when it equals one or extends it by an inflection
// (jobs, issues).
func SpanNamesOperation(text string, n SpanNode) bool {
	var ops []string
	split := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r < 'a' || r > 'z' })
	}
	if n.Call.Tool == "shell" {
		words, err := shellparse.ProgramShellWords(n.Call.Command)
		if err != nil || len(words) < 2 {
			return false
		}
		for _, w := range words[1:] {
			if trace.TypeOf(shellparse.Word{Text: w}) != trace.SlotWord {
				break
			}
			ops = append(ops, split(w)...)
		}
	} else if strings.HasPrefix(n.Call.Tool, "mcp:") {
		if parts := split(strings.TrimPrefix(n.Call.Tool, "mcp:")); len(parts) > 1 {
			ops = append(ops, parts[1:]...)
		}
		for _, st := range n.Steps {
			for _, sl := range SpanExpandedSlots(st.Slots) {
				if SpanSelectorSlot(n.Call, n.Choices, st, sl) && !redact.SensitiveSlot(st.Label, sl) {
					ops = append(ops, split(sl.Value)...)
				}
			}
		}
	}
	said := SpanWords(text)
	for _, op := range ops {
		if len(op) < 3 {
			continue
		}
		for w := range said {
			short, long := op, w
			if len(short) > len(long) {
				short, long = long, short
			}
			if strings.HasPrefix(long, short) && len(long)-len(short) <= 2 {
				return true
			}
		}
	}
	return false
}

// SpanContextText is the request with the previous substantive request in
// front of it: a value given one turn earlier is still the caller's input.
func SpanContextText(requests []string, req int) string {
	text := requests[req]
	for r := req - 1; r >= 0; r-- {
		if strings.TrimSpace(requests[r]) != "" && !trace.IsHarness(requests[r]) && !history.IsClaudeContinuationSummary(requests[r]) {
			return requests[r] + "\n" + text
		}
	}
	return text
}

func SpanWords(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if len(w) < 3 {
			continue
		}
		if strings.HasSuffix(w, "ies") && len(w) > 5 {
			w = strings.TrimSuffix(w, "ies") + "y"
		} else if strings.HasSuffix(w, "s") && len(w) > 5 && !strings.HasSuffix(w, "ss") && w != "status" {
			w = strings.TrimSuffix(w, "s")
		}
		out[w] = true
	}
	return out
}

func MakeSpanProposal(s trace.Session, req int, nodes []SpanNode, set []int, kind string) model.SpanProposal {
	var calls []int
	var callHashes []string
	var labels, tools, inputShapes []string
	effect, score := "unknown", 0
	seenInputs := map[string]bool{}
	var inputs []model.SpanInput
	for _, i := range set {
		n := nodes[i]
		calls = append(calls, n.Ordinal)
		callHashes = append(callHashes, SpanCallHash(n.Call))
		labels = append(labels, n.Label)
		for _, st := range n.Steps {
			tools = append(tools, st.Label)
		}
		if n.Effect == "write" {
			effect = "write"
		} else if n.Effect == "read" && effect != "write" {
			effect = "read"
		}
		if n.Call.Outcome == trace.OutcomeOK {
			score++
		}
		if n.Call.Output != "" {
			score++
		}
		for _, in := range n.Inputs {
			shape := in.Key + ":" + in.Type + ":" + in.Source
			inputShapes = append(inputShapes, shape)
			if in.Source != "unknown" {
				score++
			}
			if !seenInputs[shape] {
				inputs = append(inputs, in)
				seenInputs[shape] = true
			}
		}
	}
	sort.Strings(inputShapes)
	text := s.Requests[req]
	contextRequest := 0
	// Earlier task context distinguishes an elliptical follow-up from the
	// same short phrase in an unrelated session; it is not an authority grant.
	if req > 0 {
		for r := req - 1; r >= 0; r-- {
			if strings.TrimSpace(s.Requests[r]) != "" && !trace.IsHarness(s.Requests[r]) && !SpanSyntheticRequest(s, r) {
				text = s.Requests[r] + "\n" + text
				contextRequest = r + 1
				break
			}
		}
	}
	goal := ObjectRe.ReplaceAllStringFunc(strings.ToLower(text), func(v string) string {
		switch {
		case strings.HasPrefix(v, "http"):
			return "<url>"
		case strings.Contains(v, "/"):
			return "<path>"
		default:
			return "<id>"
		}
	})
	goal = strings.Join(strings.Fields(goal), " ")
	goalHash := sha256.Sum256([]byte(goal))
	var scope []string
	for _, i := range set {
		for _, st := range nodes[i].Steps {
			for _, sl := range st.Slots {
				if !trace.IsScopeSlot(st, sl) {
					continue
				}
				k := strings.ToLower(sl.Key)
				if strings.Contains(k, "context") || strings.Contains(k, "environment") || k == "env" || strings.Contains(k, "cluster") || strings.Contains(k, "profile") || strings.Contains(k, "namespace") {
					scope = append(scope, k+"="+strings.ToLower(sl.Value))
				}
			}
		}
	}
	sort.Strings(scope)
	shape := strings.Join([]string{hex.EncodeToString(goalHash[:6]), strings.Join(labels, ">"), strings.Join(inputShapes, ","), effect, strings.Join(scope, ",")}, "|")
	shapeHash := sha256.Sum256([]byte(shape))
	// The ID depends only on source identity and the selected call ordinals.
	idHash := sha256.Sum256([]byte(trace.EpisodeID(s.Client, s.ID, req) + "/" + strings.Join(IntsToStrings(calls), ",")))
	start := nodes[set[0]].Call.Time
	return model.SpanProposal{ID: "sp_" + hex.EncodeToString(idHash[:6]), Client: s.Client, Session: s.ID, Request: req,
		Task: s.Client + "/" + s.ID + "/" + strconv.Itoa(req), Status: model.BriefStatus, Kind: kind, Calls: calls, CallHashes: callHashes, Tools: tools,
		Inputs: inputs, Effect: effect, GoalKey: hex.EncodeToString(goalHash[:6]), ContextRequest: contextRequest,
		ShapeKey: "shape_" + hex.EncodeToString(shapeHash[:8]), Composition: SpanComposition(nodes, set), Review: AssessSpanTask(s, req, nodes, set, inputs), EvidenceScore: score, Start: start}
}

// SpanCallHash lets a brief match selected calls against the source session
// even when resumed-session copy removal shifted their numeric positions.
func SpanCallHash(c trace.Call) string {
	b, _ := json.Marshal(struct {
		ID, Tool, MCPServer, MCPTool, Command, Output string
		Args                                          map[string]string
		Outcome                                       trace.Outcome
		OutCollections                                []trace.ResultCollection `json:",omitempty"`
	}{c.ID, c.Tool, c.MCPServer, c.MCPTool, c.Command, c.Output, c.Args, c.Outcome, c.OutCollections})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

func IntsToStrings(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return out
}

// GroupSpanProposals reduces review load without treating similar tool use
// as proof of one procedure. Exact shape keys must agree before grouping.
func GroupSpanProposals(ps []model.SpanProposal) []model.SpanGroup {
	by := map[string]*model.SpanGroup{}
	sessions := map[string]map[string]bool{}
	for _, p := range ps {
		g := by[p.ShapeKey]
		if g == nil {
			g = &model.SpanGroup{ShapeKey: p.ShapeKey, Example: p}
			by[p.ShapeKey] = g
			sessions[p.ShapeKey] = map[string]bool{}
		}
		g.Proposals++
		g.Members = append(g.Members, p.ID)
		sessions[p.ShapeKey][p.Client+"/"+p.Session] = true
		if p.EvidenceScore > g.Example.EvidenceScore || p.EvidenceScore == g.Example.EvidenceScore && p.Start.After(g.Example.Start) {
			g.Example = p
		}
	}
	out := make([]model.SpanGroup, 0, len(by))
	for key, g := range by {
		g.Sessions = len(sessions[key])
		sort.Strings(g.Members)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Proposals != out[j].Proposals {
			return out[i].Proposals > out[j].Proposals
		}
		return out[i].ShapeKey < out[j].ShapeKey
	})
	return out
}
