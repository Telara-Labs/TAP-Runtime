package discover

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
)

// The author path (TENG-2936, doc 26). Discover proposes; it does not
// establish that a task is a useful procedure. A brief hands one selected
// task, or one candidate from a report, to the agent already working with
// the user, which establishes the contract from the task's context and
// writes the package. `tap discover validate` then runs that package through
// the real runner against an independent oracle, and `tap discover save`
// installs it with a validation status bound to its exact digest.
//
//	tap discover brief --task claude-code/<session>/<request> --out DIR
//	tap discover brief --candidate <routine id> --report report.json --out DIR
//
// A brief is private: it holds the request's text and the start of each
// result, from this machine's history, so it is written to a directory the
// user names and never anywhere else.

// BriefStatus is every brief's status. Handing a task to an agent says
// nothing about whether it is a useful procedure; the agent establishes that.
const BriefStatus = "unassessed_proposal"

// Selection kinds.
const (
	SelectedTask      = "selected_task"
	DiscoverCandidate = "discover_candidate"
)

// Brief is what the agent authoring a package starts from.
type Brief struct {
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Selection string `json:"selection"`
	// Ref is an opaque name for the source request, safe to copy into a
	// package's provenance; Source is the private location it stands for.
	Ref       string          `json:"ref"`
	Source    SourceRef       `json:"source"`
	Candidate *BriefCandidate `json:"candidate,omitempty"`
	Evidence  BriefEvidence   `json:"evidence"`
	// Missing lists the contract fields nothing has established yet. Every
	// field is missing for a selected task.
	Missing  []string      `json:"missing"`
	Contract BriefContract `json:"contract"`
}

// BriefCandidate is what Discover said about a candidate, carried as its
// claim, not as the contract.
type BriefCandidate struct {
	Routine      string      `json:"routine"`
	ReportDigest string      `json:"report_digest"`
	Decision     string      `json:"decision"`
	Failed       string      `json:"failed,omitempty"`
	Why          string      `json:"why,omitempty"`
	SourceRole   string      `json:"source_role"`
	Suitability  string      `json:"suitability"`
	Contract     Contract    `json:"contract"`
	Sources      []SourceRef `json:"sources"`
}

// BriefEvidence is the source request and the calls it made.
type BriefEvidence struct {
	Request string      `json:"request"`
	Steps   []BriefStep `json:"steps"`
	// Requests is how many requests the session holds, so the agent knows
	// whether the task continued in a later one.
	Requests int `json:"requests"`
}

type BriefStep struct {
	N       int               `json:"n"`
	Tool    string            `json:"tool"`
	Command string            `json:"command,omitempty"`
	Args    map[string]string `json:"args,omitempty"`
	Outcome string            `json:"outcome"`
	Output  string            `json:"output,omitempty"`
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
func OpaqueRef(s SourceRef) string {
	sum := sha256.Sum256([]byte(s.Client + "/" + s.Session + "/" + strconv.Itoa(s.Request)))
	return "src_" + hex.EncodeToString(sum[:6])
}

// NewBrief builds the brief for request req of session s. cand is the
// Discover candidate the request came from, or nil for a selected task.
func NewBrief(s Session, req int, cand *BriefCandidate) (*Brief, error) {
	if req < 0 || req >= len(s.Requests) {
		return nil, fmt.Errorf("session %s has %d requests; request %d does not exist", s.ID, len(s.Requests), req)
	}
	src := SourceRef{Client: s.Client, Session: s.ID, Request: req}
	b := &Brief{
		Kind: "tap.authoring-brief/v1", Status: BriefStatus, Selection: SelectedTask,
		Ref: OpaqueRef(src), Source: src, Candidate: cand,
		Evidence: BriefEvidence{Request: Redact(s.Requests[req]), Requests: len(s.Requests)},
	}
	for _, c := range s.Calls {
		if c.Request != req {
			continue
		}
		st := BriefStep{N: len(b.Evidence.Steps) + 1, Tool: c.Tool, Command: Redact(c.Command), Outcome: outcomeName(c.Outcome), Output: Redact(c.Output)}
		if len(c.Args) > 0 {
			st.Args = map[string]string{}
			for k, v := range c.Args {
				st.Args[k] = Redact(v)
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
	b.Missing = b.Contract.missing()
	return b, nil
}

func (c BriefContract) missing() []string {
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

func outcomeName(o Outcome) string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeFailed:
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
		Routines []Routine `json:"routines"`
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
func FindSession(client, id, home string) (Session, error) {
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
		ss, err := Cursor{DB: CursorStateDB(home)}.Read(time.Time{})
		if err != nil {
			return Session{}, err
		}
		for _, s := range ss {
			if s.ID == id {
				return s, nil
			}
		}
	default:
		return Session{}, fmt.Errorf("unknown client %q (want claude-code, codex or cursor)", client)
	}
	sort.Strings(files)
	for _, f := range files {
		var s Session
		var err error
		if client == "claude-code" {
			s, err = readClaudeFile(f)
		} else {
			s, err = readCodexFile(f)
		}
		if err == nil && (s.ID == id || strings.Contains(filepath.Base(f), id)) {
			return s, nil
		}
	}
	return Session{}, fmt.Errorf("no %s session %s under %s", client, id, home)
}

// ParseTaskRef reads client/session/request.
func ParseTaskRef(ref string) (SourceRef, error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 3 {
		return SourceRef{}, fmt.Errorf("task %q: want client/session/request", ref)
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 0 {
		return SourceRef{}, fmt.Errorf("task %q: request must be a number", ref)
	}
	return SourceRef{Client: parts[0], Session: parts[1], Request: n}, nil
}

func briefCommand(args []string, home string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover brief", flag.ContinueOnError)
	fs.SetOutput(errOut)
	task := fs.String("task", "", "the selected task, as client/session/request")
	candidate := fs.String("candidate", "", "a routine id from a discover report")
	report := fs.String("report", "", "with --candidate: the report written by `tap discover --out`")
	source := fs.Int("source", 0, "with --candidate: which of its source requests to brief from")
	dir := fs.String("out", "", "directory to write brief.json and BRIEF.md into (private)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || (*task == "") == (*candidate == "") || (*candidate != "" && *report == "") {
		fmt.Fprintln(errOut, "discover brief: give --out and exactly one of --task or --candidate with --report")
		return 2
	}
	var cand *BriefCandidate
	var ref SourceRef
	var err error
	if *candidate != "" {
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
	b, err := NewBrief(s, ref.Request, cand)
	if err != nil {
		fmt.Fprintln(errOut, "discover brief:", err)
		return 1
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
	if err := os.WriteFile(filepath.Join(dir, "BRIEF.md"), []byte(b.markdown()), 0o600); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (b *Brief) markdown() string {
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
	fmt.Fprintf(&w, "## Contract fields not yet established\n\n%s\n\n", strings.Join(b.Missing, ", "))
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
				what += k + "=" + truncateUTF8(st.Args[k], 120) + " "
			}
		}
		fmt.Fprintf(&w, "%d. `%s` %s [%s]\n", st.N, st.Tool, strings.TrimSpace(truncateUTF8(strings.ReplaceAll(what, "\n", " "), 300)), st.Outcome)
	}
	w.WriteString(`
## What to do

1. Read the source session if the steps above do not settle a field. The ref above names it.
   Establish each contract field (goal, inputs with their types and where each value comes from,
   scope, procedure, output, oracle, failure behaviour, and the point where the procedure hands
   back to you). Record what established each one. A word in the request, or an output label,
   is not enough. If the task is not a bounded procedure, stop and say why.
2. Write the package as docs/writing-a-primitive.md describes: primitive.yaml declaring every
   command, file and tool it uses, and one program. No TODO: or REPLACE_ markers may remain.
   Check it with ` + "`tap manifest check <package>`" + `.
3. Put AUTHORING.json in the package:
   {"kind": "tap.authoring/v1", "name": ..., "publisher": ..., "author": "host-agent",
    "agent": <client and model>, "selection": <selected_task|discover_candidate>,
    "sources": [<ref>], "brief_digest": <printed by brief>, "contract": {<field>: {"value", "established_by"}},
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
