package discover

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// This file decides what a routine is: its task contract (where each input
// comes from, what it changes, what it produces, where judgment sits) and,
// from that contract, whether it is a useful bounded procedure. Recurrence
// finds candidates; the contract decides them. See states.go for the
// dimensions and the plan in tap-discover-review-2026-09-29 for the rules.
//
// Some evidence comes from declared vocabularies (which tool arguments name
// an operation or an authority scope, which subcommands only read). They
// supply evidence for the contract. None of them decides suitability on its
// own, and an unknown tool yields "unknown", never a guess.

// selectorKeys are tool arguments whose value names the operation to run.
// A routine whose runs used different operations is not one procedure.
var selectorKeys = map[string]bool{"action": true, "operation": true, "op": true, "method": true, "verb": true, "tool": true, "tool_name": true, "command": true}

// scopeFlags and scopeArgs carry authority: which cluster, namespace,
// project or environment a call acts on.
var (
	scopeFlags = map[string]bool{"--context=": true, "--kube-context=": true, "-n=": true, "--namespace=": true, "--project=": true, "--profile=": true, "--region=": true, "--cluster=": true, "--env=": true, "--environment=": true, "-C=": true}
	scopeArgs  = map[string]bool{"integration": true, "project": true, "project_id": true, "project_key": true, "namespace": true, "context": true, "cluster": true, "environment": true, "env": true}
)

func isScopeSlot(st Step, sl Slot) bool {
	if strings.HasPrefix(st.Label, "sh:") {
		return scopeFlags[strings.SplitN(sl.Key, "#", 2)[0]]
	}
	return scopeArgs[sl.Key]
}

// Effect evidence.
var (
	readPrograms = map[string]bool{"cat": true, "head": true, "tail": true, "grep": true, "rg": true, "find": true, "ls": true, "wc": true, "awk": true, "jq": true, "sort": true, "uniq": true,
		"diff": true, "stat": true, "file": true, "du": true, "df": true, "date": true, "pwd": true, "shasum": true, "sha256sum": true, "md5": true, "md5sum": true, "tree": true, "which": true,
		"nl": true, "cut": true, "tr": true, "column": true, "less": true, "realpath": true, "basename": true, "dirname": true, "[": true, "test": true}
	readSub = map[string]map[string]bool{
		"git":     {"status": true, "diff": true, "log": true, "show": true, "rev-parse": true, "ls-files": true, "blame": true, "describe": true, "shortlog": true, "grep": true, "cat-file": true, "ls-remote": true, "rev-list": true, "merge-base": true},
		"kubectl": {"get": true, "describe": true, "logs": true, "top": true, "explain": true, "version": true, "api-resources": true, "auth": true},
		"gh":      {"view": true, "list": true, "status": true, "diff": true, "checks": true},
		"go":      {"test": true, "vet": true, "build": true, "list": true, "version": true, "env": true},
		"helm":    {"list": true, "status": true, "get": true, "history": true, "template": true, "show": true},
		"docker":  {"ps": true, "images": true, "logs": true, "inspect": true},
		"npm":     {"test": true, "ls": true, "view": true},
	}
	writeSub = map[string]map[string]bool{
		"git":     {"push": true, "commit": true, "tag": true, "merge": true, "rebase": true, "reset": true, "checkout": true, "add": true, "rm": true, "mv": true, "stash": true, "cherry-pick": true, "revert": true, "switch": true, "restore": true, "clean": true},
		"kubectl": {"apply": true, "create": true, "delete": true, "set": true, "patch": true, "scale": true, "rollout": true, "edit": true, "label": true, "annotate": true, "replace": true, "cordon": true, "drain": true},
		"helm":    {"install": true, "upgrade": true, "uninstall": true, "rollback": true},
		"docker":  {"push": true, "rm": true, "rmi": true, "run": true, "build": true},
		"npm":     {"publish": true, "install": true},
	}
	writePrograms = map[string]bool{"rm": true, "mv": true, "cp": true, "mkdir": true, "touch": true, "tee": true, "chmod": true, "chown": true, "ln": true, "rsync": true, "scp": true}
	toolWord      = regexp.MustCompile(`[a-z]+`)
	readVerbs     = map[string]bool{"get": true, "list": true, "search": true, "read": true, "describe": true, "fetch": true, "show": true, "view": true, "count": true, "query": true, "find": true, "lookup": true, "download": true, "status": true}
	writeVerbs    = map[string]bool{"create": true, "update": true, "delete": true, "add": true, "send": true, "post": true, "set": true, "transition": true, "merge": true, "complete": true, "checkpoint": true,
		"write": true, "remove": true, "publish": true, "upload": true, "edit": true, "assign": true, "move": true, "archive": true, "close": true, "reply": true, "forward": true, "trash": true, "share": true,
		"comment": true, "approve": true, "trigger": true, "run": true, "execute": true, "retry": true, "cancel": true, "restart": true, "deploy": true, "push": true, "apply": true, "patch": true, "store": true, "save": true, "insert": true}
)

// stepEffect is what one recorded step does by declared evidence: "read",
// "write" or "unknown".
func stepEffect(st Step) string {
	switch {
	case readTools[st.Label] || fetchTools[st.Label]:
		return "read"
	case editTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
		return "write"
	case strings.HasPrefix(st.Label, "sh:"):
		if st.Compound && hasFileRedirect(st.Raw) {
			return "write"
		}
		f := strings.Fields(strings.TrimPrefix(st.Label, "sh:"))
		prog := f[0]
		sub := ""
		if len(f) > 1 {
			sub = f[1]
		}
		if prog == "git" && sub == "branch" {
			for _, sl := range st.Slots {
				if sl.Value == "-d" || sl.Value == "-D" || sl.Value == "--delete" || sl.Value == "-m" {
					return "write"
				}
			}
			return "read"
		}
		if prog == "sed" {
			for _, sl := range st.Slots {
				if strings.HasPrefix(sl.Value, "-i") {
					return "write"
				}
			}
			return "read"
		}
		if prog == "curl" {
			for _, sl := range st.Slots {
				v := strings.ToUpper(sl.Value)
				k := strings.SplitN(sl.Key, "#", 2)[0]
				if (k == "-X=" || k == "--request=") && v != "GET" && v != "HEAD" {
					return "write"
				}
				if k == "-d" || k == "--data" || k == "-d=" || k == "--data=" || k == "-F=" || k == "--form=" || k == "--data-raw=" || k == "--data-binary=" {
					return "write"
				}
			}
			return "read"
		}
		if writePrograms[prog] || writeSub[prog][sub] {
			return "write"
		}
		if readPrograms[prog] || readSub[prog][sub] {
			return "read"
		}
		// A subcommand the label does not carry (a one-off word): look at the
		// recorded words for a known subcommand.
		for _, sl := range st.Slots {
			if writeSub[prog][sl.Value] {
				return "write"
			}
			if readSub[prog][sl.Value] && sl.Key == "p0" {
				return "read"
			}
		}
		return "unknown"
	case strings.HasPrefix(st.Label, "mcp:"):
		name := strings.TrimPrefix(st.Label, "mcp:")
		if strings.HasPrefix(name, "browser_") || name == "js" {
			return "unknown"
		}
		var action string
		for _, sl := range st.Slots {
			if selectorKeys[sl.Key] {
				action = sl.Value
			}
		}
		for _, text := range []string{action, name} {
			if text == "" {
				continue
			}
			words := toolWord.FindAllString(strings.ToLower(text), -1)
			for _, w := range words {
				if writeVerbs[w] {
					return "write"
				}
			}
			for _, w := range words {
				if readVerbs[w] {
					return "read"
				}
			}
		}
		return "unknown"
	}
	return "unknown"
}

var fileRedirectRe = regexp.MustCompile(`(^|[^0-9&<>])>>?\s*([^\s&|;]+)`)

// hasFileRedirect reports a > or >> redirect to something other than
// /dev/null or a file descriptor.
func hasFileRedirect(raw string) bool {
	for _, m := range fileRedirectRe.FindAllStringSubmatch(raw, -1) {
		if m[2] != "/dev/null" && !strings.HasPrefix(m[2], "&") {
			return true
		}
	}
	return false
}

// splitKey is what a request's run of the group's common steps must agree
// on to be the same procedure: the operation each tool call named and,
// when a step writes, the authority scope it wrote to.
func splitKey(steps []Step, common map[string]bool) string {
	var parts []string
	seen := map[string]bool{}
	for _, st := range steps {
		if !common[st.Label] || seen[st.Label] {
			continue
		}
		seen[st.Label] = true
		write := stepEffect(st) == "write"
		for _, sl := range st.Slots {
			if sl.Sub {
				continue
			}
			if !strings.HasPrefix(st.Label, "sh:") && selectorKeys[sl.Key] {
				parts = append(parts, st.Label+"|"+sl.Key+"="+sl.Value)
			}
			if write && isScopeSlot(st, sl) {
				parts = append(parts, st.Label+"|"+sl.Key+sl.Value)
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1f")
}

// splitGroup separates a group's requests by splitKey over the steps at
// least half of them ran. Subgroups keep corpus order.
func splitGroup(corpus []normSession, inst []reqInstance, g []int) [][]int {
	present := map[string]int{}
	for _, i := range g {
		s := corpus[inst[i].session]
		seen := map[string]bool{}
		for _, si := range inst[i].steps {
			if l := s.Steps[si].Label; replayable(l) && !seen[l] {
				seen[l] = true
				present[l]++
			}
		}
	}
	common := map[string]bool{}
	for l, n := range present {
		if 2*n >= len(g) {
			common[l] = true
		}
	}
	byKey := map[string][]int{}
	var order []string
	for _, i := range g {
		s := corpus[inst[i].session]
		steps := make([]Step, 0, len(inst[i].steps))
		for _, si := range inst[i].steps {
			steps = append(steps, s.Steps[si])
		}
		k := splitKey(steps, common)
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], i)
	}
	out := make([][]int, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

var goalWord = regexp.MustCompile(`[a-z][a-z]+`)

// goalTokens are the words of a request that are not the routine's input
// values: the template the caller filled in.
func goalTokens(text string, values []string) map[string]bool {
	t := strings.ToLower(text)
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); len(v) >= 2 {
			t = strings.ReplaceAll(t, v, " ")
		}
	}
	out := map[string]bool{}
	for _, w := range goalWord.FindAllString(t, -1) {
		out[w] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// sharesRun reports whether v and out share a run of at least n bytes.
func sharesRun(v, out string, n int) bool {
	if len(v) < n || len(out) < n {
		return false
	}
	for i := 0; i+n <= len(v); i += 4 {
		if strings.Contains(out, v[i:i+n]) {
			return true
		}
	}
	return false
}

// contractRun is one run of a routine with the evidence the contract needs.
type contractRun struct {
	steps     []Step
	all       []Step // every call of the request, in order
	text      string
	approvals int
}

// buildContract fills rt.Contract from the draft and its runs, and sets the
// remaining dimensions and the decision.
func buildContract(rt *Routine, d *Draft, runs []contractRun, loops []string) {
	c := &rt.Contract
	c.Inputs = nil
	// Effect and output.
	effect := "read"
	if len(runs) > 0 {
		for _, st := range runs[0].steps {
			switch stepEffect(st) {
			case "write":
				effect = "write"
			case "unknown":
				if effect != "write" {
					effect = "unknown"
				}
			}
		}
	} else {
		effect = "unknown"
	}
	switch effect {
	case "read":
		c.Effect, c.Output = EffectReadOnly, "report"
	case "write":
		c.Effect, c.Output = EffectWrites, "state_change"
	default:
		c.Effect, c.Output = EffectUnknown, "unknown"
	}
	// Scope: authority constants.
	scope := map[string]bool{}
	if len(runs) > 0 {
		for k, st := range runs[0].steps {
			for _, sl := range st.Slots {
				if !isScopeSlot(st, sl) {
					continue
				}
				same := true
				for _, r := range runs[1:] {
					if k >= len(r.steps) || slotValue(r.steps[k], sl.Key) != sl.Value {
						same = false
					}
				}
				if same {
					prog := strings.Fields(strings.TrimPrefix(st.Label, "sh:"))[0]
					if !strings.HasPrefix(st.Label, "sh:") {
						prog = strings.TrimPrefix(st.Label, "mcp:")
					}
					key := strings.SplitN(sl.Key, "#", 2)[0]
					if !strings.HasSuffix(key, "=") {
						key += "="
					}
					scope[prog+" "+key+sl.Value] = true
				}
			}
		}
	}
	c.Scope = nil
	for s := range scope {
		c.Scope = append(c.Scope, s)
	}
	sort.Strings(c.Scope)
	// Approvals.
	c.Approvals = 0
	for _, r := range runs {
		c.Approvals += r.approvals
	}
	// Inputs and their sources.
	var callerValues []string
	unresolvedRead, unresolvedWrite, composed := 0, 0, 0
	stepEff := func(pos int) string {
		if len(runs) == 0 || pos < 0 || pos >= len(runs[0].steps) {
			return "unknown"
		}
		return stepEffect(runs[0].steps[pos])
	}
	for n, in := range d.Inputs {
		ci := ContractInput{Name: in.Name, Type: in.Type}
		vals := d.inputValues(n)
		switch {
		case in.List:
			// Only lists the request gave become list inputs (loopSpecs).
			ci.Source = InputCaller
		case in.Sensitive:
			ci.Source = "credential"
		case in.DerivedFrom > 0:
			ci.Source, ci.From = InputPriorOutput, in.DerivedFrom
		default:
			hit, comp, total := 0, 0, 0
			for j, v := range vals {
				if v == "" || j >= len(runs) {
					continue
				}
				total++
				if inRequest(v, runs[j].text) || composedFromRequest(v, runs[j].text) {
					hit++
					continue
				}
				for _, st := range runs[j].all {
					if st.Output != "" && sharesRun(v, st.Output, 24) {
						comp++
						break
					}
				}
			}
			switch {
			case total > 0 && 2*hit >= total:
				ci.Source = InputCaller
				callerValues = append(callerValues, vals[0])
				for _, v := range vals {
					callerValues = append(callerValues, v)
				}
			case total > 0 && 2*comp >= total:
				ci.Source = InputComposed
				composed++
			default:
				// Nobody stated it and no earlier result held it. On a step
				// that only reads, the agent chose what to look at: the
				// selection rule is missing. On a write it is the action's
				// parameter, which a caller supplies when the goal is stated.
				ci.Source = InputUnresolved
				if stepEff(in.pos) == "write" {
					unresolvedWrite++
				} else {
					unresolvedRead++
				}
			}
		}
		if in.List {
			ci.Type = "list"
		}
		c.Inputs = append(c.Inputs, ci)
	}
	// Judgment, and whether it only sits at the end: a procedure that runs
	// its steps and then hands back to the agent for the last one (write
	// the summary, decide the fix) names that boundary; judgment in the
	// middle means the steps after it depend on a decision no program made.
	c.Judgment = nil
	firstJudg, lastPlain := -1, -1
	for k, st := range d.Steps {
		if st.Kind == KindHuman {
			c.Judgment = append(c.Judgment, st.Label)
			if firstJudg < 0 {
				firstJudg = k
			}
		} else if st.Kind != KindSkipped {
			lastPlain = k
		}
	}
	for n, in := range c.Inputs {
		if in.Source == InputComposed {
			c.Judgment = append(c.Judgment, "composed:"+in.Name)
			// A composed value's step is where judgment enters.
			if k := d.Inputs[n].pos; k >= 0 {
				if s := d.posStep[k]; s > 0 && (firstJudg < 0 || s-1 < firstJudg) {
					firstJudg = s - 1
				}
			}
		}
	}
	c.Boundary = ""
	// Only a human step (drafted as an explicit stop) can be the boundary.
	// A composed value would be drafted as an input to a call, hiding the
	// judgment inside it (plan section 3, item 6).
	if len(c.Judgment) > 0 && composed == 0 && firstJudg > lastPlain && firstJudg >= 2 {
		c.Boundary = "hands back to the agent before: " + strings.Join(c.Judgment, ", ")
	}
	// Goal.
	var toks []map[string]bool
	for k, r := range runs {
		if k == 25 {
			break
		}
		toks = append(toks, goalTokens(r.text, callerValues))
	}
	var sims []float64
	for a := 0; a < len(toks); a++ {
		for b := a + 1; b < len(toks); b++ {
			sims = append(sims, jaccard(toks[a], toks[b]))
		}
	}
	sort.Float64s(sims)
	hasCaller := false
	for _, in := range c.Inputs {
		if in.Source == InputCaller {
			hasCaller = true
		}
	}
	switch {
	case len(sims) > 0 && sims[len(sims)/2] >= 0.5:
		c.Goal = GoalStated
	case unresolvedRead == 0 && unresolvedWrite == 0 && (len(c.Judgment) == 0 || c.Boundary != "") && hasCaller:
		c.Goal = GoalSelfContained
	default:
		c.Goal = GoalUnknown
	}
	// Family: role, goal template, effect and output.
	fam := map[string]int{}
	for _, t := range toks {
		for w := range t {
			fam[w]++
		}
	}
	var words []string
	for w, k := range fam {
		if 2*k >= len(toks) {
			words = append(words, w)
		}
	}
	sort.Strings(words)
	if c.Goal == GoalUnknown {
		rt.Family = "unknown:" + rt.ID
	} else {
		h := sha256.Sum256([]byte(rt.SourceRole + "\x00" + strings.Join(words, " ") + "\x00" + c.Effect + "\x00" + c.Output))
		rt.Family = "fam_" + hex.EncodeToString(h[:6])
	}

	decide(rt, d, loops, unresolvedRead, unresolvedWrite)
}

// decide applies the suitability rules in order, then the draft status,
// outcome and value dimensions, and derives the legacy decision.
func decide(rt *Routine, d *Draft, loops []string, unresolvedRead, unresolvedWrite int) {
	c := &rt.Contract
	// A loop is bounded when its list's source is known: the request, or an
	// earlier result. Otherwise the agent chose the items as it went; over
	// reads that is exploration, over writes it is unknown selection.
	var openReadLoops, openOtherLoops, priorLoops []string
	for _, l := range loops {
		switch {
		case d != nil && d.listLoop[l]:
		case d != nil && d.priorLoop[l]:
			priorLoops = append(priorLoops, l)
		default:
			eff := "unknown"
			if d != nil {
				for p, st := range rt.Steps {
					if st.Label == l && len(d.firstRun) > p {
						eff = stepEffect(d.firstRun[p])
					}
				}
			}
			if eff == "read" || eff == "unknown" {
				openReadLoops = append(openReadLoops, l)
			} else {
				openOtherLoops = append(openOtherLoops, l)
			}
		}
	}
	rt.Reasons = nil
	reason := func(s string) { rt.Reasons = append(rt.Reasons, s) }
	nIn := len(c.Inputs)
	unresolvedNames := func() {
		for _, in := range c.Inputs {
			if in.Source == InputUnresolved {
				reason("unresolved_input:" + in.Name)
			}
		}
	}
	switch {
	case rt.SourceRole == RoleHarness:
		rt.Suitability = SuitInvalid
		reason("harness_request")
	case rt.SourceRole == RoleInfrastructure:
		rt.Suitability = SuitInsufficient
		reason("infrastructure_only")
	case len(openReadLoops) > 0 && c.Goal != GoalStated:
		// Different asks, and the agent picked what to read as it went.
		rt.Suitability = SuitInvestigation
		reason("loop_unbounded:" + strings.Join(openReadLoops, ","))
	case len(openReadLoops) > 0:
		// One stated task whose loop items came from somewhere the history
		// does not show (a page, a file beyond what was recorded): abstain.
		rt.Suitability = SuitInsufficient
		reason("loop_source_unknown:" + strings.Join(openReadLoops, ","))
	case len(openOtherLoops) > 0:
		rt.Suitability = SuitInsufficient
		reason("loop_selection_unknown:" + strings.Join(openOtherLoops, ","))
	case nIn > 0 && 2*unresolvedRead > nIn && c.Goal != GoalStated:
		rt.Suitability = SuitInvestigation
		reason("values_chosen_during_run")
	case unresolvedRead > 0:
		rt.Suitability = SuitInsufficient
		unresolvedNames()
	case len(c.Judgment) > 0 && c.Boundary == "":
		rt.Suitability = SuitInsufficient
		reason("judgment_step")
	case unresolvedWrite > 0 && c.Goal != GoalStated:
		rt.Suitability = SuitInsufficient
		unresolvedNames()
	case c.Goal == GoalUnknown:
		rt.Suitability = SuitInsufficient
		reason("goal_unknown")
	case rt.Consistency < 0.5:
		rt.Suitability = SuitInsufficient
		reason("inconsistent_order")
	case !hasParam(c) && rt.Coverage < 0.5:
		// Nothing to parameterize and a small part of what the requests
		// did: the constant opening of varying work (open the browser,
		// print the directory), not the procedure.
		rt.Suitability = SuitInsufficient
		reason("constant_part_of_larger_work")
	default:
		rt.Suitability = SuitUseful
		reason("contract_complete:" + c.Goal)
	}
	// Draft status.
	rt.Blockers = nil
	switch {
	case rt.Suitability != SuitUseful:
		rt.DraftStatus = DraftNotAttempted
	case d == nil:
		rt.DraftStatus = DraftNotAttempted
	case len(d.Blocked) > 0:
		rt.DraftStatus = DraftBlocked
	default:
		for _, in := range d.Inputs {
			if in.DerivedFrom > 0 && in.Extract == "" {
				rt.Blockers = append(rt.Blockers, "output_binding:"+in.Name)
			}
		}
		if c.Approvals > 0 {
			rt.Blockers = append(rt.Blockers, "approval_between_steps")
		}
		for _, l := range priorLoops {
			rt.Blockers = append(rt.Blockers, "loop_collection_binding:"+l)
		}
		if len(d.Problems) > 0 {
			rt.Blockers = append(rt.Blockers, "manifest_problems")
		}
		if len(rt.Blockers) > 0 {
			rt.DraftStatus = DraftNeedsAuthor
		} else {
			rt.DraftStatus = DraftComplete
		}
	}
	// Legacy single decision, kept for callers that list primitives.
	switch {
	case rt.Suitability == SuitUseful && rt.SourceRole == RoleScheduled:
		rt.Decision = "baseline"
	case rt.Suitability == SuitUseful && rt.DraftStatus == DraftComplete:
		rt.Decision = "primitive"
	case rt.Suitability == SuitUseful:
		rt.Decision = "needs_authoring"
	default:
		rt.Decision = "removed"
	}
	if rt.Decision == "removed" {
		rt.Failed = strings.SplitN(rt.Reasons[0], ":", 2)[0]
	} else {
		rt.Failed = ""
	}
	rt.Why = strings.Join(rt.Reasons, "; ")
	if len(rt.Blockers) > 0 {
		rt.Why += "; blockers: " + strings.Join(rt.Blockers, ", ")
	}
}

// hasParam reports an input a caller or an earlier result supplies.
func hasParam(c *Contract) bool {
	for _, in := range c.Inputs {
		if in.Source == InputCaller || in.Source == InputPriorOutput || in.Source == InputUnresolved {
			return true
		}
	}
	return false
}

var compositeSep = regexp.MustCompile(`\.\.\.?|[=:,]`)

// composedFromRequest reports a value built from what the request said:
// split at the operators that join values (a range a..b, a selector k=v, a
// list a,b), some part is in the request and every other part is a short
// fixed word (a key such as involvedObject.name). A path is not split, so
// a file under a directory the request named is still the agent's choice.
func composedFromRequest(v, text string) bool {
	if !compositeSep.MatchString(v) || strings.ContainsAny(v, " /") {
		return false
	}
	parts := compositeSep.Split(v, -1)
	in := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case len(p) >= 3 && inRequest(p, text):
			in++
		case !strings.ContainsAny(p, "0123456789") && len(p) <= 40:
			// a fixed key or keyword
		default:
			return false
		}
	}
	return in > 0
}
