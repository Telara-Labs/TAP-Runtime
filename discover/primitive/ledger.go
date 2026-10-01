package primitive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RulesVersion names the discovery rules whose output a decision was made
// on. Change it when a rules change alters what a person would have seen;
// earlier decisions are then shown again, marked as re-evaluated.
const RulesVersion = "discover-rules-2026-10-01"

// LedgerEntry is one decision, appended to decisions.jsonl and never
// rewritten. It is keyed by a fingerprint that survives new runs: the head
// command and whether it reads or may write. A decision covers the
// follow-ups the person saw; a follow-up seen later is asked about on its
// own. Only structure is stored, never request text or argument values.
type LedgerEntry struct {
	Fingerprint string   `json:"fingerprint"`
	FollowUps   []string `json:"followUps"`
	// Decision is accept, deny, eval, or undo (forget the follow-ups again).
	Decision string `json:"decision"`
	At       string `json:"at"`
	Rules    string `json:"rules"`
	// Evidence at decision time, for the person's own record.
	Runs     int   `json:"runs"`
	Sessions int   `json:"sessions"`
	Tokens   int64 `json:"estTokens"`
}

// Ledger is the latest decision per fingerprint and follow-up.
type Ledger map[string]map[string]LedgerEntry

func ledgerPath(stateDir string) string { return filepath.Join(stateDir, "decisions.jsonl") }

// LoadLedger reads every decision; later lines win, an undo forgets. A line
// that does not parse is skipped, so a torn last write loses one decision,
// never the ledger.
func LoadLedger(stateDir string) Ledger {
	l := Ledger{}
	f, err := os.Open(ledgerPath(stateDir))
	if err != nil {
		return l
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e LedgerEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Fingerprint == "" {
			continue
		}
		if l[e.Fingerprint] == nil {
			l[e.Fingerprint] = map[string]LedgerEntry{}
		}
		for _, fu := range e.FollowUps {
			if e.Decision == "undo" {
				delete(l[e.Fingerprint], fu)
				continue
			}
			l[e.Fingerprint][fu] = e
		}
	}
	return l
}

// appendLedger adds one decision with a single write: an interrupted write
// leaves at most a partial last line, which LoadLedger skips.
func appendLedger(stateDir string, e LedgerEntry) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	e.At = time.Now().UTC().Format(time.RFC3339)
	if e.Rules == "" {
		e.Rules = RulesVersion
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "a > b" readable
	if err := enc.Encode(e); err != nil {
		return err
	}
	line := buf.Bytes()
	// A torn last line (an interrupted write) must not swallow this one.
	if old, err := os.ReadFile(ledgerPath(stateDir)); err == nil && len(old) > 0 && old[len(old)-1] != '\n' {
		line = append([]byte{'\n'}, line...)
	}
	f, err := os.OpenFile(ledgerPath(stateDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func followUpKey(fu FollowUp) string { return strings.Join(fu.Steps, " > ") }

// entryFor records a decision on everything the person saw of a family.
func entryFor(f Family, decision string) LedgerEntry {
	e := LedgerEntry{Fingerprint: f.Fingerprint, Decision: decision,
		Runs: f.ExecutionCount, Sessions: f.SessionCount, Tokens: int64(inputEquivalent(f.Saved))}
	for _, fu := range f.FollowUps {
		e.FollowUps = append(e.FollowUps, followUpKey(fu))
	}
	return e
}

// Status says why a family is shown.
const (
	StatusNew          = "new"
	StatusNewSince     = "new since your last decision"
	StatusReevaluated  = "re-evaluated by new rules"
	StatusAlreadyKnown = "decided"
)

// Triage splits proposed families by the ledger. A family whose every
// follow-up was decided under the current rules is hidden and counted. A
// family with follow-ups nobody decided is shown with only those (rebuilt
// from just their chains). A family decided under other rules is shown again,
// marked re-evaluated.
func Triage(res Result, l Ledger) (shown []Family, hidden map[string]int) {
	hidden = map[string]int{}
	byID := map[string]Primitive{}
	for _, p := range res.Primitives {
		byID[p.ID] = p
	}
	for _, f := range res.Families {
		seen := l[f.Fingerprint]
		if len(seen) == 0 {
			f.Status = StatusNew
			shown = append(shown, f)
			continue
		}
		var undecided []Primitive
		stale, last := false, ""
		decided := map[string]int{}
		for _, id := range f.Members {
			p := byID[id]
			e, ok := seen[tail(p)]
			if !ok {
				undecided = append(undecided, p)
				continue
			}
			decided[e.Decision]++
			if e.Rules != RulesVersion {
				stale = true
			}
			if e.At > last {
				last = e.At
			}
		}
		switch {
		case stale:
			f.Status = StatusReevaluated
			shown = append(shown, f)
		case len(undecided) > 0:
			d := familyOf(undecided)
			d.Status = StatusNewSince
			d.Earlier = majority(decided)
			shown = append(shown, d)
		default:
			hidden[majority(decided)]++
		}
	}
	return shown, hidden
}

func majority(m map[string]int) string {
	best, n := "", -1
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m[k] > n {
			best, n = k, m[k]
		}
	}
	return best
}

// familyOf builds a family from a subset of its chains.
func familyOf(members []Primitive) Family {
	heads, tails := map[string]int{}, map[string]int{}
	for _, p := range members {
		heads[headKey(p.Steps[0])] += p.ExecutionCount
		tails[tail(p)] += p.ExecutionCount
	}
	return buildFamily(heads, tails, members)
}

// Revisit lists the decisions in force and lets the person undo one. An
// undo is appended (the ledger is never rewritten); the follow-ups it covers
// are proposed again on the next run.
func Revisit(in io.Reader, out io.Writer, stateDir string, color bool) error {
	s := style{on: color}
	l := LoadLedger(stateDir)
	type row struct {
		fp, decision, at, rules string
		followUps               []string
	}
	var rows []row
	var fps []string
	for fp := range l {
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	for _, fp := range fps {
		by := map[string]*row{}
		var order []string
		for fu, e := range l[fp] {
			r := by[e.Decision]
			if r == nil {
				r = &row{fp: fp, decision: e.Decision, rules: e.Rules}
				by[e.Decision] = r
				order = append(order, e.Decision)
			}
			r.followUps = append(r.followUps, fu)
			if e.At > r.at {
				r.at = e.At
			}
		}
		sort.Strings(order)
		for _, d := range order {
			sort.Strings(by[d].followUps)
			rows = append(rows, *by[d])
		}
	}
	section(out, s, "Decisions in force")
	if len(rows) == 0 {
		fmt.Fprintln(out, "  None yet.")
		return nil
	}
	t := table{head: []string{"#", "Starts with", "Decision", "Covers", "Decided"}, widths: []int{3, 30, 8, 40, 10}, right: map[int]bool{0: true}, flex: 4}
	for i, r := range rows {
		head, _, _ := strings.Cut(r.fp, "|")
		var covers []string
		for _, fu := range r.followUps {
			var steps []string
			for _, x := range strings.Split(fu, " > ") {
				steps = append(steps, display(x))
			}
			covers = append(covers, strings.Join(steps, ", then "))
		}
		t.rows = append(t.rows, []string{fmt.Sprint(i + 1), display(head), s.choice(r.decision), "then " + strings.Join(covers, " · "), r.at[:min(10, len(r.at))]})
	}
	t.render(out, s)
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "\n"+keys(s, "<n> u", "undo that decision", "q", "quit")+s.accent(" › "))
		if !sc.Scan() {
			return sc.Err()
		}
		f := strings.Fields(strings.ToLower(sc.Text()))
		if len(f) == 0 || f[0] == "q" {
			return nil
		}
		n, err := strconv.Atoi(f[0])
		if len(f) != 2 || f[1] != "u" || err != nil || n < 1 || n > len(rows) {
			fmt.Fprintln(out, s.dim("  Give a number and u, or q."))
			continue
		}
		r := rows[n-1]
		if err := appendLedger(stateDir, LedgerEntry{Fingerprint: r.fp, FollowUps: r.followUps, Decision: "undo"}); err != nil {
			return err
		}
		fmt.Fprintf(out, "  Undone: #%d %s will be proposed again on the next run.\n", n, r.decision)
	}
}
