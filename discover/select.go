package discover

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Opportunity selection (TENG-3054, plan v3 section 0). The routine pass
// looks for the same sequence of steps across requests. The labeled useful
// procedures mostly do not have one: a scheduled prompt states the same
// procedure every run while the agent performs it differently, and a
// person's check is often one request in which the agent wrote a program
// and ran it again. This pass judges each request using mechanical evidence.
//
//   - stated_template: the request's text (digits aside) recurs in several
//     sessions, it states a procedure (several step lines, or paths and
//     commands), and the calls touch what it names. The goal, inputs and
//     output come from the stated text, not from a guess about the calls.
//   - rerun_check: the agent wrote a nontrivial program and ran it at least
//     twice in the request, in a request whose calls are commands rather
//     than code navigation or editing. The repeated program is the
//     procedure; what varied between its runs are its inputs.
//   - parametric_loop: one step acts on a fixed list visible in the request
//     or an earlier result, with one argument varying and no item selected
//     from an intermediate result.
//   - named_object: a short request names an object that calls act on, and
//     a later step uses an earlier result.
//
// Everything else is not recommended, with a reason. These routes do not
// establish semantic usefulness, so even a recommendation is only evidence
// for authoring. The brief marks it unassessed until the authoring agent
// establishes the contract.

// Opportunity is the selection pass's judgment of one request.
type Opportunity struct {
	ID          string   `json:"id"`
	Client      string   `json:"client"`
	Session     string   `json:"session"`
	Request     int      `json:"request"`
	Recommended bool     `json:"recommended"`
	Route       string   `json:"route,omitempty"`
	Reasons     []string `json:"reasons"`
	// Task is the reference `tap discover brief --task` takes.
	Task string `json:"task"`
	// Contract is what the procedure is, as far as the evidence shows:
	// the grouping key. Start is when the request began.
	Contract string    `json:"contract,omitempty"`
	Start    time.Time `json:"start,omitempty"`
}

// Selection routes.
const (
	RouteStatedTemplate = "stated_template"
	RouteRerunCheck     = "rerun_check"
	RouteParamLoop      = "parametric_loop"
	RouteNamedObject    = "named_object"
)

// Thresholds, fixed before the lineage-separated holdout was labeled.
const (
	templateMinSessions = 3
	stateMinStepLines   = 3
	stateMinAnchors     = 2
	rerunMinRuns        = 2
	rerunMinProgram     = 120
	navMaxShare         = 0.25
	editMaxShare        = 0.30
	minWorkSteps        = 2
	loopMinItems        = 2
	singlePassMaxSteps  = 15
)

var (
	stepLine  = regexp.MustCompile(`(?m)^\s*(?:[-*•]|\d+[.)])\s+\S`)
	anchorRe  = regexp.MustCompile("`[^`\n]{3,}`|(?:~|\\.{0,2})/[\\w.@-]+(?:/[\\w.@-]+)+|\\b[\\w-]+\\.(?:md|json|jsonl|csv|yaml|yml|go|py|ts|sh|txt|log)\\b")
	pathNoise = regexp.MustCompile(`(?:~|\.{0,2})/[\w.@/-]+`)
	numNoise  = regexp.MustCompile(`\b\d+\b`)
	strNoise  = regexp.MustCompile(`'[^'\n]*'|"[^"\n]*"`)
)

// SelectOpportunities judges every request of the corpus that made calls.
func SelectOpportunities(ss []trace.Session) []Opportunity {
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
	var out []Opportunity
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
			o := Opportunity{ID: trace.EpisodeID(ns.Client, ns.ID, r), Client: ns.Client, Session: ns.ID, Request: r,
				Task: ns.Client + "/" + ns.ID + "/" + strconv.Itoa(r)}
			var shell []string
			for _, c := range raw[ns.Client+"/"+ns.ID] {
				if c.Request == r && c.Tool == "shell" {
					shell = append(shell, c.Command)
				}
			}
			judgeOpportunity(&o, text, byReq[r], shell, len(sessionsByText[trace.TextKey(text)]))
			if st := byReq[r]; len(st) > 0 {
				o.Start = st[0].Time
			}
			out = append(out, o)
		}
	}
	return out
}

func judgeOpportunity(o *Opportunity, text string, steps []trace.Step, shell []string, textSessions int) {
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
		case trace.BookkeepingTools[st.Label]:
		case trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
			edits++
			work++
		case strings.HasPrefix(st.Label, "sh:") && viewingCall(st.Call, steps):
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
	if work < minWorkSteps {
		reason("fewer_than_two_work_steps")
		return
	}
	// Route 1: a recurring prompt that states its procedure.
	if textSessions >= templateMinSessions {
		lines := len(stepLine.FindAllString(text, -1))
		anchors := stateAnchors(text)
		touched := 0
		for _, a := range anchors {
			if touches(a, steps) {
				touched++
			}
		}
		switch {
		case lines < stateMinStepLines && len(anchors) < stateMinAnchors:
			reason("template_states_no_procedure")
		case touched == 0:
			reason("template_anchors_untouched")
		default:
			o.Recommended, o.Route = true, RouteStatedTemplate
			o.Contract = "template " + templateTitle(text)
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
		if len(cmd) >= rerunMinProgram && writesProgram(cmd) {
			runs[programKey(cmd)]++
		}
	}
	most, prog := 0, ""
	for k, n := range runs {
		if n > most || (n == most && k < prog) {
			most, prog = n, k
		}
	}
	loopLabel, loopItems, loopArg, loopSourceUnknown := parametricLoop(text, steps)
	switch {
	case most < rerunMinRuns && loopItems < loopMinItems:
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
		named, win, wNav, wEdit, dependent, seq := namedObject(text, steps)
		switch {
		case named == 0:
			reason("no_named_object_touched")
		case !dependent:
			reason("named_object_without_dependent_steps")
		case work > singlePassMaxSteps:
			// The whole request must be one short pass. Judging only the
			// window was tried on the diagnostic sets and added a false
			// positive and no true one.
			reason("single_pass_too_long:" + strconv.Itoa(work))
		case float64(wNav) >= navMaxShare*float64(win):
			reason("navigation_share:" + strconv.FormatFloat(float64(wNav)/float64(win), 'f', 2, 64))
		case float64(wEdit) >= editMaxShare*float64(win):
			reason("edit_share:" + strconv.FormatFloat(float64(wEdit)/float64(win), 'f', 2, 64))
		default:
			o.Reasons = nil
			o.Recommended, o.Route = true, RouteNamedObject
			reason("named_objects_touched:" + strconv.Itoa(named))
			reason("window_steps:" + strconv.Itoa(win))
			o.Contract = "single pass " + strings.Join(seq, " > ")
		}
	case navShare >= navMaxShare:
		reason("navigation_share:" + strconv.FormatFloat(navShare, 'f', 2, 64))
	case editShare >= editMaxShare:
		reason("edit_share:" + strconv.FormatFloat(editShare, 'f', 2, 64))
	case most >= rerunMinRuns:
		o.Recommended, o.Route = true, RouteRerunCheck
		reason("program_runs:" + strconv.Itoa(most))
		o.Contract = "program " + shortHash(prog) + ": " + trace.OneLine(prog, 60)
	default:
		o.Recommended, o.Route = true, RouteParamLoop
		reason("loop:" + loopLabel + ":" + strconv.Itoa(loopItems))
		o.Contract = "loop " + loopLabel + " over " + loopArg
	}
}

// parametricLoop finds a replayable step run on two or more items where
// exactly one argument varies. The complete list must be visible in the
// request or in one earlier result, and no later item may have been picked
// from an intermediate result. Otherwise its source and termination rule
// are unknown, even if the calls happen to look like a loop.
func parametricLoop(text string, steps []trace.Step) (string, int, string, bool) {
	byLabel := map[string][]int{}
	var order []string
	for i, st := range steps {
		if !(strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:")) {
			continue
		}
		if trace.BookkeepingTools[st.Label] || trace.StepEffect(st) == "write" {
			continue
		}
		if f := strings.Fields(strings.TrimPrefix(st.Label, "sh:")); strings.HasPrefix(st.Label, "sh:") && len(f) > 0 && (viewPrograms[f[0]] || trace.SearchPrograms[f[0]] || f[0] == "sed") {
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
		if len(idx) < loopMinItems {
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
		if t := types[key]; t != trace.SlotID && t != trace.SlotURL && t != trace.SlotPath && !(t == trace.SlotNumber && longNumbers(vals[key])) {
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
			source = loopListSource(text, steps, idx[0], vals[key])
			if source == "" {
				unknownSource = true
			}
		}
		if source != "" && len(idx) > bestN {
			best, bestN, bestArg = l, len(idx), key+" ("+types[key]+") "+itemShape(types[key], vals[key])+" from "+source
		}
	}
	return best, bestN, bestArg, unknownSource
}

func loopListSource(text string, steps []trace.Step, before int, items []string) string {
	all := func(has func(string) bool) bool {
		for _, item := range items {
			if !has(item) {
				return false
			}
		}
		return true
	}
	if all(func(item string) bool { return containsItem(text, item) }) {
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
			return containsItem(st.Output, item)
		}) {
			return "prior_output"
		}
	}
	return ""
}

// containsItem requires an item boundary, so ID 81234567 is not mistaken
// for a caller-supplied ID in 1812345679.
func containsItem(text, item string) bool {
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

// stateAnchors are the paths, file names and quoted commands a request
// names.
func stateAnchors(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range anchorRe.FindAllString(text, -1) {
		m = strings.Trim(m, "`")
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// touches reports whether a step's command or arguments name the anchor (a
// path by its last element, a command by its first word).
func touches(anchor string, steps []trace.Step) bool {
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

// programKey is a shell call with its paths, numbers and quoted strings
// abstracted, so the same program run on other inputs has the same key.
func programKey(raw string) string {
	k := pathNoise.ReplaceAllString(raw, "<p>")
	k = strNoise.ReplaceAllString(k, "<s>")
	k = numNoise.ReplaceAllString(k, "<n>")
	return strings.Join(strings.Fields(k), " ")
}

// viewPrograms print files or listings. A shell call made only of them is
// navigation, whatever its length.
var viewPrograms = map[string]bool{"cat": true, "head": true, "tail": true, "nl": true, "ls": true, "tree": true, "less": true, "wc": true, "stat": true}

// viewingCall reports whether every command of a shell call only views:
// a viewer, a search program, or sed printing (-n, no -i).
func viewingCall(call int, steps []trace.Step) bool {
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
		case viewPrograms[prog] || trace.SearchPrograms[prog]:
		case prog == "sed" && trace.StepEffect(st) == "read":
		case prog == "cd" || prog == "echo" || prog == "printf":
		default:
			return false
		}
		any = true
	}
	return any
}

// scriptMarks show a call carries a program the agent wrote: a heredoc,
// inline interpreter code, or a jq or awk program.
var scriptMarks = regexp.MustCompile(`<<-?\s*['"]?\w+|\b(?:python3?|node|ruby|perl|deno|bun)\s+(?:-c|-e|-)\s|\bjq\s+(?:-\w+\s+)*'[^']{8,}|\bawk\s+'[^']{8,}`)

// writesProgram reports whether a shell call runs a program written for it,
// rather than a single named command or a file view.
func writesProgram(cmd string) bool { return scriptMarks.MatchString(cmd) }

func longNumbers(vs []string) bool {
	for _, v := range vs {
		if len(v) < 5 {
			return false
		}
	}
	return true
}

// objectRe finds what a request names that a procedure could take as an
// input: an issue key, a URL, a long number, a path or a file name.
var objectRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b|https?://[^\s)>"'` + "`" + `]+|\b\d{5,}\b|(?:~|\.{0,2})/[\w.@-]+(?:/[\w.@-]+)+|\b[\w-]+\.(?:md|json|jsonl|csv|yaml|yml|go|py|ts|sh|txt|log|pdf|html)\b`)

// namedObject counts the objects the request names that some step acts on,
// and finds the window from the first such step to the last step that used
// a value an earlier step in the window produced. It returns the count, the
// window's work steps, how many of them navigate and edit, and whether any
// dependency was found.
func namedObject(text string, steps []trace.Step) (named, win, nav, edits int, dependent bool, seq []string) {
	seen := map[string]bool{}
	first := -1
	for _, m := range objectRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:")
		if len(m) < 4 || seen[m] {
			continue
		}
		seen[m] = true
		if i := firstTouch(m, steps); i >= 0 {
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
		if calls[st.Call] || trace.BookkeepingTools[st.Label] {
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
		case strings.HasPrefix(st.Label, "sh:") && viewingCall(st.Call, steps):
			nav++
		case strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:"):
		default:
			nav++
		}
	}
	return
}

// firstTouch is the index of the first step whose command or arguments name
// the object, or -1.
func firstTouch(obj string, steps []trace.Step) int {
	for i := range steps {
		if touches(obj, steps[i:i+1]) {
			return i
		}
	}
	return -1
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// OpportunityGroup is every recommended request with the same contract.
type OpportunityGroup struct {
	Contract string    `json:"contract"`
	Route    string    `json:"route"`
	Requests int       `json:"requests"`
	Sessions int       `json:"sessions"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	// Example is the most recent member: the one to brief.
	Example Opportunity `json:"example"`
	Members []string    `json:"members"`
}

// GroupOpportunities groups recommended requests by contract and ranks the
// groups: most distinct sessions first, then most requests, then the most
// recently seen. Grouping and ranking change what a person reads, not which
// requests are recommended.
func GroupOpportunities(ops []Opportunity) []OpportunityGroup {
	by := map[string]*OpportunityGroup{}
	sess := map[string]map[string]bool{}
	var order []string
	for _, o := range ops {
		if !o.Recommended {
			continue
		}
		k := o.Route + "\x00" + o.Contract
		g := by[k]
		if g == nil {
			g = &OpportunityGroup{Contract: o.Contract, Route: o.Route, First: o.Start, Last: o.Start, Example: o}
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
	out := make([]OpportunityGroup, 0, len(order))
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

// templateTitle is a stated prompt's first line, digits aside: the name a
// scheduled prompt keeps while its body is edited, so its versions group
// together. A prompt whose first line is too short to name it falls back to
// a hash of its whole text.
func templateTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		k := trace.TextKey(line)
		if len(k) >= 12 {
			return trace.OneLine(k, 80)
		}
		if strings.TrimSpace(line) != "" {
			break
		}
	}
	return shortHash(trace.TextKey(text))
}

// itemShape says what a loop's items are, so loops of one step over
// different things do not group together: the hosts of URLs, the parent
// directory of paths.
func itemShape(typ string, vs []string) string {
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
