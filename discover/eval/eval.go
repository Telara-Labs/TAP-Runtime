// Package eval is the evaluation corpus: reproducible samples of task episodes from a frozen
// corpus, labeled from their own evidence, and the request-level episode selection.
package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/routine"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// EpisodeClaim is the single-request judgment.
type EpisodeClaim struct {
	ID          string   `json:"id"`
	Client      string   `json:"client"`
	Session     string   `json:"session"`
	Request     int      `json:"request"`
	Suitability string   `json:"suitability"`
	Reasons     []string `json:"reasons"`
	Steps       []string `json:"steps"`
	// Unexplained lists values no source accounts for (step: value kind).
	Unexplained []string `json:"unexplained,omitempty"`
	// Accounted is, per step, whether every value of it has a source (a
	// judgment step is false).
	Accounted []bool `json:"accounted,omitempty"`
}

// AssessEpisodes judges each episode on its own contract.
func (c *Corpus) AssessEpisodes(eps []Episode) []EpisodeClaim {
	norm := trace.Normalize(c.Sessions)
	byKey := map[string]*trace.NormSession{}
	for i := range norm {
		byKey[norm[i].Client+"/"+norm[i].ID] = &norm[i]
	}
	var out []EpisodeClaim
	for _, e := range eps {
		ec := EpisodeClaim{ID: e.ID, Client: e.Client, Session: e.Session, Request: e.Request}
		ns := byKey[e.Client+"/"+e.Session]
		if ns == nil {
			ec.Suitability, ec.Reasons = model.SuitInsufficient, []string{"no_steps"}
			out = append(out, ec)
			continue
		}
		AssessEpisode(ns, e.Request, &ec)
		out = append(out, ec)
	}
	return out
}

func AssessEpisode(ns *trace.NormSession, req int, ec *EpisodeClaim) {
	text := ""
	if req < len(ns.Requests) {
		text = ns.Requests[req]
	}
	var all []trace.Step
	for _, st := range ns.Steps {
		if st.Request == req {
			all = append(all, st)
		}
	}
	reason := func(s string) { ec.Reasons = append(ec.Reasons, s) }
	if strings.TrimSpace(text) == "" || trace.IsHarness(text) {
		ec.Suitability = model.SuitInvalid
		reason("no_request_text")
		return
	}
	// The work: replayable steps that are not the agent's bookkeeping, and
	// edits (judgment) in their position.
	type item struct {
		st    trace.Step
		human bool
	}
	var work []item
	book := 0
	for _, st := range all {
		switch {
		case trace.BookkeepingTools[st.Label]:
			book++
		case trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
			work = append(work, item{st, true})
		case trace.Replayable(st.Label):
			work = append(work, item{st, false})
		}
	}
	for _, w := range work {
		ec.Steps = append(ec.Steps, w.st.Label)
	}
	plain := 0
	for _, w := range work {
		if !w.human {
			plain++
		}
	}
	switch {
	case plain == 0 && book > 0:
		ec.Suitability = model.SuitInsufficient
		reason("infrastructure_only")
		return
	case plain < 2:
		ec.Suitability = model.SuitInsufficient
		reason("fewer_than_two_steps")
		return
	}
	long := len(work) > 25
	// Judgment: human steps only at the end are the hand-back boundary.
	lastPlain, firstHuman := -1, -1
	for k, w := range work {
		if w.human {
			if firstHuman < 0 {
				firstHuman = k
			}
		} else {
			lastPlain = k
		}
	}
	judged := firstHuman >= 0 && firstHuman < lastPlain
	// Every value must have a source.
	chosenSteps, readChosen := 0, 0
	ec.Accounted = make([]bool, len(work))
	defer func() {
		for k := range ec.Accounted {
			ec.Accounted[k] = !work[k].human && !strings.Contains(strings.Join(ec.Unexplained, " "), fmt.Sprintf("step%d:", k+1))
		}
	}()
	for k, w := range work {
		if w.human {
			continue
		}
		prog := strings.Fields(strings.TrimPrefix(w.st.Label, "sh:"))[0]
		unexplained := false
		for _, sl := range w.st.Slots {
			if sl.Sub || sl.Type == trace.SlotFlag || sl.Type == trace.SlotNumber || trace.Derived(sl.Key) || sl.Key == "recv" || len(sl.Value) < 3 {
				continue
			}
			if sl.Type == trace.SlotWord && !(strings.HasPrefix(w.st.Label, "sh:") && trace.SearchPrograms[prog]) {
				continue // a fixed word of the command: a resource kind, a branch
			}
			v := sl.Value
			if trace.InRequest(v, text) || routine.ComposedFromRequest(v, text) {
				continue
			}
			from := false
			for _, prev := range all {
				if prev.Call >= w.st.Call {
					break
				}
				if trace.InResult(v, prev) {
					from = true
					break
				}
			}
			if from {
				continue
			}
			unexplained = true
			ec.Unexplained = append(ec.Unexplained, fmt.Sprintf("step%d:%s", k+1, sl.Type))
		}
		if unexplained {
			chosenSteps++
			if trace.StepEffect(w.st) != "write" {
				readChosen++
			}
		}
	}
	switch {
	case long:
		ec.Suitability = model.SuitInvestigation
		reason(fmt.Sprintf("unbounded_length:%d", len(work)))
	case judged:
		ec.Suitability = model.SuitInsufficient
		reason("judgment_step")
	case readChosen > 0 && 2*chosenSteps > plain:
		ec.Suitability = model.SuitInvestigation
		reason("values_chosen_during_run")
	case chosenSteps > 0:
		ec.Suitability = model.SuitInsufficient
		reason("unexplained_values")
	default:
		ec.Suitability = model.SuitUseful
		reason("single_episode_contract")
		if firstHuman >= 0 {
			reason("hands_back_before_judgment")
		}
	}
}

// Episode is one request of one session.
type Episode struct {
	// ID is opaque: it names the episode without revealing its source.
	ID      string    `json:"id"`
	Client  string    `json:"client"`
	Session string    `json:"session"`
	Request int       `json:"request"`
	Calls   int       `json:"calls"`
	Start   time.Time `json:"start"`
	// Lineage groups episodes that must stay on one side of a split: the
	// same session's resumed copies and requests with the same text.
	Lineage string `json:"lineage"`
	// Stratum says why the episode was sampled. It is withheld from labelers.
	Stratum string `json:"stratum"`
	// Routine is the routine whose sources included it, if any; withheld
	// from labelers.
	Routine string `json:"routine,omitempty"`
}

// EpisodeKey locates a request in the corpus.
type EpisodeKey struct {
	Client, Session string
	Request         int
}

// Corpus indexes the frozen sessions for sampling and rendering.
type Corpus struct {
	Sessions []trace.Session
	ByKey    map[string]int    `json:"-"` // client/session -> index
	Lineage  map[string]string `json:"-"`
}

// NewCorpus drops copied calls, as Run does, and computes lineages.
func NewCorpus(ss []trace.Session) *Corpus {
	// Copied calls tie a resumed session to its original; compute that
	// before the copies are dropped.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != "" && parent[x] != x {
			x = parent[x]
		}
		return x
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra == "" {
			ra = a
		}
		if rb == "" {
			rb = b
		}
		if ra != rb {
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}
	firstByCall := map[string]string{}
	firstByText := map[string]string{}
	for _, s := range ss {
		k := s.Client + "/" + s.ID
		if parent[k] == "" {
			parent[k] = k
		}
		for _, c := range s.Calls {
			if c.ID == "" {
				continue
			}
			ck := s.Client + "\x00" + c.ID
			if o, ok := firstByCall[ck]; ok {
				union(o, k)
			} else {
				firstByCall[ck] = k
			}
		}
		// Only the task-defining message (the session's first substantive
		// request) ties sessions: a templated or re-sent task. Follow-ups
		// such as "do you still have more to do?" are typed into unrelated
		// sessions and must not chain them, nor must injected wrappers
		// ("# AGENTS.md instructions ...", "<...>", "[Request interrupted").
		for _, t := range s.Requests {
			t = strings.Join(strings.Fields(strings.ToLower(t)), " ")
			if t == "" || strings.ContainsAny(t[:1], "#<[") {
				continue
			}
			if len(t) >= 40 {
				if o, ok := firstByText[t]; ok {
					union(o, k)
				} else {
					firstByText[t] = k
				}
			}
			break
		}
	}
	cp := make([]trace.Session, len(ss))
	copy(cp, ss)
	for i := range cp {
		cp[i].Calls = append([]trace.Call(nil), cp[i].Calls...)
	}
	trace.DropCopiedCalls(cp)
	c := &Corpus{Sessions: cp, ByKey: map[string]int{}, Lineage: map[string]string{}}
	for i, s := range cp {
		k := s.Client + "/" + s.ID
		c.ByKey[k] = i
		h := sha256.Sum256([]byte(find(k)))
		c.Lineage[k] = "ln_" + hex.EncodeToString(h[:5])
	}
	return c
}

// Session returns the session a key names.
func (c *Corpus) Session(client, id string) (trace.Session, bool) {
	i, ok := c.ByKey[client+"/"+id]
	if !ok {
		return trace.Session{}, false
	}
	return c.Sessions[i], true
}

// Episodes lists every request that has at least one call, in corpus order.
func (c *Corpus) Episodes() []Episode {
	var out []Episode
	for _, s := range c.Sessions {
		n := map[int]int{}
		first := map[int]time.Time{}
		for _, cl := range s.Calls {
			if n[cl.Request] == 0 {
				first[cl.Request] = cl.Time
			}
			n[cl.Request]++
		}
		reqs := make([]int, 0, len(n))
		for r := range n {
			reqs = append(reqs, r)
		}
		sort.Ints(reqs)
		for _, r := range reqs {
			out = append(out, Episode{ID: trace.EpisodeID(s.Client, s.ID, r), Client: s.Client, Session: s.ID, Request: r,
				Calls: n[r], Start: first[r], Lineage: c.Lineage[s.Client+"/"+s.ID]})
		}
	}
	return out
}

// SampleOptions sets the sample's strata sizes.
type SampleOptions struct {
	Seed int64
	// PerReady episodes from each ready primitive; PerGroup routines from
	// needs_authoring and from each removal check; one episode each.
	PerReady, PerGroup int
	// Uncovered episodes that produced no routine, split evenly by client.
	Uncovered int
}

// SampleEpisodes draws the evaluation sample: episodes behind every ready
// primitive, routines from needs_authoring and from each removal check,
// merged routines, and episodes that produced no routine. At most one
// episode per session is drawn, so one session cannot fill a stratum.
func SampleEpisodes(c *Corpus, rep *model.Report, o SampleOptions) []Episode {
	rng := rand.New(rand.NewSource(o.Seed))
	all := c.Episodes()
	byKey := map[EpisodeKey]Episode{}
	for _, e := range all {
		byKey[EpisodeKey{e.Client, e.Session, e.Request}] = e
	}
	usedSession := map[string]bool{}
	var out []Episode
	take := func(e Episode, stratum, routine string) bool {
		k := e.Client + "/" + e.Session
		if usedSession[k] {
			return false
		}
		usedSession[k] = true
		e.Stratum, e.Routine = stratum, routine
		out = append(out, e)
		return true
	}
	// pick draws n episodes from a routine's sources, preferring the ones
	// that ran its steps.
	pick := func(r model.Routine, n int, stratum string) {
		srcs := append([]model.SourceRef(nil), r.Sources...)
		rng.Shuffle(len(srcs), func(i, j int) { srcs[i], srcs[j] = srcs[j], srcs[i] })
		sort.SliceStable(srcs, func(i, j int) bool { return srcs[i].Ran && !srcs[j].Ran })
		got := 0
		for _, s := range srcs {
			if got == n {
				break
			}
			if e, ok := byKey[EpisodeKey{s.Client, s.Session, s.Request}]; ok && take(e, stratum, r.ID) {
				got++
			}
		}
	}
	groups := map[string][]model.Routine{}
	var order []string
	for _, r := range rep.Routines {
		var g string
		switch {
		case r.MergedInto != "":
			g = "merged"
		case r.Decision == "removed":
			g = "removed:" + r.Failed
		default:
			g = r.Decision
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], r)
	}
	sort.Strings(order)
	for _, g := range order {
		rs := groups[g]
		switch g {
		case "primitive":
			for _, r := range rs {
				pick(r, o.PerReady, g)
			}
		default:
			idx := rng.Perm(len(rs))
			if g != "merged" && len(idx) > o.PerGroup {
				idx = idx[:o.PerGroup]
			}
			sort.Ints(idx)
			for _, i := range idx {
				pick(rs[i], 1, g)
			}
		}
	}
	// Episodes behind no routine at all.
	covered := map[EpisodeKey]bool{}
	for _, r := range rep.Routines {
		for _, s := range r.Sources {
			covered[EpisodeKey{s.Client, s.Session, s.Request}] = true
		}
	}
	byClient := map[string][]Episode{}
	var clients []string
	for _, e := range all {
		if covered[EpisodeKey{e.Client, e.Session, e.Request}] {
			continue
		}
		if _, ok := byClient[e.Client]; !ok {
			clients = append(clients, e.Client)
		}
		byClient[e.Client] = append(byClient[e.Client], e)
	}
	sort.Strings(clients)
	for ci, cl := range clients {
		want := o.Uncovered / len(clients)
		if ci < o.Uncovered%len(clients) {
			want++
		}
		es := byClient[cl]
		got := 0
		for _, i := range rng.Perm(len(es)) {
			if got == want {
				break
			}
			if take(es[i], "no_routine", "") {
				got++
			}
		}
	}
	return out
}

// RenderEpisode writes what a labeler sees: the request, the message before
// it, and each call with its arguments and the start of its result. It
// carries no program decision. Credentials are redacted; the text is
// otherwise the user's own and must stay on this machine.
func (c *Corpus) RenderEpisode(e Episode) string {
	s, ok := c.Session(e.Client, e.Session)
	if !ok {
		return "(session not in corpus)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Episode %s\n\nclient: %s\nstarted: %s\n\n", e.ID, e.Client, e.Start.UTC().Format(time.RFC3339))
	if e.Request > 0 && e.Request-1 < len(s.Requests) {
		fmt.Fprintf(&b, "## Previous message (context only)\n\n%s\n\n", Indent(trace.TruncateUTF8(redact.Redact(s.Requests[e.Request-1]), 600)))
	}
	req := ""
	if e.Request < len(s.Requests) {
		req = s.Requests[e.Request]
	}
	if req == "" {
		req = "(no user message before these calls)"
	}
	fmt.Fprintf(&b, "## Request\n\n%s\n\n", Indent(trace.TruncateUTF8(redact.Redact(req), 3000)))
	if e.Request+1 < len(s.Requests) {
		fmt.Fprintf(&b, "## Next message (context only)\n\n%s\n\n", Indent(trace.TruncateUTF8(redact.Redact(s.Requests[e.Request+1]), 400)))
	}
	var calls []trace.Call
	for _, cl := range s.Calls {
		if cl.Request == e.Request {
			calls = append(calls, cl)
		}
	}
	fmt.Fprintf(&b, "## Calls (%d)\n\n", len(calls))
	for i, cl := range calls {
		if len(calls) > 60 && i == 40 {
			fmt.Fprintf(&b, "... %d calls omitted ...\n\n", len(calls)-60)
		}
		if len(calls) > 60 && i >= 40 && i < len(calls)-20 {
			continue
		}
		what := cl.Command
		if cl.Tool != "shell" {
			keys := make([]string, 0, len(cl.Args))
			for k := range cl.Args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				parts = append(parts, k+"="+trace.TruncateUTF8(cl.Args[k], 300))
			}
			what = strings.Join(parts, " ")
		}
		outcome := map[trace.Outcome]string{trace.OutcomeUnknown: "unknown", trace.OutcomeOK: "ok", trace.OutcomeFailed: "failed"}[cl.Outcome]
		fmt.Fprintf(&b, "%d. [%s] (outcome: %s)\n%s\n", i+1, cl.Tool, outcome, Indent(trace.TruncateUTF8(redact.Redact(what), 700)))
		if cl.Output != "" {
			fmt.Fprintf(&b, "   result: %s\n", strings.ReplaceAll(trace.TruncateUTF8(redact.Redact(cl.Output), 300), "\n", " ⏎ "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func Indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

// HoldoutOptions sets a lineage-separated holdout draw (plan v3 section 0).
type HoldoutOptions struct {
	Seed int64
	N    int
	// Exclude names episodes of earlier samples. Their sessions, every
	// session sharing their lineage, and every session whose first request
	// has the same text up to digits (a templated prompt whose run stamp
	// changes) are left out of the draw.
	Exclude []EpisodeKey
}

// TemplateKey is a session's first substantive request with digits and
// spacing normalized, so a scheduled prompt that differs only in its run
// stamp is one template.
func TemplateKey(s trace.Session) string {
	for _, t := range s.Requests {
		t = strings.Join(strings.Fields(strings.ToLower(t)), " ")
		if t == "" || strings.ContainsAny(t[:1], "#<[") {
			continue
		}
		var b strings.Builder
		for _, r := range t {
			if r >= '0' && r <= '9' {
				r = '0'
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	return ""
}

// SampleHoldout draws n episodes uniformly from the sessions no excluded
// episode's lineage or template reaches, at most one per session. It never
// consults any program decision, so recall on it measures what the
// program misses among ordinary history.
func SampleHoldout(c *Corpus, o HoldoutOptions) []Episode {
	exLineage := map[string]bool{}
	exTemplate := map[string]bool{}
	for _, k := range o.Exclude {
		exLineage[c.Lineage[k.Client+"/"+k.Session]] = true
		if s, ok := c.Session(k.Client, k.Session); ok {
			if t := TemplateKey(s); t != "" {
				exTemplate[t] = true
			}
		}
	}
	var pool []Episode
	for _, e := range c.Episodes() {
		s, _ := c.Session(e.Client, e.Session)
		if exLineage[e.Lineage] || exTemplate[TemplateKey(s)] {
			continue
		}
		pool = append(pool, e)
	}
	rng := rand.New(rand.NewSource(o.Seed))
	used := map[string]bool{}
	var out []Episode
	for _, i := range rng.Perm(len(pool)) {
		if len(out) == o.N {
			break
		}
		e := pool[i]
		k := e.Client + "/" + e.Session
		if used[k] {
			continue
		}
		used[k] = true
		e.Stratum = "holdout_uniform"
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
