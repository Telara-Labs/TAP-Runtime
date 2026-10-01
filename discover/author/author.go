package author

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Selection kinds.
const (
	SelectedTask        = "selected_task"
	DiscoverCandidate   = "discover_candidate"
	DiscoverOpportunity = "discover_opportunity"
	DiscoverSpan        = "discover_span"
	DiscoverLogic       = "discover_logic"
)

// Brief is what the agent authoring a package starts from.
type Brief struct {
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Selection string `json:"selection"`
	// Ref is an opaque name for the source request, safe to copy into a
	// package's provenance; Source is the private location it stands for.
	Ref       string          `json:"ref"`
	Source    model.SourceRef `json:"source"`
	Candidate *BriefCandidate `json:"candidate,omitempty"`
	// Opportunity is the selection pass's claim, when the task came from it.
	Opportunity   *model.Opportunity    `json:"opportunity,omitempty"`
	Span          *model.SpanProposal   `json:"span,omitempty"`
	Logic         *model.LogicCandidate `json:"logic,omitempty"`
	LogicExamples []LogicExample        `json:"logic_examples,omitempty"`
	Evidence      BriefEvidence         `json:"evidence"`
	// Missing lists the contract fields nothing has established yet. Every
	// field is missing for a selected task.
	Missing  []string      `json:"missing"`
	Contract BriefContract `json:"contract"`
}

// LogicExample is one independently observed execution of the same shape.
// The agent uses differing values to infer parameters, not to bake them in.
type LogicExample struct {
	SpanID   string          `json:"span_id"`
	Ref      string          `json:"ref"`
	Source   model.SourceRef `json:"source"`
	Evidence BriefEvidence   `json:"evidence"`
}

// BriefCandidate is what Discover said about a candidate, carried as its
// claim, not as the contract.
type BriefCandidate struct {
	Routine      string            `json:"routine"`
	ReportDigest string            `json:"report_digest"`
	Decision     string            `json:"decision"`
	Failed       string            `json:"failed,omitempty"`
	Why          string            `json:"why,omitempty"`
	SourceRole   string            `json:"source_role"`
	Suitability  string            `json:"suitability"`
	Contract     model.Contract    `json:"contract"`
	Sources      []model.SourceRef `json:"sources"`
}

// BriefEvidence is the source request and the calls it made.
type BriefEvidence struct {
	Request string `json:"request"`
	// PreviousRequest may contain the input/goal of an elliptical follow-up.
	// It is context for the author, not proven to be the same task.
	PreviousRequest string      `json:"previous_request,omitempty"`
	Steps           []BriefStep `json:"steps"`
	// Requests is how many requests the session holds, so the agent knows
	// whether the task continued in a later one.
	Requests int `json:"requests"`
}

type BriefStep struct {
	N          int               `json:"n"`
	SourceCall int               `json:"source_call,omitempty"`
	Tool       string            `json:"tool"`
	Command    string            `json:"command,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	Outcome    string            `json:"outcome"`
	Output     string            `json:"output,omitempty"`
}

// BriefField is one contract field: its value, and what established it. An
// empty EstablishedBy means nothing has.
type BriefField struct {
	Value         any    `json:"value"`
	EstablishedBy string `json:"established_by"`
}

type BriefContract struct {
	Goal      BriefField `json:"goal"`
	Inputs    BriefField `json:"inputs"`
	Scope     BriefField `json:"scope"`
	Procedure BriefField `json:"procedure"`
	Output    BriefField `json:"output"`
	Oracle    BriefField `json:"oracle"`
	Failures  BriefField `json:"failures"`
	Boundary  BriefField `json:"boundary"`
}

// OpaqueRef names a source request without revealing it.
func OpaqueRef(s model.SourceRef) string {
	sum := sha256.Sum256([]byte(s.Client + "/" + s.Session + "/" + strconv.Itoa(s.Request)))
	return "src_" + hex.EncodeToString(sum[:6])
}

// NewBrief builds the brief for request req of session s. cand is the
// Discover candidate the request came from, or nil for a selected task.
func NewBrief(s trace.Session, req int, cand *BriefCandidate) (*Brief, error) {
	if req < 0 || req >= len(s.Requests) {
		return nil, fmt.Errorf("session %s has %d requests; request %d does not exist", s.ID, len(s.Requests), req)
	}
	src := model.SourceRef{Client: s.Client, Session: s.ID, Request: req}
	b := &Brief{
		Kind: "tap.authoring-brief/v1", Status: model.BriefStatus, Selection: SelectedTask,
		Ref: OpaqueRef(src), Source: src, Candidate: cand,
		Evidence: BriefEvidence{Request: redact.Redact(s.Requests[req]), Requests: len(s.Requests)},
	}
	for _, c := range s.Calls {
		if c.Request != req {
			continue
		}
		st := BriefStep{N: len(b.Evidence.Steps) + 1, SourceCall: len(b.Evidence.Steps) + 1, Tool: c.Tool, Command: redact.Redact(c.Command), Outcome: OutcomeName(c.Outcome), Output: redact.Redact(c.Output)}
		if len(c.Args) > 0 {
			st.Args = map[string]string{}
			for k, v := range c.Args {
				st.Args[k] = redact.Redact(v)
			}
		}
		b.Evidence.Steps = append(b.Evidence.Steps, st)
	}
	if cand != nil {
		b.Selection = DiscoverCandidate
		// What Discover inferred mechanically is a proposal the agent checks;
		// it is recorded as such, never as established.
		by := "discover " + cand.Routine + " (mechanical proposal, unverified)"
		if cand.Contract.Goal != "" && cand.Contract.Goal != "unknown" {
			b.Contract.Goal = BriefField{Value: cand.Contract.Goal, EstablishedBy: by}
		}
		if len(cand.Contract.Inputs) > 0 {
			b.Contract.Inputs = BriefField{Value: cand.Contract.Inputs, EstablishedBy: by}
		}
		if len(cand.Contract.Scope) > 0 {
			b.Contract.Scope = BriefField{Value: cand.Contract.Scope, EstablishedBy: by}
		}
		if cand.Contract.Output != "" && cand.Contract.Output != "unknown" {
			b.Contract.Output = BriefField{Value: cand.Contract.Output, EstablishedBy: by}
		}
		if cand.Contract.Boundary != "" {
			b.Contract.Boundary = BriefField{Value: cand.Contract.Boundary, EstablishedBy: by}
		}
	}
	b.Missing = b.Contract.Missing()
	return b, nil
}

// NewBriefSpan narrows the evidence to the exact recorded calls of a span.
// A stale or ambiguous source is an error rather than a silently wrong brief.
func NewBriefSpan(s trace.Session, p model.SpanProposal) (*Brief, error) {
	if s.Client != p.Client || s.ID != p.Session || p.Request < 0 || p.Request >= len(s.Requests) {
		return nil, fmt.Errorf("span %s source does not match session", p.ID)
	}
	if len(p.CallHashes) == 0 || len(p.CallHashes) != len(p.Calls) {
		return nil, fmt.Errorf("span %s has no verifiable call identities", p.ID)
	}
	b, err := NewBrief(s, p.Request, nil)
	if err != nil {
		return nil, err
	}
	byHash := map[string][]int{}
	ordinal := 0
	for _, c := range s.Calls {
		if c.Request != p.Request {
			continue
		}
		ordinal++
		h := retrieval.SpanCallHash(c)
		byHash[h] = append(byHash[h], ordinal)
	}
	want := map[int]bool{}
	for _, h := range p.CallHashes {
		if len(byHash[h]) != 1 {
			return nil, fmt.Errorf("span %s call evidence is stale or ambiguous", p.ID)
		}
		want[byHash[h][0]] = true
	}
	var selected []BriefStep
	for _, step := range b.Evidence.Steps {
		if !want[step.SourceCall] {
			continue
		}
		step.N = len(selected) + 1
		selected = append(selected, step)
	}
	if len(selected) != len(p.Calls) {
		return nil, fmt.Errorf("span %s selected call count changed", p.ID)
	}
	b.Evidence.Steps = selected
	if p.ContextRequest > 0 && p.ContextRequest <= len(s.Requests) {
		b.Evidence.PreviousRequest = redact.Redact(s.Requests[p.ContextRequest-1])
	}
	b.Selection, b.Span = DiscoverSpan, &p
	return b, nil
}

func (c BriefContract) Missing() []string {
	var out []string
	for _, f := range []struct {
		name string
		f    BriefField
	}{{"goal", c.Goal}, {"inputs", c.Inputs}, {"scope", c.Scope}, {"procedure", c.Procedure},
		{"output", c.Output}, {"oracle", c.Oracle}, {"failures", c.Failures}, {"boundary", c.Boundary}} {
		if f.f.EstablishedBy == "" {
			out = append(out, f.name)
		}
	}
	return out
}

func OutcomeName(o trace.Outcome) string {
	switch o {
	case trace.OutcomeOK:
		return "ok"
	case trace.OutcomeFailed:
		return "failed"
	}
	return "unknown"
}

// CandidateFrom finds routine id in a report file written by
// `tap discover --out`. Any routine can be briefed, whatever Discover
// decided about it: a rejected candidate is still one the user may select.
func CandidateFrom(reportPath, id string) (*BriefCandidate, error) {
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, err
	}
	var rep struct {
		Routines []model.Routine `json:"routines"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("%s is not a discover report: %w", reportPath, err)
	}
	sum := sha256.Sum256(raw)
	for _, r := range rep.Routines {
		if r.ID != id {
			continue
		}
		if len(r.Sources) == 0 {
			return nil, fmt.Errorf("routine %s lists no source requests to brief from", id)
		}
		return &BriefCandidate{Routine: r.ID, ReportDigest: "sha256:" + hex.EncodeToString(sum[:]),
			Decision: r.Decision, Failed: r.Failed, Why: r.Why, SourceRole: r.SourceRole,
			Suitability: r.Suitability, Contract: r.Contract, Sources: r.Sources}, nil
	}
	return nil, fmt.Errorf("routine %s is not in %s", id, reportPath)
}

// FindSession reads one session of a client's history under home.
func FindSession(client, id, home string) (trace.Session, error) {
	var files []string
	switch client {
	case "claude-code":
		files, _ = filepath.Glob(filepath.Join(home, ".claude", "projects", "*", id+".jsonl"))
	case "codex":
		_ = filepath.WalkDir(filepath.Join(home, ".codex", "sessions"), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") && strings.Contains(filepath.Base(p), id) {
				files = append(files, p)
			}
			return nil
		})
	case "cursor":
		ss, err := history.Cursor{DB: history.CursorStateDB(home)}.Read(time.Time{})
		if err != nil {
			return trace.Session{}, err
		}
		for _, s := range ss {
			if s.ID == id {
				return s, nil
			}
		}
	default:
		return trace.Session{}, fmt.Errorf("unknown client %q (want claude-code, codex or cursor)", client)
	}
	sort.Strings(files)
	for _, f := range files {
		var s trace.Session
		var err error
		if client == "claude-code" {
			s, err = history.ReadClaudeFile(f)
		} else {
			s, err = history.ReadCodexFile(f)
		}
		if err == nil && (s.ID == id || strings.Contains(filepath.Base(f), id)) {
			return s, nil
		}
	}
	return trace.Session{}, fmt.Errorf("no %s session %s under %s", client, id, home)
}

// ParseTaskRef reads client/session/request.
func ParseTaskRef(ref string) (model.SourceRef, error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 3 {
		return model.SourceRef{}, fmt.Errorf("task %q: want client/session/request", ref)
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 0 {
		return model.SourceRef{}, fmt.Errorf("task %q: request must be a number", ref)
	}
	return model.SourceRef{Client: parts[0], Session: parts[1], Request: n}, nil
}

func BriefCommand(args []string, home string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover brief", flag.ContinueOnError)
	fs.SetOutput(errOut)
	task := fs.String("task", "", "the selected task, as client/session/request")
	candidate := fs.String("candidate", "", "a routine id from a discover report")
	opportunity := fs.String("opportunity", "", "an opportunity id from a discover report")
	span := fs.String("span", "", "a bounded-span proposal id from a discover report")
	logic := fs.String("logic", "", "a recurring execution-logic candidate id from a discover report")
	report := fs.String("report", "", "with --candidate: the report written by `tap discover --out`")
	source := fs.Int("source", 0, "with --candidate: which of its source requests to brief from")
	dir := fs.String("out", "", "directory to write brief.json and BRIEF.md into (private)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	given := 0
	for _, v := range []string{*task, *candidate, *opportunity, *span, *logic} {
		if v != "" {
			given++
		}
	}
	if *dir == "" || given != 1 || (*task == "" && *report == "") {
		fmt.Fprintln(errOut, "discover brief: give --out and exactly one of --task, --candidate, --opportunity, --span or --logic with --report")
		return 2
	}
	var cand *BriefCandidate
	var opp *model.Opportunity
	var spanProposal *model.SpanProposal
	var logicCandidate *model.LogicCandidate
	var logicMembers []model.SpanProposal
	var ref model.SourceRef
	var err error
	if *logic != "" {
		if logicCandidate, logicMembers, err = LogicFrom(*report, *logic); err != nil {
			fmt.Fprintln(errOut, "discover brief:", err)
			return 1
		}
		for i := range logicMembers {
			if logicMembers[i].ID == logicCandidate.Example.ID {
				spanProposal = &logicMembers[i]
				break
			}
		}
		if spanProposal == nil {
			fmt.Fprintln(errOut, "discover brief: candidate example is absent from report")
			return 1
		}
		ref = model.SourceRef{Client: spanProposal.Client, Session: spanProposal.Session, Request: spanProposal.Request}
	} else if *span != "" {
		if spanProposal, err = SpanFrom(*report, *span); err != nil {
			fmt.Fprintln(errOut, "discover brief:", err)
			return 1
		}
		ref = model.SourceRef{Client: spanProposal.Client, Session: spanProposal.Session, Request: spanProposal.Request}
	} else if *opportunity != "" {
		if opp, err = OpportunityFrom(*report, *opportunity); err != nil {
			fmt.Fprintln(errOut, "discover brief:", err)
			return 1
		}
		ref = model.SourceRef{Client: opp.Client, Session: opp.Session, Request: opp.Request}
	} else if *candidate != "" {
		if cand, err = CandidateFrom(*report, *candidate); err != nil {
			fmt.Fprintln(errOut, "discover brief:", err)
			return 1
		}
		if *source < 0 || *source >= len(cand.Sources) {
			fmt.Fprintf(errOut, "discover brief: routine %s has %d sources\n", cand.Routine, len(cand.Sources))
			return 2
		}
		ref = cand.Sources[*source]
	} else if ref, err = ParseTaskRef(*task); err != nil {
		fmt.Fprintln(errOut, "discover brief:", err)
		return 2
	}
	s, err := FindSession(ref.Client, ref.Session, home)
	if err != nil {
		fmt.Fprintln(errOut, "discover brief:", err)
		return 1
	}
	var b *Brief
	if spanProposal != nil {
		b, err = NewBriefSpan(s, *spanProposal)
	} else {
		b, err = NewBrief(s, ref.Request, cand)
	}
	if err != nil {
		fmt.Fprintln(errOut, "discover brief:", err)
		return 1
	}
	if opp != nil {
		// The route and reasons are the selection pass's evidence, carried
		// as its claim; every contract field stays for the agent to establish.
		b.Selection, b.Opportunity = DiscoverOpportunity, opp
	}
	if logicCandidate != nil {
		b.Selection, b.Logic = DiscoverLogic, logicCandidate
		seenSessions := map[string]bool{}
		for _, member := range append([]model.SpanProposal{*spanProposal}, logicMembers...) {
			key := member.Client + "/" + member.Session
			if seenSessions[key] || len(b.LogicExamples) >= 3 {
				continue
			}
			seenSessions[key] = true
			var example *Brief
			if member.ID == spanProposal.ID {
				example = b
			} else {
				es, findErr := FindSession(member.Client, member.Session, home)
				if findErr != nil {
					fmt.Fprintln(errOut, "discover brief:", findErr)
					return 1
				}
				example, findErr = NewBriefSpan(es, member)
				if findErr != nil {
					fmt.Fprintln(errOut, "discover brief:", findErr)
					return 1
				}
			}
			src := model.SourceRef{Client: member.Client, Session: member.Session, Request: member.Request}
			b.LogicExamples = append(b.LogicExamples, LogicExample{SpanID: member.ID, Ref: OpaqueRef(src), Source: src, Evidence: example.Evidence})
		}
	}
	digest, err := b.Write(*dir)
	if err != nil {
		fmt.Fprintln(errOut, "discover brief:", err)
		return 1
	}
	fmt.Fprintf(out, "brief %s (%s, %s) written to %s\n%s\n", b.Ref, b.Selection, b.Status, *dir, digest)
	return 0
}

// Write puts brief.json and BRIEF.md into dir and returns brief.json's
// digest, which the package's AUTHORING.json names.
func (b *Brief) Write(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, "brief.json"), raw, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "BRIEF.md"), []byte(b.Markdown()), 0o600); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (b *Brief) Markdown() string {
	var w strings.Builder
	fmt.Fprintf(&w, "# Authoring brief %s\n\n", b.Ref)
	w.WriteString("Private: this file holds text from your session history. Do not commit or share it.\n\n")
	fmt.Fprintf(&w, "Status: **%s**. Nothing has established that this is a useful procedure. You establish it, or reject it, from the task's context.\n\n", b.Status)
	fmt.Fprintf(&w, "Selection: %s. Source: %s session %s, request %d (of %d).\n\n", b.Selection, b.Source.Client, b.Source.Session, b.Source.Request, b.Evidence.Requests)
	if c := b.Candidate; c != nil {
		fmt.Fprintf(&w, "## What Discover said (a proposal, not a finding)\n\nRoutine %s, report %s: decision %s", c.Routine, c.ReportDigest, c.Decision)
		if c.Failed != "" {
			fmt.Fprintf(&w, " (%s: %s)", c.Failed, c.Why)
		}
		fmt.Fprintf(&w, "; role %s; suitability %s; %d source requests.\n\n", c.SourceRole, c.Suitability, len(c.Sources))
	}
	if o := b.Opportunity; o != nil {
		fmt.Fprintf(&w, "## What Discover's selection pass said (a proposal, not a finding)\n\nOpportunity %s, route %s: %s.\n\n", o.ID, o.Route, strings.Join(o.Reasons, "; "))
	}
	if p := b.Span; p != nil {
		fmt.Fprintf(&w, "## Bounded-span proposal (not a useful-procedure finding)\n\nSpan %s, %s; source call numbers %v; input provenance %v. The selected calls are only a proposed part of the task.\n\n", p.ID, p.Kind, p.Calls, p.Inputs)
	}
	if c := b.Logic; c != nil {
		fmt.Fprintf(&w, "## Recurring execution logic (authoring lead)\n\nCandidate %s; actions %s; result edges %s; possible parameter roles %s. Observed in %d independent executions across %d sessions. Concrete argument and output values are examples, not fixed contract values.\n\n",
			c.ID, strings.Join(c.Actions, " → "), strings.Join(c.Edges, "; "), strings.Join(c.Parameters, ", "), c.Executions, c.Sessions)
		for i, ex := range b.LogicExamples {
			fmt.Fprintf(&w, "### Execution example %d — %s (ref %s)\n\n> %s\n\n", i+1, ex.SpanID, ex.Ref, strings.ReplaceAll(ex.Evidence.Request, "\n", "\n> "))
			for _, step := range ex.Evidence.Steps {
				fmt.Fprintf(&w, "- `%s` %s %v → %s\n", step.Tool, trace.TruncateUTF8(strings.ReplaceAll(step.Command, "\n", " "), 160), step.Args, step.Outcome)
			}
			w.WriteString("\n")
		}
	}
	fmt.Fprintf(&w, "## Contract fields not yet established\n\n%s\n\n", strings.Join(b.Missing, ", "))
	if b.Evidence.PreviousRequest != "" {
		fmt.Fprintf(&w, "## Earlier request context (may be a different task)\n\n> %s\n\n", strings.ReplaceAll(b.Evidence.PreviousRequest, "\n", "\n> "))
	}
	w.WriteString("## The request\n\n")
	for _, line := range strings.Split(strings.TrimSpace(b.Evidence.Request), "\n") {
		w.WriteString("> " + line + "\n")
	}
	fmt.Fprintf(&w, "\n## The calls it made (%d)\n\n", len(b.Evidence.Steps))
	for _, st := range b.Evidence.Steps {
		what := st.Command
		if what == "" {
			keys := make([]string, 0, len(st.Args))
			for k := range st.Args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				what += k + "=" + trace.TruncateUTF8(st.Args[k], 120) + " "
			}
		}
		fmt.Fprintf(&w, "%d. `%s` %s [%s; source call %d]\n", st.N, st.Tool, strings.TrimSpace(trace.TruncateUTF8(strings.ReplaceAll(what, "\n", " "), 300)), st.Outcome, st.SourceCall)
	}
	w.WriteString(`
## What to do

1. Read the source session if the steps above do not settle a field. The ref above names it.
   Establish each contract field (goal, inputs with their types and where each value comes from,
   scope, procedure, output, oracle, failure behaviour, and the point where the procedure hands
   back to you). Record what established each one. A word in the request, or an output label,
	   is not enough. Author the repeatable chunk; the surrounding user task need not
	   be automated. Turn changing values into runtime arguments or values derived
	   by earlier steps, rather than copying an example's concrete values.
2. Write the package as docs/writing-a-primitive.md describes: primitive.yaml declaring every
   command, file and tool it uses, and one program. No TODO: or REPLACE_ markers may remain.
   Check it with ` + "`tap manifest check <package>`" + `.
3. Put AUTHORING.json in the package:
   {"kind": "tap.authoring/v1", "name": ..., "publisher": ..., "author": "host-agent",
    "agent": <client and model>, "selection": <selected_task|discover_candidate|discover_opportunity|discover_span|discover_logic>,
    "candidate": <Discover id when selected by Discover>, "sources": [<opaque refs from examples>],
    "brief_digest": <printed by brief>, "contract": {<field>: {"value", "established_by"}},
    "interface": {"args": [...], "output": ..., "exit": {...}}}
4. Before running the package, write cases.json (at least: normal, empty, a changed count,
   a missing prerequisite, a malformed or ambiguous input) and an oracle derived from the
   contract, not from the package. ` + "`tap discover validate --cases cases.json --freeze`" + ` records the
   oracle's expected results.
5. ` + "`tap discover validate <package> --cases cases.json --out receipts.json`" + ` runs the package
   through the real runner on fresh fixtures and checks each result and each effect.
6. ` + "`tap discover save <package> --receipts receipts.json`" + ` installs it privately. It is marked
   validated only for the exact digest the receipts passed.
`)
	return w.String()
}

// OpportunityFrom finds opportunity id in a report written by
// `tap discover --out`.
func OpportunityFrom(reportPath, id string) (*model.Opportunity, error) {
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, err
	}
	var rep struct {
		Opportunities []model.Opportunity `json:"opportunities"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("%s is not a discover report: %w", reportPath, err)
	}
	for i := range rep.Opportunities {
		if rep.Opportunities[i].ID == id {
			return &rep.Opportunities[i], nil
		}
	}
	return nil, fmt.Errorf("opportunity %s is not in %s", id, reportPath)
}

// SpanFrom finds a proposal in a local Discover report.
func SpanFrom(reportPath, id string) (*model.SpanProposal, error) {
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, err
	}
	var rep struct {
		SpanProposals []model.SpanProposal `json:"span_proposals"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("%s is not a discover report: %w", reportPath, err)
	}
	for i := range rep.SpanProposals {
		if rep.SpanProposals[i].ID == id {
			return &rep.SpanProposals[i], nil
		}
	}
	return nil, fmt.Errorf("span %s is not in %s", id, reportPath)
}

// LogicFrom resolves a recurring logic candidate and all of its recorded
// spans. Call hashes are rechecked when each example is turned into a brief.
func LogicFrom(reportPath, id string) (*model.LogicCandidate, []model.SpanProposal, error) {
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, nil, err
	}
	var rep struct {
		LogicCandidates []model.LogicCandidate `json:"logic_candidates"`
		SpanProposals   []model.SpanProposal   `json:"span_proposals"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, nil, fmt.Errorf("%s is not a discover report: %w", reportPath, err)
	}
	for i := range rep.LogicCandidates {
		if rep.LogicCandidates[i].ID != id {
			continue
		}
		wanted := map[string]bool{}
		for _, member := range rep.LogicCandidates[i].Members {
			wanted[member] = true
		}
		var spans []model.SpanProposal
		for _, p := range rep.SpanProposals {
			if wanted[p.ID] {
				spans = append(spans, p)
				delete(wanted, p.ID)
			}
		}
		if len(wanted) != 0 {
			return nil, nil, fmt.Errorf("logic candidate %s has missing source spans", id)
		}
		return &rep.LogicCandidates[i], spans, nil
	}
	return nil, nil, fmt.Errorf("logic candidate %s is not in %s", id, reportPath)
}
