package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Thresholds, fixed before the lineage-separated holdout was labeled.
const (
	TemplateMinSessions = 3
	StateMinStepLines   = 3
	StateMinAnchors     = 2
	RerunMinRuns        = 2
	RerunMinProgram     = 120
	NavMaxShare         = 0.25
	EditMaxShare        = 0.30
	MinWorkSteps        = 2
	LoopMinItems        = 2
	SinglePassMaxSteps  = 15
)

var (
	StepLine  = regexp.MustCompile(`(?m)^\s*(?:[-*•]|\d+[.)])\s+\S`)
	AnchorRe  = regexp.MustCompile("`[^`\n]{3,}`|(?:~|\\.{0,2})/[\\w.@-]+(?:/[\\w.@-]+)+|\\b[\\w-]+\\.(?:md|json|jsonl|csv|yaml|yml|go|py|ts|sh|txt|log)\\b")
	PathNoise = regexp.MustCompile(`(?:~|\.{0,2})/[\w.@/-]+`)
	NumNoise  = regexp.MustCompile(`\b\d+\b`)
	StrNoise  = regexp.MustCompile(`'[^'\n]*'|"[^"\n]*"`)
)

// SelectOpportunities judges every request of the corpus that made calls.
func SelectOpportunities(ss []trace.Session) []model.Opportunity {
	ss = append([]trace.Session(nil), ss...)
	for i := range ss {
		ss[i].Calls = append([]trace.Call(nil), ss[i].Calls...)
	}
	trace.DropCopiedCalls(ss)
	raw := map[string][]trace.Call{}
	for _, s := range ss {
		raw[s.Client+"/"+s.ID] = s.Calls
	}
	norm := trace.Normalize(ss)
	sessionsByText := map[string]map[string]bool{}
	for _, ns := range norm {
		for _, r := range ns.Requests {
			k := trace.TextKey(r)
			if k == "" {
				continue
			}
			if sessionsByText[k] == nil {
				sessionsByText[k] = map[string]bool{}
			}
			sessionsByText[k][ns.Client+"/"+ns.ID] = true
		}
	}
	var out []model.Opportunity
	for i := range norm {
		ns := &norm[i]
		byReq := map[int][]trace.Step{}
		for _, st := range ns.Steps {
			byReq[st.Request] = append(byReq[st.Request], st)
		}
		reqs := make([]int, 0, len(byReq))
		for r := range byReq {
			reqs = append(reqs, r)
		}
		sort.Ints(reqs)
		for _, r := range reqs {
			text := ""
			if r < len(ns.Requests) {
				text = ns.Requests[r]
			}
			o := model.Opportunity{ID: trace.EpisodeID(ns.Client, ns.ID, r), Client: ns.Client, Session: ns.ID, Request: r,
				Task: ns.Client + "/" + ns.ID + "/" + strconv.Itoa(r)}
			var shell []string
			for _, c := range raw[ns.Client+"/"+ns.ID] {
				if c.Request == r && c.Tool == "shell" {
					shell = append(shell, c.Command)
				}
			}
			JudgeOpportunity(&o, text, byReq[r], shell, len(sessionsByText[trace.TextKey(text)]))
			if st := byReq[r]; len(st) > 0 {
				o.Start = st[0].Time
			}
			out = append(out, o)
		}
	}
	return out
}

func JudgeOpportunity(o *model.Opportunity, text string, steps []trace.Step, shell []string, textSessions int) {
	reason := func(s string) { o.Reasons = append(o.Reasons, s) }
	if strings.TrimSpace(text) == "" || trace.IsHarness(text) || strings.HasPrefix(strings.TrimSpace(text), "<") {
		reason("no_request_text")
		return
	}
	var work, nav, edits int
	calls := map[int]bool{}
	for _, st := range steps {
		if calls[st.Call] {
			continue // one call can hold several steps
		}
		calls[st.Call] = true
		switch {
		case trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
			edits++
			work++
		case strings.HasPrefix(st.Label, "sh:") && ViewingCall(st.Call, steps):
			// Printing a file or a listing through the shell is navigation
			// too (sed -n, cat, head, rg ...).
			nav++
			work++
		case strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:"):
			work++
		default:
			// A client's own read, search or listing tool: navigation.
			nav++
			work++
		}
	}
	if work < MinWorkSteps {
		reason("fewer_than_two_work_steps")
		return
	}
	// Route 1: a recurring prompt that states its procedure.
	if textSessions >= TemplateMinSessions {
		lines := len(StepLine.FindAllString(text, -1))
		anchors := StateAnchors(text)
		touched := 0
		for _, a := range anchors {
			if Touches(a, steps) {
				touched++
			}
		}
		switch {
		case lines < StateMinStepLines && len(anchors) < StateMinAnchors:
			reason("template_states_no_procedure")
		case touched == 0:
			reason("template_anchors_untouched")
		default:
			o.Recommended, o.Route = true, model.RouteStatedTemplate
			o.Contract = "template " + TemplateTitle(text)
			reason("template_sessions:" + strconv.Itoa(textSessions))
			reason("step_lines:" + strconv.Itoa(lines))
			reason("anchors_touched:" + strconv.Itoa(touched) + "/" + strconv.Itoa(len(anchors)))
			return
		}
	}
	// Route 2: a program the agent wrote and ran again.
	navShare, editShare := float64(nav)/float64(work), float64(edits)/float64(work)
	// Counted over the recorded calls: normalizing merges identical repeats.
	runs := map[string]int{}
	for _, cmd := range shell {
		if len(cmd) >= RerunMinProgram && WritesProgram(cmd) {
			runs[ProgramKey(cmd)]++
		}
	}
	most, prog := 0, ""
	for k, n := range runs {
		if n > most || (n == most && k < prog) {
			most, prog = n, k
		}
	}
	loopLabel, loopItems, loopArg, loopSourceUnknown := ParametricLoop(text, steps)
	switch {
	case most < RerunMinRuns && loopItems < LoopMinItems:
		reason("no_rerun_program")
		if loopSourceUnknown {
			reason("loop_source_unknown")
		} else {
			reason("no_parametric_loop")
		}
		// Route 4: one short pass over an object the request names: the
		// request is short, a step acts on a named object, and a later step
		// depends on an earlier one. The navigation and edit gates apply to
		// the window from the first such step to the last dependent step.
		named, win, wNav, wEdit, dependent, seq := NamedObject(text, steps)
		switch {
		case named == 0:
			reason("no_named_object_touched")
		case !dependent:
			reason("named_object_without_dependent_steps")
		case work > SinglePassMaxSteps:
			// The whole request must be one short pass. Judging only the
			// window was tried on the diagnostic sets and added a false
			// positive and no true one.
			reason("single_pass_too_long:" + strconv.Itoa(work))
		case float64(wNav) >= NavMaxShare*float64(win):
			reason("navigation_share:" + strconv.FormatFloat(float64(wNav)/float64(win), 'f', 2, 64))
		case float64(wEdit) >= EditMaxShare*float64(win):
			reason("edit_share:" + strconv.FormatFloat(float64(wEdit)/float64(win), 'f', 2, 64))
		default:
			o.Reasons = nil
			o.Recommended, o.Route = true, model.RouteNamedObject
			reason("named_objects_touched:" + strconv.Itoa(named))
			reason("window_steps:" + strconv.Itoa(win))
			o.Contract = "single pass " + strings.Join(seq, " > ")
		}
	case navShare >= NavMaxShare:
		reason("navigation_share:" + strconv.FormatFloat(navShare, 'f', 2, 64))
	case editShare >= EditMaxShare:
		reason("edit_share:" + strconv.FormatFloat(editShare, 'f', 2, 64))
	case most >= RerunMinRuns:
		o.Recommended, o.Route = true, model.RouteRerunCheck
		reason("program_runs:" + strconv.Itoa(most))
		o.Contract = "program " + ShortHash(prog) + ": " + trace.OneLine(prog, 60)
	default:
		o.Recommended, o.Route = true, model.RouteParamLoop
		reason("loop:" + loopLabel + ":" + strconv.Itoa(loopItems))
		o.Contract = "loop " + loopLabel + " over " + loopArg
	}
}

// ParametricLoop finds a replayable step run on two or more items where
// exactly one argument varies. The complete list must be visible in the
// request or in one earlier result, and no later item may have been picked
// from an intermediate result. Otherwise its source and termination rule
// are unknown, even if the calls happen to look like a loop.
func ParametricLoop(text string, steps []trace.Step) (string, int, string, bool) {
	byLabel := map[string][]int{}
	var order []string
	for i, st := range steps {
		if !(strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:")) {
			continue
		}
		if trace.StepEffect(st) == "write" {
			continue
		}
		if f := strings.Fields(strings.TrimPrefix(st.Label, "sh:")); strings.HasPrefix(st.Label, "sh:") && len(f) > 0 && (ViewPrograms[f[0]] || trace.SearchPrograms[f[0]] || f[0] == "sed") {
			continue // viewing files one after another is navigation
		}
		if _, ok := byLabel[st.Label]; !ok {
			order = append(order, st.Label)
		}
		byLabel[st.Label] = append(byLabel[st.Label], i)
	}
	best, bestN, bestArg, unknownSource := "", 0, "", false
	for _, l := range order {
		idx := byLabel[l]
		if len(idx) < LoopMinItems {
			continue
		}
		// The one varying argument.
		vals := map[string][]string{}
		for _, i := range idx {
			for _, sl := range steps[i].Slots {
				if sl.Sub || sl.Type == trace.SlotFlag || trace.Derived(sl.Key) {
					continue
				}
				vals[sl.Key] = append(vals[sl.Key], sl.Value)
			}
		}
		key := ""
		ok := true
		types := map[string]string{}
		for _, sl := range steps[idx[0]].Slots {
			types[sl.Key] = sl.Type
		}
		for k, vs := range vals {
			if len(vs) != len(idx) {
				continue
			}
			distinct := map[string]bool{}
			for _, v := range vs {
				distinct[v] = true
			}
			if len(distinct) == 1 {
				continue
			}
			if key != "" {
				ok = false // two arguments vary: not one list
				break
			}
			if len(distinct) != len(vs) {
				ok = false // an item repeated: a retry, not a list
				break
			}
			key = k
		}
		// A list a caller gives names things: ids, URLs, paths. A loop over
		// free text (search phrasings, scripts) is the agent trying things.
		// A number of five or more digits is an id (a job, a pipeline); a
		// short one is a count or a page.
		if t := types[key]; t != trace.SlotID && t != trace.SlotURL && t != trace.SlotPath && !(t == trace.SlotNumber && LongNumbers(vals[key])) {
			ok = false
		}
		if !ok || key == "" {
			continue
		}
		// No item may come from what the loop read since the last item.
		for n := 1; n < len(idx) && ok; n++ {
			v := vals[key][n]
			for j := idx[n-1]; j < idx[n]; j++ {
				if trace.InResult(v, steps[j]) {
					ok = false
					break
				}
			}
		}
		source := ""
		if ok {
			source = LoopListSource(text, steps, idx[0], vals[key])
			if source == "" {
				unknownSource = true
			}
		}
		if source != "" && len(idx) > bestN {
			best, bestN, bestArg = l, len(idx), key+" ("+types[key]+") "+ItemShape(types[key], vals[key])+" from "+source
		}
	}
	return best, bestN, bestArg, unknownSource
}

func LoopListSource(text string, steps []trace.Step, before int, items []string) string {
	all := func(has func(string) bool) bool {
		for _, item := range items {
			if !has(item) {
				return false
			}
		}
		return true
	}
	if all(func(item string) bool { return ContainsItem(text, item) }) {
		return "caller"
	}
	for i := 0; i < before; i++ {
		st := steps[i]
		if st.Outcome == trace.OutcomeFailed {
			continue
		}
		if all(func(item string) bool {
			for _, id := range st.OutIDs {
				if id == item {
					return true
				}
			}
			for _, token := range st.OutTokens {
				if token == item {
					return true
				}
			}
			return ContainsItem(st.Output, item)
		}) {
			return "prior_output"
		}
	}
	return ""
}

// ContainsItem requires an item boundary, so ID 81234567 is not mistaken
// for a caller-supplied ID in 1812345679.
func ContainsItem(text, item string) bool {
	if item == "" {
		return false
	}
	word := func(b byte) bool {
		return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '_'
	}
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], item)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(item)
		if (i == 0 || !word(text[i-1]) || !word(item[0])) &&
			(end == len(text) || !word(text[end]) || !word(item[len(item)-1])) {
			return true
		}
		offset = i + 1
	}
	return false
}

// StateAnchors are the paths, file names and quoted commands a request
// names.
func StateAnchors(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range AnchorRe.FindAllString(text, -1) {
		m = strings.Trim(m, "`")
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// Touches reports whether a step's command or arguments name the anchor (a
// path by its last element, a command by its first word).
func Touches(anchor string, steps []trace.Step) bool {
	key := anchor
	if strings.Contains(anchor, "/") {
		key = anchor[strings.LastIndex(anchor, "/")+1:]
	} else if f := strings.Fields(anchor); len(f) > 1 {
		key = f[0] + " " + f[1]
	}
	if len(key) < 3 {
		return false
	}
	for _, st := range steps {
		if strings.Contains(st.Raw, key) {
			return true
		}
		for _, sl := range st.Slots {
			if strings.Contains(sl.Value, key) {
				return true
			}
		}
	}
	return false
}

// ProgramKey is a shell call with its paths, numbers and quoted strings
// abstracted, so the same program run on other inputs has the same key.
func ProgramKey(raw string) string {
	k := PathNoise.ReplaceAllString(raw, "<p>")
	k = StrNoise.ReplaceAllString(k, "<s>")
	k = NumNoise.ReplaceAllString(k, "<n>")
	return strings.Join(strings.Fields(k), " ")
}

// ViewPrograms print files or listings. A shell call made only of them is
// navigation, whatever its length.
var ViewPrograms = map[string]bool{"cat": true, "head": true, "tail": true, "nl": true, "ls": true, "tree": true, "less": true, "wc": true, "stat": true}

// ViewingCall reports whether every command of a shell call only views:
// a viewer, a search program, or sed printing (-n, no -i).
func ViewingCall(call int, steps []trace.Step) bool {
	any := false
	for _, st := range steps {
		if st.Call != call {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(st.Label, "sh:"))
		if len(f) == 0 {
			return false
		}
		prog := f[0]
		switch {
		case ViewPrograms[prog] || trace.SearchPrograms[prog]:
		case prog == "sed" && trace.StepEffect(st) == "read":
		case prog == "cd" || prog == "echo" || prog == "printf":
		default:
			return false
		}
		any = true
	}
	return any
}

// ScriptMarks show a call carries a program the agent wrote: a heredoc,
// inline interpreter code, or a jq or awk program.
var ScriptMarks = regexp.MustCompile(`<<-?\s*['"]?\w+|\b(?:python3?|node|ruby|perl|deno|bun)\s+(?:-c|-e|-)\s|\bjq\s+(?:-\w+\s+)*'[^']{8,}|\bawk\s+'[^']{8,}`)

// WritesProgram reports whether a shell call runs a program written for it,
// rather than a single named command or a file view.
func WritesProgram(cmd string) bool { return ScriptMarks.MatchString(cmd) }

func LongNumbers(vs []string) bool {
	for _, v := range vs {
		if len(v) < 5 {
			return false
		}
	}
	return true
}

// ObjectRe finds what a request names that a procedure could take as an
// input: an issue key, a URL, a long number, a path or a file name.
var ObjectRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b|https?://[^\s)>"'` + "`" + `]+|\b\d{5,}\b|(?:~|\.{0,2})/[\w.@-]+(?:/[\w.@-]+)+|\b[\w-]+\.(?:md|json|jsonl|csv|yaml|yml|go|py|ts|sh|txt|log|pdf|html)\b`)

// NamedObject counts the objects the request names that some step acts on,
// and finds the window from the first such step to the last step that used
// a value an earlier step in the window produced. It returns the count, the
// window's work steps, how many of them navigate and edit, and whether any
// dependency was found.
func NamedObject(text string, steps []trace.Step) (named, win, nav, edits int, dependent bool, seq []string) {
	seen := map[string]bool{}
	first := -1
	for _, m := range ObjectRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:")
		if len(m) < 4 || seen[m] {
			continue
		}
		seen[m] = true
		if i := FirstTouch(m, steps); i >= 0 {
			named++
			if first < 0 || i < first {
				first = i
			}
		}
	}
	if named == 0 {
		return
	}
	last := -1
	for j := first + 1; j < len(steps); j++ {
		for _, sl := range steps[j].Slots {
			if sl.Sub || sl.Type == trace.SlotFlag || len(sl.Value) < 4 {
				continue
			}
			for i := first; i < j; i++ {
				if steps[i].Call != steps[j].Call && trace.InResult(sl.Value, steps[i]) {
					last = j
				}
			}
		}
	}
	if last < 0 {
		return
	}
	dependent = true
	calls := map[int]bool{}
	for _, st := range steps[first : last+1] {
		if calls[st.Call] {
			continue
		}
		calls[st.Call] = true
		win++
		if len(seq) == 0 || seq[len(seq)-1] != st.Label {
			seq = append(seq, st.Label)
		}
		switch {
		case trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
			edits++
		case strings.HasPrefix(st.Label, "sh:") && ViewingCall(st.Call, steps):
			nav++
		case strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:"):
		default:
			nav++
		}
	}
	return
}

// FirstTouch is the index of the first step whose command or arguments name
// the object, or -1.
func FirstTouch(obj string, steps []trace.Step) int {
	for i := range steps {
		if Touches(obj, steps[i:i+1]) {
			return i
		}
	}
	return -1
}

func ShortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// GroupOpportunities groups recommended requests by contract and ranks the
// groups: most distinct sessions first, then most requests, then the most
// recently seen. Grouping and ranking change what a person reads, not which
// requests are recommended.
func GroupOpportunities(ops []model.Opportunity) []model.OpportunityGroup {
	by := map[string]*model.OpportunityGroup{}
	sess := map[string]map[string]bool{}
	var order []string
	for _, o := range ops {
		if !o.Recommended {
			continue
		}
		k := o.Route + "\x00" + o.Contract
		g := by[k]
		if g == nil {
			g = &model.OpportunityGroup{Contract: o.Contract, Route: o.Route, First: o.Start, Last: o.Start, Example: o}
			by[k] = g
			sess[k] = map[string]bool{}
			order = append(order, k)
		}
		g.Requests++
		g.Members = append(g.Members, o.ID)
		sess[k][o.Client+"/"+o.Session] = true
		if o.Start.Before(g.First) {
			g.First = o.Start
		}
		if !o.Start.Before(g.Last) {
			g.Last, g.Example = o.Start, o
		}
	}
	out := make([]model.OpportunityGroup, 0, len(order))
	for _, k := range order {
		g := by[k]
		g.Sessions = len(sess[k])
		sort.Strings(g.Members)
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Sessions != b.Sessions:
			return a.Sessions > b.Sessions
		case a.Requests != b.Requests:
			return a.Requests > b.Requests
		case !a.Last.Equal(b.Last):
			return a.Last.After(b.Last)
		}
		return a.Contract < b.Contract
	})
	return out
}

// TemplateTitle is a stated prompt's first line, digits aside: the name a
// scheduled prompt keeps while its body is edited, so its versions group
// together. A prompt whose first line is too short to name it falls back to
// a hash of its whole text.
func TemplateTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		k := trace.TextKey(line)
		if len(k) >= 12 {
			return trace.OneLine(k, 80)
		}
		if strings.TrimSpace(line) != "" {
			break
		}
	}
	return ShortHash(trace.TextKey(text))
}

// ItemShape says what a loop's items are, so loops of one step over
// different things do not group together: the hosts of URLs, the parent
// directory of paths.
func ItemShape(typ string, vs []string) string {
	set := map[string]bool{}
	for _, v := range vs {
		switch typ {
		case trace.SlotURL:
			h := v
			if i := strings.Index(h, "://"); i >= 0 {
				h = h[i+3:]
			}
			if i := strings.IndexAny(h, "/?#"); i >= 0 {
				h = h[:i]
			}
			set[h] = true
		case trace.SlotPath:
			d := v
			if i := strings.LastIndex(strings.TrimRight(d, "/"), "/"); i >= 0 {
				d = d[:i]
			}
			if i := strings.LastIndex(d, "/"); i >= 0 {
				d = d[i+1:]
			}
			set[d+"/"] = true
		}
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > 3 {
		out = append(out[:3], "…")
	}
	return strings.Join(out, ",")
}
