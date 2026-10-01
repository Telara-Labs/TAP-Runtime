// Package author is the author path: Discover proposes, an agent writes the primitive,
// and the result is validated through the real runner before it is saved.
package author

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
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

// OriginAgentAuthored marks a package a host agent wrote from a brief.
const OriginAgentAuthored = "agent_authored"

// Authoring is a package's AUTHORING.json: who wrote it, from what, and
// what established each contract field.
type Authoring struct {
	Kind        string                `json:"kind"`
	Name        string                `json:"name"`
	Publisher   string                `json:"publisher"`
	Author      string                `json:"author"`
	Agent       string                `json:"agent"`
	Selection   string                `json:"selection"`
	Sources     []string              `json:"sources"`
	Candidate   string                `json:"candidate,omitempty"`
	BriefDigest string                `json:"brief_digest"`
	Contract    map[string]BriefField `json:"contract"`
	Interface   json.RawMessage       `json:"interface"`
}

// ContractFields are the fields an authored contract must establish.
var ContractFields = []string{"goal", "inputs", "scope", "procedure", "output", "oracle", "failures", "boundary"}

// ReadAuthoring reads and checks a package's AUTHORING.json. Every contract
// field must have a value and say what established it.
func ReadAuthoring(pkg string) (*Authoring, error) {
	raw, err := os.ReadFile(filepath.Join(pkg, "AUTHORING.json"))
	if err != nil {
		return nil, fmt.Errorf("an authored package needs AUTHORING.json: %w", err)
	}
	var a Authoring
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("AUTHORING.json: %w", err)
	}
	var problems []string
	if a.Kind != "tap.authoring/v1" {
		problems = append(problems, "kind is not tap.authoring/v1")
	}
	if a.Author != "host-agent" {
		problems = append(problems, `author is not "host-agent"`)
	}
	if !pack.SkillName.MatchString(a.Name) {
		problems = append(problems, fmt.Sprintf("name %q is not a usable folder name", a.Name))
	}
	if a.Selection != SelectedTask && a.Selection != DiscoverCandidate && a.Selection != DiscoverOpportunity && a.Selection != DiscoverSpan && a.Selection != DiscoverLogic {
		problems = append(problems, "selection is not a supported authoring brief selection")
	}
	if a.Selection != SelectedTask && a.Candidate == "" {
		problems = append(problems, "a package from discover must name its candidate, opportunity, span or logic group")
	}
	if len(a.Sources) == 0 || a.BriefDigest == "" {
		problems = append(problems, "sources and brief_digest are required")
	}
	for _, s := range a.Sources {
		if !strings.HasPrefix(s, "src_") {
			problems = append(problems, "sources must be opaque refs (src_...), not session locations")
			break
		}
	}
	for _, f := range ContractFields {
		v, ok := a.Contract[f]
		if !ok || v.Value == nil || strings.TrimSpace(v.EstablishedBy) == "" {
			problems = append(problems, "contract."+f+" needs a value and established_by")
		}
	}
	if len(a.Interface) == 0 {
		problems = append(problems, "interface is required")
	}
	if len(problems) > 0 {
		return nil, errors.New("AUTHORING.json: " + strings.Join(problems, "; "))
	}
	return &a, nil
}

// ValidationFor is the status receipts give the package with this digest:
// passed only when they are for this digest and every case passed.
func ValidationFor(rec *Receipts, digest string) (string, error) {
	if rec == nil {
		return model.ValidationNotRun, nil
	}
	if rec.PackageDigest != digest {
		return model.ValidationNotRun, fmt.Errorf("the receipts are for %s, not this package (%s)", rec.PackageDigest, digest)
	}
	if rec.AllPassed && len(rec.Cases) > 0 {
		return model.ValidationPassed, nil
	}
	return model.ValidationFailed, nil
}

// SavePackage installs an authored package directory into root.
func SavePackage(pkgDir, root string, rec *Receipts, receiptsDigest string) (path, validation string, unchanged bool, err error) {
	a, err := ReadAuthoring(pkgDir)
	if err != nil {
		return "", "", false, err
	}
	if marks, err := FindPlaceholders(pkgDir); err != nil {
		return "", "", false, err
	} else if len(marks) > 0 {
		return "", "", false, fmt.Errorf("the package is not finished: %s", strings.Join(marks, "; "))
	}
	pkg, digest, err := PackageDir(pkgDir)
	if err != nil {
		return "", "", false, err
	}
	validation, err = ValidationFor(rec, digest)
	if err != nil {
		return "", "", false, err
	}
	m := pack.Marker{Name: a.Publisher + "/" + a.Name, Digest: digest, Validation: validation,
		Origin: OriginAgentAuthored, Receipts: receiptsDigest}
	if rec != nil {
		m.Cases = len(rec.Cases)
	}
	path, unchanged, err = pack.Install(root, a.Name, pkg, m, AuthoredSkillMD(a, m, filepath.Join(root, a.Name)))
	return path, validation, unchanged, err
}

func AuthoredSkillMD(a *Authoring, m pack.Marker, dir string) string {
	desc := a.Name
	if g, ok := a.Contract["goal"].Value.(string); ok && g != "" {
		desc = g
	}
	descJSON, _ := json.Marshal(desc)
	status := "Validation: **" + m.Validation + "**"
	switch m.Validation {
	case model.ValidationPassed:
		status += fmt.Sprintf(" for digest %s, on %d fresh cases through the TAP runner (receipts %s).", m.Digest, m.Cases, m.Receipts)
	case model.ValidationFailed:
		status += fmt.Sprintf(": at least one of %d cases failed for digest %s. Do not rely on it.", m.Cases, m.Digest)
	default:
		status += ": it has not been run on held-out cases."
	}
	iface, _ := json.MarshalIndent(json.RawMessage(a.Interface), "", "  ")
	return fmt.Sprintf(`---
name: %s
description: %s
---

# %s

A primitive written by a host agent (%s) from a task you selected, not a
recommendation made by `+"`tap discover`"+`. Read AUTHORING.json for what
established each part of its contract, and the program before running it.

%s

Run it with the TAP runner's `+"`tap_run`"+` tool, giving this folder as the package:

    package: %s

Its interface:

    %s

If no `+"`tap_run`"+` tool is available, connect the runner: `+"`tap install --client <client>`"+`.
`, a.Name, descJSON, a.Name, a.Agent, status, dir, strings.ReplaceAll(string(iface), "\n", "\n    "))
}

// LoadReceipts reads receipts written by validate, with their digest.
func LoadReceipts(path string) (*Receipts, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var rec Receipts
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if rec.Kind != "tap.validation-receipts/v1" {
		return nil, "", fmt.Errorf("%s is not validation receipts", path)
	}
	sum := sha256.Sum256(raw)
	return &rec, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func SaveCommand(args []string, home string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover save", flag.ContinueOnError)
	fs.SetOutput(errOut)
	receipts := fs.String("receipts", "", "receipts from `tap discover validate` for this package")
	client := fs.String("client", "claude-code", "where it goes: claude-code or codex")
	project := fs.Bool("project", false, "save into this project's skills directory instead of your home")
	pos, flags := SplitPositional(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 1 {
		fmt.Fprintln(errOut, "discover save: give one package directory")
		return 2
	}
	var rec *Receipts
	var recSum string
	if *receipts != "" {
		var err error
		if rec, recSum, err = LoadReceipts(*receipts); err != nil {
			fmt.Fprintln(errOut, "discover save:", err)
			return 1
		}
	}
	cwd, _ := os.Getwd()
	root, err := pack.SkillsDir(*client, *project, home, cwd)
	if err != nil {
		fmt.Fprintln(errOut, "discover save:", err)
		return 2
	}
	path, validation, unchanged, err := SavePackage(pos[0], root, rec, recSum)
	if err != nil {
		fmt.Fprintln(errOut, "discover save:", err)
		return 1
	}
	note := ""
	if unchanged {
		note = " (already saved)"
	}
	fmt.Fprintf(out, "saved %s%s\nvalidation: %s\n", path, note, validation)
	return 0
}

// CaseFile is a frozen set of cases for one package.
type CaseFile struct {
	Package  string   `json:"package"`
	Contract string   `json:"contract"`
	Oracle   []string `json:"oracle"`
	Cases    []Case   `json:"cases"`
}

// Case is one fresh fixture, the arguments to run with, and, once frozen,
// the oracle's expected result.
type Case struct {
	ID      string      `json:"id"`
	Kind    string      `json:"kind"`
	Note    string      `json:"note,omitempty"`
	Setup   []SetupStep `json:"setup"`
	Args    []string    `json:"args"`
	Approve bool        `json:"approve,omitempty"`
	Expect  *Observed   `json:"expect,omitempty"`
}

// SetupStep builds a fixture: a file written, a directory made, or a git
// command run. Paths are relative to the working directory and may climb
// one level (to the fixture root) to place a file outside it.
type SetupStep struct {
	Write string   `json:"write,omitempty"`
	Text  string   `json:"text,omitempty"`
	Mkdir string   `json:"mkdir,omitempty"`
	Run   []string `json:"run,omitempty"`
}

// Observed is one program's result: its exit code and its stdout, parsed
// when it is JSON.
type Observed struct {
	Exit   int    `json:"exit"`
	Output any    `json:"output,omitempty"`
	Raw    string `json:"raw,omitempty"`
}

// Receipts are the record of one validation run.
type Receipts struct {
	Kind          string            `json:"kind"`
	Package       string            `json:"package"`
	PackageDigest string            `json:"package_digest"`
	Runner        string            `json:"runner"`
	RunnerFlags   []string          `json:"runner_flags,omitempty"`
	RunnerSHA256  string            `json:"runner_sha256"`
	CasesSHA256   string            `json:"cases_sha256"`
	OracleSHA256  map[string]string `json:"oracle_sha256"`
	Started       time.Time         `json:"started"`
	ManifestCheck Observed          `json:"manifest_check"`
	Cases         []CaseReceipt     `json:"cases"`
	AllPassed     bool              `json:"all_passed"`
}

type CaseReceipt struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Args     []string  `json:"args"`
	Expect   *Observed `json:"expect,omitempty"`
	Oracle   Observed  `json:"oracle"`
	Package  Observed  `json:"package"`
	HostLog  []string  `json:"host_log"`
	Actions  []any     `json:"actions"`
	Before   string    `json:"fixture_before"`
	After    string    `json:"fixture_after"`
	Effects  []string  `json:"effects,omitempty"`
	Failures []string  `json:"failures,omitempty"`
	Pass     bool      `json:"pass"`
	Elapsed  int64     `json:"package_ms"`
}

const CaseTimeout = 2 * time.Minute

// PackageDir packs a package directory the way Draft.Package packs a draft:
// a gzip tar with sorted entries and a fixed time, so the same bytes always
// have the same digest. Links and special files are refused.
func PackageDir(dir string) ([]byte, string, error) {
	files := map[string][]byte{}
	exe := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		name := filepath.ToSlash(rel)
		files[name] = b
		exe[name] = info.Mode()&0o111 != 0
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("%s holds no files", dir)
	}
	return pack.PackFiles(files, func(n string) bool { return exe[n] })
}

// Placeholders are markers of unfinished authoring. A package holding one is
// not validated at all.
var Placeholders = []string{"TODO:", "REPLACE_"}

// FindPlaceholders lists each file line holding a placeholder marker.
func FindPlaceholders(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range Placeholders {
				if strings.Contains(line, m) {
					out = append(out, fmt.Sprintf("%s line %d: %s", rel, i+1, m))
				}
			}
		}
		return nil
	})
	return out, err
}

// LoadCases reads a case file and returns it with its digest.
func LoadCases(path string) (*CaseFile, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var cf CaseFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cf); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if len(cf.Oracle) == 0 {
		return nil, "", fmt.Errorf("%s names no oracle", path)
	}
	seen := map[string]bool{}
	for _, c := range cf.Cases {
		if c.ID == "" || seen[c.ID] {
			return nil, "", fmt.Errorf("%s: case id %q is empty or repeated", path, c.ID)
		}
		seen[c.ID] = true
	}
	sum := sha256.Sum256(raw)
	return &cf, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Fixture is one case's fresh directory: root holds work, the directory the
// runner and the oracle start in.
type Fixture struct {
	Root, Work string `json:"-"`
}

func NewFixture(c Case) (*Fixture, error) {
	tmp, err := os.MkdirTemp("", "tap-validate-"+c.ID+"-")
	if err != nil {
		return nil, err
	}
	// The runner resolves links in every path it is asked about; so does
	// this, so both see the same names.
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return nil, err
	}
	f := &Fixture{Root: root, Work: filepath.Join(root, "work")}
	if err := os.Mkdir(f.Work, 0o755); err != nil {
		return nil, err
	}
	for i, st := range c.Setup {
		if err := f.Apply(st); err != nil {
			f.Remove()
			return nil, fmt.Errorf("case %s setup step %d: %w", c.ID, i+1, err)
		}
	}
	return f, nil
}

func (f *Fixture) Remove() { os.RemoveAll(f.Root) }

// inside resolves a setup path, which must stay within the fixture root.
func (f *Fixture) Inside(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative", p)
	}
	full := filepath.Clean(filepath.Join(f.Work, filepath.FromSlash(p)))
	if full != f.Root && !strings.HasPrefix(full, f.Root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the fixture", p)
	}
	return full, nil
}

func (f *Fixture) Apply(st SetupStep) error {
	n := 0
	for _, set := range []bool{st.Write != "", st.Mkdir != "", len(st.Run) > 0} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("a step is exactly one of write, mkdir or run")
	}
	switch {
	case st.Write != "":
		p, err := f.Inside(st.Write)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(st.Text), 0o644)
	case st.Mkdir != "":
		p, err := f.Inside(st.Mkdir)
		if err != nil {
			return err
		}
		return os.MkdirAll(p, 0o755)
	}
	// Setup runs git only: fixtures are files and repositories, and a case
	// file must not be a way to run anything else.
	if st.Run[0] != "git" {
		return fmt.Errorf("setup may run git only, not %q", st.Run[0])
	}
	cmd := exec.Command("git", st.Run[1:]...)
	cmd.Dir = f.Work
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(st.Run, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// snapshot is every file under the fixture root, by relative path, as
// sha256 and mode. Directories count too, so a created empty one shows.
func (f *Fixture) Snapshot() (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(f.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.Root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[rel+"/"] = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			out[rel] = "link:" + t
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			out[rel] = hex.EncodeToString(sum[:]) + " " + info.Mode().Perm().String()
		}
		return nil
	})
	return out, err
}

func SnapshotDigest(s map[string]string) string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\n", k, s[k])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func SnapshotDiff(a, b map[string]string) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			out = append(out, "removed "+k)
		} else if w != v {
			out = append(out, "changed "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, "created "+k)
		}
	}
	sort.Strings(out)
	return out
}

func Observe(exit int, stdout string) Observed {
	o := Observed{Exit: exit}
	t := strings.TrimSpace(stdout)
	var v any
	if t != "" && json.Unmarshal([]byte(t), &v) == nil {
		o.Output = v
	} else {
		o.Raw = stdout
	}
	return o
}

func SameResult(a, b Observed) bool {
	return a.Exit == b.Exit && reflect.DeepEqual(a.Output, b.Output) && a.Raw == b.Raw
}

func RunProgram(dir string, argv []string) (exit int, stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), CaseTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return ee.ExitCode(), so.String(), se.String(), nil
	}
	if ctx.Err() != nil {
		return -1, so.String(), se.String(), fmt.Errorf("timed out after %s", CaseTimeout)
	}
	return 0, so.String(), se.String(), err
}

// OracleArgv resolves the oracle's words: one naming a file beside the case
// file becomes its absolute path.
func OracleArgv(cf *CaseFile, casesDir string) ([]string, map[string]string) {
	// The oracle runs inside each fixture, so a path to it must not be
	// relative to where this command started.
	if abs, err := filepath.Abs(casesDir); err == nil {
		casesDir = abs
	}
	argv := append([]string(nil), cf.Oracle...)
	sums := map[string]string{}
	for i, w := range argv {
		p := filepath.Join(casesDir, w)
		if b, err := os.ReadFile(p); err == nil && !filepath.IsAbs(w) {
			argv[i] = p
			sum := sha256.Sum256(b)
			sums[w] = "sha256:" + hex.EncodeToString(sum[:])
		}
	}
	return argv, sums
}

// OracleRan tells an oracle that reported a result from one that never got
// to: an oracle always prints its result, so a failing exit with nothing on
// stdout is the harness failing (a missing file, a crash), not an expected
// failure of the procedure.
func OracleRan(exit int, stdout string) error {
	if exit != 0 && strings.TrimSpace(stdout) == "" {
		return fmt.Errorf("exited %d and printed nothing", exit)
	}
	return nil
}

// Freeze records the oracle's result on a fresh fixture as each case's
// expectation. A case already frozen is left as it is.
func Freeze(casesPath string) (int, error) {
	cf, _, err := LoadCases(casesPath)
	if err != nil {
		return 0, err
	}
	argv, _ := OracleArgv(cf, filepath.Dir(casesPath))
	n := 0
	for i := range cf.Cases {
		c := &cf.Cases[i]
		if c.Expect != nil {
			continue
		}
		f, err := NewFixture(*c)
		if err != nil {
			return n, err
		}
		exit, so, se, err := RunProgram(f.Work, append(append([]string(nil), argv...), c.Args...))
		f.Remove()
		if err == nil {
			err = OracleRan(exit, so)
		}
		if err != nil {
			return n, fmt.Errorf("case %s oracle: %w: %s", c.ID, err, strings.TrimSpace(se))
		}
		o := Observe(exit, so)
		c.Expect = &o
		n++
	}
	b, err := json.MarshalIndent(cf, "", " ")
	if err != nil {
		return n, err
	}
	return n, os.WriteFile(casesPath, append(b, '\n'), 0o644)
}

// ValidateOptions says what to validate and with which runner.
type ValidateOptions struct {
	Package string
	Cases   string
	Runner  string
	// RunnerFlags go to the runner before its own flags, such as an
	// interpreter store for a machine that must not download one.
	RunnerFlags []string
}

// Validate runs every case and returns the receipts. An error means the run
// could not be carried out; a failing case is a receipt, not an error.
func Validate(o ValidateOptions) (*Receipts, error) {
	pkg, err := filepath.Abs(o.Package)
	if err != nil {
		return nil, err
	}
	if marks, err := FindPlaceholders(pkg); err != nil {
		return nil, err
	} else if len(marks) > 0 {
		return nil, fmt.Errorf("the package is not finished: %s", strings.Join(marks, "; "))
	}
	_, digest, err := PackageDir(pkg)
	if err != nil {
		return nil, err
	}
	cf, casesSum, err := LoadCases(o.Cases)
	if err != nil {
		return nil, err
	}
	runnerBytes, err := os.ReadFile(o.Runner)
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	rs := sha256.Sum256(runnerBytes)
	argv, oracleSums := OracleArgv(cf, filepath.Dir(o.Cases))
	rec := &Receipts{Kind: "tap.validation-receipts/v1", Package: pkg, PackageDigest: digest, Runner: o.Runner, RunnerFlags: o.RunnerFlags,
		RunnerSHA256: "sha256:" + hex.EncodeToString(rs[:]), CasesSHA256: casesSum, OracleSHA256: oracleSums,
		Started: time.Now().UTC(), AllPassed: len(cf.Cases) > 0}

	exit, so, se, err := RunProgram(pkg, []string{o.Runner, "manifest", "check", pkg})
	if err != nil {
		return nil, fmt.Errorf("manifest check: %w", err)
	}
	rec.ManifestCheck = Observed{Exit: exit, Raw: strings.TrimSpace(so + se)}
	if exit != 0 {
		rec.AllPassed = false
	}
	for _, c := range cf.Cases {
		cr, err := ValidateCase(c, pkg, o.Runner, o.RunnerFlags, argv)
		if err != nil {
			return nil, err
		}
		rec.Cases = append(rec.Cases, *cr)
		rec.AllPassed = rec.AllPassed && cr.Pass
	}
	return rec, nil
}

func ValidateCase(c Case, pkg, runner string, runnerFlags, oracle []string) (*CaseReceipt, error) {
	f, err := NewFixture(c)
	if err != nil {
		return nil, err
	}
	defer f.Remove()
	cr := &CaseReceipt{ID: c.ID, Kind: c.Kind, Args: c.Args, Expect: c.Expect}
	fail := func(format string, a ...any) { cr.Failures = append(cr.Failures, fmt.Sprintf(format, a...)) }

	// The oracle runs first, on the same fresh fixture, and must itself
	// leave it as it found it.
	s0, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	exit, so, se, err := RunProgram(f.Work, append(append([]string(nil), oracle...), c.Args...))
	if err == nil {
		err = OracleRan(exit, so)
	}
	if err != nil {
		return nil, fmt.Errorf("case %s oracle: %w: %s", c.ID, err, strings.TrimSpace(se))
	}
	cr.Oracle = Observe(exit, so)
	if c.Expect != nil && !SameResult(*c.Expect, cr.Oracle) {
		fail("oracle drift: the oracle no longer gives the frozen expectation")
	}
	s1, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	if d := SnapshotDiff(s0, s1); len(d) > 0 {
		fail("the oracle changed the fixture: %s", strings.Join(d, ", "))
	}
	cr.Before = SnapshotDigest(s1)

	// The runner's own record goes outside the fixture, so the fixture
	// shows only what the package did.
	journal := filepath.Join(os.TempDir(), "tap-validate-journal-"+c.ID+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".jsonl")
	defer os.Remove(journal)
	run := append(append([]string{runner}, runnerFlags...), "-no-record", "-journal", journal)
	if c.Approve {
		run = append(run, "-approve")
	}
	run = append(append(run, pkg), c.Args...)
	t0 := time.Now()
	_, so, se, err = RunProgram(f.Work, run)
	cr.Elapsed = time.Since(t0).Milliseconds()
	if err != nil {
		fail("runner: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(se), "\n") {
		if line != "" {
			cr.HostLog = append(cr.HostLog, line)
		}
	}
	head, body, _ := strings.Cut(so, "\n")
	var programExit int
	if _, err := fmt.Sscanf(head, "RESULT (exit %d)", &programExit); err != nil {
		fail("the runner did not run the program (no RESULT line)")
		cr.Package = Observed{Exit: -1, Raw: so}
	} else {
		cr.Package = Observe(programExit, body)
	}
	if b, err := os.ReadFile(journal); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var v any
			if line != "" && json.Unmarshal([]byte(line), &v) == nil {
				cr.Actions = append(cr.Actions, v)
			}
		}
	}
	s2, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	cr.After = SnapshotDigest(s2)
	if !c.Approve {
		if d := SnapshotDiff(s1, s2); len(d) > 0 {
			cr.Effects = d
			fail("the package changed the fixture: %s", strings.Join(d, ", "))
		}
	}
	if !SameResult(cr.Oracle, cr.Package) {
		fail("result differs from the oracle")
	}
	cr.Pass = len(cr.Failures) == 0
	return cr, nil
}

// DefaultRunner is the tap executable running this command, or tap on PATH
// when this code runs inside another program (telara's CLI).
func DefaultRunner() string {
	if self, err := os.Executable(); err == nil && strings.HasPrefix(filepath.Base(self), "tap") {
		return self
	}
	if p, err := exec.LookPath("tap"); err == nil {
		return p
	}
	return ""
}

func ValidateCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover validate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cases := fs.String("cases", "", "the case file")
	freeze := fs.Bool("freeze", false, "record the oracle's results as the cases' expectations; runs no package")
	runner := fs.String("runner", DefaultRunner(), "the tap runner to run the package with")
	outFile := fs.String("out", "", "write the receipts here")
	pkgArgs, flagArgs := SplitPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	pkgArgs = append(pkgArgs, fs.Args()...)
	if *cases == "" {
		fmt.Fprintln(errOut, "discover validate: --cases is required")
		return 2
	}
	if *freeze {
		n, err := Freeze(*cases)
		if err != nil {
			fmt.Fprintln(errOut, "discover validate:", err)
			return 1
		}
		fmt.Fprintf(out, "froze %d case expectations in %s\n", n, *cases)
		return 0
	}
	if len(pkgArgs) != 1 || *runner == "" {
		fmt.Fprintln(errOut, "discover validate: give one package directory, and a runner if tap is not on PATH")
		return 2
	}
	rec, err := Validate(ValidateOptions{Package: pkgArgs[0], Cases: *cases, Runner: *runner})
	if err != nil {
		fmt.Fprintln(errOut, "discover validate:", err)
		return 1
	}
	if *outFile != "" {
		b, _ := json.MarshalIndent(rec, "", "  ")
		if err := os.WriteFile(*outFile, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(errOut, "discover validate:", err)
			return 1
		}
	}
	fmt.Fprintf(out, "package %s\nrunner  %s\nmanifest check: exit %d\n", rec.PackageDigest, rec.RunnerSHA256, rec.ManifestCheck.Exit)
	passed := 0
	for _, c := range rec.Cases {
		verdict := "PASS"
		if c.Pass {
			passed++
		} else {
			verdict = "FAIL"
		}
		fmt.Fprintf(out, "  %-4s %-8s %-22s oracle exit %d, package exit %d, %dms\n", verdict, c.ID, c.Kind, c.Oracle.Exit, c.Package.Exit, c.Elapsed)
		for _, f := range c.Failures {
			fmt.Fprintf(out, "         %s\n", f)
		}
	}
	fmt.Fprintf(out, "%d of %d cases passed\n", passed, len(rec.Cases))
	if !rec.AllPassed {
		return 1
	}
	return 0
}

// SplitPositional lets the package come before or after the flags.
func SplitPositional(args []string) (pos, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && a != "--freeze" && a != "-freeze" {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return pos, flags
}
