package primitive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Decision is one recorded menu choice.
type Decision struct {
	ID     string `json:"id"`
	Choice string `json:"choice"` // accept | deny | eval
}

// MenuConfig says where decisions and accepted primitives live.
type MenuConfig struct {
	StateDir string
	// All accepts every proposed primitive without asking.
	All bool
	// Clients names what was read, for the summary.
	Clients string
	// Home, Sessions and Skill feed the agent-eval handoff: transcripts are
	// located under Home, request text comes from Sessions.
	Home     string
	Sessions []trace.Session
	Skill    Skill
}

// LoadKnown returns the primitives accepted earlier on this machine, for
// composition.
func LoadKnown(stateDir string) []Known {
	files, _ := filepath.Glob(filepath.Join(stateDir, "accepted", "*.json"))
	var out []Known
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p Primitive
		if json.Unmarshal(b, &p) == nil && len(p.Steps) > 0 {
			out = append(out, Known{Name: p.ID, Steps: p.Steps})
		}
	}
	return out
}

func loadDecisions(stateDir string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(stateDir, "decisions.jsonl"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		var d Decision
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			out[d.ID] = d.Choice
		}
	}
	return out
}

func record(stateDir string, d Decision) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "decisions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(d)
	_, err = f.Write(append(b, '\n'))
	return err
}

// WriteSummary prints what was read and found.
func WriteSummary(out io.Writer, res Result, clients string) {
	s := res.Summary
	fmt.Fprintln(out, "Summary")
	fmt.Fprintf(out, "  Read %d sessions (%s) holding %d tool calls.\n", s.Sessions, clients, s.ToolCalls)
	fmt.Fprintf(out, "  %d calls are replayable operations, %d distinct; %d were edits or inline scripts (judgment) and %d failed.\n",
		s.Operations, s.DistinctOps, s.JudgmentCalls, s.FailedCalls)
	fmt.Fprintf(out, "  %d calls through a dispatching route were matched to their direct tool; %d arguments were settings (inputs), not separate operations.\n",
		s.RouteMerged, s.SettingsAsInput)
	fmt.Fprintf(out, "  Found %d primitives: %d multi-step, %d single commands that loop over a list; %d are composed of other primitives.\n",
		s.Primitives, s.MultiStep, s.Primitives-s.MultiStep, s.Composed)
}

func describe(p Primitive) string {
	loop := map[int]bool{}
	for _, l := range p.Loops {
		loop[l] = true
	}
	steps := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		steps[i] = short(s)
		if loop[i+1] {
			steps[i] = "for each: " + steps[i]
		}
	}
	return strings.Join(steps, " > ")
}

func short(op string) string {
	op = strings.TrimPrefix(op, "mcp:")
	op = strings.TrimPrefix(op, "sh:")
	if len(op) > 70 {
		op = op[:69] + "…"
	}
	return op
}

func inputNames(p Primitive) string {
	var names []string
	seen := map[string]bool{}
	flags := 0
	for _, in := range p.Inputs {
		k := in.Key
		if seen[k] {
			continue
		}
		seen[k] = true
		if strings.HasPrefix(k, "-") {
			flags++
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	if flags > 0 {
		names = append(names, fmt.Sprintf("+%d flags", flags))
	}
	if len(names) == 0 {
		return "none"
	}
	s := strings.Join(names, ", ")
	if len(s) > 110 {
		s = s[:109] + "…"
	}
	return s
}

// Menu shows the summary and every proposed primitive, then takes accept,
// deny or agent-eval choices. Denied primitives are not shown again.
func Menu(in io.Reader, out io.Writer, res Result, cfg MenuConfig) error {
	decided := loadDecisions(cfg.StateDir)
	var shown []Primitive
	for _, p := range res.Primitives {
		if decided[p.ID] != "deny" {
			shown = append(shown, p)
		}
	}
	WriteSummary(out, res, cfg.Clients)
	if hidden := len(res.Primitives) - len(shown); hidden > 0 {
		fmt.Fprintf(out, "  %d denied earlier and hidden.\n", hidden)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Proposed primitives")
	index := map[string]int{}
	for i, p := range shown {
		index[p.ID] = i + 1
	}
	for i, p := range shown {
		mark := ""
		if c := decided[p.ID]; c != "" {
			mark = " [" + c + "ed]"
		}
		fmt.Fprintf(out, "%3d. %s%s\n", i+1, describe(p), mark)
		fmt.Fprintf(out, "     inputs: %s · effect: %s · sessionCount: %d · executionCount: %d\n", inputNames(p), p.Effect, p.SessionCount, p.ExecutionCount)
		fmt.Fprintf(out, "     flow confidence: %s\n", p.Confidence.Summary())
		fmt.Fprintf(out, "     bindings: %s\n", bindingSummary(p))
		for _, u := range p.Unresolved {
			fmt.Fprintf(out, "     unresolved: %s\n", u)
		}
		if len(p.Variants) > 0 {
			fmt.Fprintf(out, "     variants: %d (choices that change what follows)\n", len(p.Variants))
		}
		if len(p.ComposedOf) > 0 {
			var uses []string
			for _, c := range p.ComposedOf {
				if n, ok := index[c]; ok {
					uses = append(uses, "#"+strconv.Itoa(n))
				} else {
					uses = append(uses, c)
				}
			}
			fmt.Fprintf(out, "     composed of: %s\n", strings.Join(uses, ", "))
		}
	}
	if len(shown) == 0 {
		return nil
	}
	if cfg.All {
		for _, p := range shown {
			if err := accept(cfg.StateDir, p); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "\nAccepted all %d into %s\n", len(shown), filepath.Join(cfg.StateDir, "accepted"))
		return nil
	}
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "\nChoose: <number> a (accept) | d (deny) | e (agent eval); 'all a' accepts every one; q quits > ")
		if !sc.Scan() {
			return sc.Err()
		}
		f := strings.Fields(strings.ToLower(sc.Text()))
		if len(f) == 0 || f[0] == "q" {
			return nil
		}
		if len(f) != 2 {
			fmt.Fprintln(out, "  Give a number (or 'all') and a, d or e.")
			continue
		}
		var targets []Primitive
		if f[0] == "all" {
			targets = shown
		} else if n, err := strconv.Atoi(f[0]); err == nil && n >= 1 && n <= len(shown) {
			targets = []Primitive{shown[n-1]}
		} else {
			fmt.Fprintln(out, "  No such primitive.")
			continue
		}
		for _, p := range targets {
			var msg string
			var err error
			switch f[1] {
			case "a", "accept":
				err = accept(cfg.StateDir, p)
				msg = "accepted: " + filepath.Join(cfg.StateDir, "accepted", p.ID+".json")
			case "d", "deny":
				err = record(cfg.StateDir, Decision{p.ID, "deny"})
				msg = "denied; it will not be shown again"
			case "e", "eval":
				where := filepath.Join(cfg.StateDir, "eval", p.ID)
				err = WriteHandoff(where, cfg.Home, p, cfg.Sessions, cfg.Skill)
				if err == nil {
					err = record(cfg.StateDir, Decision{p.ID, "eval"})
				}
				msg = "agent eval handoff: " + where + " (give your coding agent HANDOFF.md)"
			default:
				fmt.Fprintln(out, "  Choose a, d or e.")
				continue
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  #%d %s\n", index[p.ID], msg)
		}
	}
}

func accept(stateDir string, p Primitive) error {
	dir := filepath.Join(stateDir, "accepted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, p.ID+".json"), b, 0o600); err != nil {
		return err
	}
	return record(stateDir, Decision{p.ID, "accept"})
}

// bindingSummary counts each argument's evidence level.
func bindingSummary(p Primitive) string {
	n := map[string]int{}
	for _, b := range p.Bindings {
		n[b.Label]++
	}
	if len(n) == 0 {
		return "no value flows"
	}
	var parts []string
	for _, l := range []string{"explicit", "inferred", "ambiguous", "missing"} {
		if n[l] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[l], l))
		}
	}
	return strings.Join(parts, ", ")
}
