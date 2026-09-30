package discover

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteText prints a report for a person: what was read, the qualified
// candidates with their templates, and how well known skills were recovered.
func WriteText(w io.Writer, r *Report, top int) {
	fmt.Fprintf(w, "Rules %s, window %d, min support %d, max length %d, %d permutations, FDR %.2f, seed %d\n\n",
		r.RulesVersion, r.Options.Window, r.Options.MinSupport, r.Options.MaxLen, r.Options.Permutations, r.Options.Alpha, r.Options.Seed)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLIENT\tSESSIONS\tDUPLICATES\tCALLS\tSTEPS\tFROM\tTO\tNOTE")
	for _, c := range r.Clients {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", c.Client, c.Sessions, c.DuplicateSessions, c.Calls, c.Steps, day(c.Earliest), day(c.Latest), c.Error)
	}
	tw.Flush()
	trunc := ""
	if r.Truncated {
		trunc = " (stopped at the kept-pattern limit; raise --max-patterns)"
	}
	fmt.Fprintf(w, "\n%d distinct step labels. %d patterns examined at support >= %d, %d kept%s; %d passed the per-step test and were shuffle-tested; %d qualified, in %d families.\n\n",
		r.Labels, r.Examined, r.MinSupportUsed, r.Mined, trunc, r.Tested, r.Qualified, r.Families)

	variants := map[int]int{}
	for i, c := range r.Candidates {
		if c.Qualified && c.Family != i {
			variants[c.Family]++
		}
	}
	shown := 0
	for i, c := range r.Candidates {
		if !c.Qualified || c.Family != i || shown >= top {
			continue
		}
		shown++
		clients := make([]string, 0, len(c.ByClient))
		for k, v := range c.ByClient {
			clients = append(clients, fmt.Sprintf("%s %d", k, v))
		}
		ordered := "order not significant"
		if c.Ordered {
			ordered = "ordered"
		}
		fmt.Fprintf(w, "#%d  %d sessions (%s), null mean %.1f, q %.1e, %s, stability %.2f, specificity %.2f, %d weeks, median gap %.1f days, %s → %s\n",
			shown, c.Sessions, strings.Join(clients, ", "), c.NullMean, c.Q, ordered, c.Stability, c.Specificity, c.Weeks, c.MedianGapDays, day(c.FirstSeen), day(c.LastSeen))
		for i, s := range c.Steps {
			fmt.Fprintf(w, "    %d. %s   [stability %.2f, weight %.2f]\n", i+1, s.Template, s.Stability, s.Weight)
		}
		if c.Measured > 0 {
			fmt.Fprintf(w, "    tokens per run %s; a primitive saves %s per run, %s over %d measured runs\n",
				fmtUsage(c.PerRun), fmtUsage(c.SavedPerRun), fmtUsage(c.SavedTotal), c.Measured)
		}
		if n := variants[i]; n > 0 {
			fmt.Fprintf(w, "    +%d variant patterns over mostly the same sessions\n", n)
		}
		fmt.Fprintf(w, "    e.g. %s\n\n", strings.Join(c.Examples, ", "))
	}
	if shown == 0 {
		fmt.Fprintln(w, "No candidate recurs beyond chance.")
	}

	if len(r.Skills) > 0 {
		fmt.Fprintln(w, "\nProcedures specific to a skill (sessions that loaded it vs. those that did not):")
		for i, sk := range r.Skills {
			if i >= top {
				break
			}
			fmt.Fprintf(w, "\n  %s — loaded in %d sessions, %d enriched patterns\n", sk.Skill, sk.Sessions, sk.Significant)
			for j, p := range sk.Procedures {
				if j >= 3 {
					break
				}
				where := fmt.Sprintf("%d elsewhere, lift %.1f", p.Outside, p.Lift)
				if p.OnlyInSkill {
					where = "never elsewhere"
				}
				fmt.Fprintf(w, "    in %d of its sessions (%.0f%%), %s, q %.1e:\n", p.InSkill, 100*p.Coverage, where, p.EnrichmentQ)
				for k, st := range p.Steps {
					fmt.Fprintf(w, "      %d. %s\n", k+1, st.Template)
				}
			}
		}
	}

	if len(r.Recall) > 0 {
		fmt.Fprintln(w, "\nKnown skills (loaded in 2+ sessions) and the best-matching candidate:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SKILL\tSESSIONS\tBEST QUALIFIED F1\tBEST TESTED F1\tBEST TESTED PATTERN")
		for _, s := range r.Recall {
			q, t, pat := "-", "-", ""
			if s.BestQualified != nil {
				q = fmt.Sprintf("%.2f", s.BestQualified.F1)
			}
			if s.BestTested != nil {
				t = fmt.Sprintf("%.2f (P %.2f R %.2f)", s.BestTested.F1, s.BestTested.Precision, s.BestTested.Recall)
				pat = s.BestTested.Pattern
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", s.Skill, s.Sessions, q, t, pat)
		}
		tw.Flush()
	}
}

func day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}

// fmtUsage prints fresh input, cached input and output separately: cached
// input costs a small fraction of fresh.
func fmtUsage(u Usage) string {
	return fmt.Sprintf("%s (%s fresh, %s cached, %s out)", humanTokens(u.Total()), humanTokens(u.Fresh), humanTokens(u.Cached), humanTokens(u.Output))
}

func humanTokens(t float64) string {
	switch {
	case t >= 1e6:
		return fmt.Sprintf("%.1fM", t/1e6)
	case t >= 1e3:
		return fmt.Sprintf("%.1fk", t/1e3)
	}
	return fmt.Sprintf("%.0f", t)
}

// WriteFunnel prints the request-level result: what was read, how it narrowed
// to primitives, and the primitives. With rejected, it also lists what each
// check removed and why.
func WriteFunnel(w io.Writer, r *Report, top int, rejected bool) {
	f := r.Funnel
	var clients []string
	for _, c := range r.Clients {
		clients = append(clients, fmt.Sprintf("%s %d", c.Client, c.Sessions))
	}
	fmt.Fprintf(w, "Reviewed %d sessions (%s) and %d tool calls.\n", f.Sessions, strings.Join(clients, ", "), f.Calls)
	fmt.Fprintf(w, "%d requests; %d ran at least two steps a primitive could replay.\n", f.Requests, f.RequestsWithSteps)
	fmt.Fprintf(w, "Grouped into %d kinds of request; %d recurred (%d+ requests in 2+ sessions): the routines.\n", f.Groups, f.Routines, r.Options.MinSupport)
	for _, ck := range CheckOrder {
		if n := f.Removed[ck]; n > 0 {
			fmt.Fprintf(w, "  - %d removed by \"%s\"\n", n, ck)
		}
	}
	if f.Merged > 0 {
		fmt.Fprintf(w, "  - %d merged into another routine with the same kind and steps\n", f.Merged)
	}
	fmt.Fprintf(w, "Consolidated to %d primitives ready to save, and %d more that need authoring before they can run.\n", f.Primitives, f.NeedsAuthoring)
	var kinds []string
	for _, k := range []string{"user", "automated", "scheduled", "bookkeeping"} {
		if n := f.ByKind[k]; n > 0 {
			kinds = append(kinds, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(kinds) > 0 {
		fmt.Fprintf(w, "The primitives by who the work is for: %s.\n", strings.Join(kinds, ", "))
	}
	fmt.Fprintln(w, "Savings are estimates from the recorded token use, mostly cached input; no primitive run was measured.")
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tKIND\tCOVERS\tREQUESTS\tSESSIONS\tWEEKS\tSAVED/RUN\tSAVED TOTAL\tSTEPS\tINPUTS\tEXAMPLE REQUEST")
	shown := 0
	for _, rt := range r.Routines {
		if rt.Decision != "primitive" || rt.MergedInto != "" || (top > 0 && shown >= top) {
			continue
		}
		shown++
		saved, per := "-", "-"
		if rt.Measured > 0 {
			saved, per = humanTokens(rt.SavedTotal.Total()), humanTokens(rt.SavedPerRun.Total())
		}
		fixed := rt.Coverage
		fmt.Fprintf(tw, "%d\t%s\t%.0f%%\t%d\t%d\t%d\t%s\t%s\t%s\t%d\t%s\n", shown, rt.Kind, 100*fixed, rt.Requests, rt.Sessions, rt.Weeks, per, saved,
			oneLine(labelsOf(rt.Candidate), 70), len(rt.Inputs), oneLine(rt.Example, 60))
	}
	tw.Flush()
	if f.NeedsAuthoring > 0 {
		fmt.Fprintln(w, "\nNeed authoring (they recur and replay, but part must be written by hand):")
		n := 0
		for _, rt := range r.Routines {
			if rt.Decision != "needs_authoring" || rt.MergedInto != "" || (top > 0 && n >= top) {
				continue
			}
			n++
			fmt.Fprintf(w, "  %d requests, %d weeks: %s\n      %s\n", rt.Requests, rt.Weeks, oneLine(labelsOf(rt.Candidate), 90), rt.Why)
		}
	}
	if !rejected {
		fmt.Fprintln(w, "\n(--rejected lists the routines each check removed.)")
		return
	}
	for _, ck := range CheckOrder {
		fmt.Fprintf(w, "\nRemoved by \"%s\":\n", ck)
		for _, rt := range r.Routines {
			if rt.Failed == ck {
				fmt.Fprintf(w, "  %d requests, %d weeks: %s\n      %s\n", rt.Requests, rt.Weeks, oneLine(labelsOf(rt.Candidate), 90), rt.Why)
			}
		}
	}
}

// Primitives returns the routines ready to save (decision "primitive"), in
// report order.
func (r *Report) Primitives() []*Routine {
	var out []*Routine
	for i := range r.Routines {
		if r.Routines[i].Decision == "primitive" && r.Routines[i].MergedInto == "" {
			out = append(out, &r.Routines[i])
		}
	}
	return out
}
