package routine

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// WriteText prints a report for a person: what was read, the qualified
// candidates with their templates, and how well known skills were recovered.
func WriteText(w io.Writer, r *model.Report, top int) {
	fmt.Fprintf(w, "Rules %s, window %d, min support %d, max length %d, %d permutations, FDR %.2f, seed %d\n\n",
		r.RulesVersion, r.Options.Window, r.Options.MinSupport, r.Options.MaxLen, r.Options.Permutations, r.Options.Alpha, r.Options.Seed)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLIENT\tSESSIONS\tDUPLICATES\tCALLS\tSTEPS\tFROM\tTO\tNOTE")
	for _, c := range r.Clients {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", c.Client, c.Sessions, c.DuplicateSessions, c.Calls, c.Steps, Day(c.Earliest), Day(c.Latest), c.Error)
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
			shown, c.Sessions, strings.Join(clients, ", "), c.NullMean, c.Q, ordered, c.Stability, c.Specificity, c.Weeks, c.MedianGapDays, Day(c.FirstSeen), Day(c.LastSeen))
		for i, s := range c.Steps {
			fmt.Fprintf(w, "    %d. %s   [stability %.2f, weight %.2f]\n", i+1, s.Template, s.Stability, s.Weight)
		}
		if c.Measured > 0 {
			fmt.Fprintf(w, "    tokens per run %s; a primitive saves %s per run, %s over %d measured runs\n",
				FmtUsage(c.PerRun), FmtUsage(c.SavedPerRun), FmtUsage(c.SavedTotal), c.Measured)
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

func Day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}

// FmtUsage prints fresh input, cached input and output separately: cached
// input costs a small fraction of fresh.
func FmtUsage(u trace.Usage) string {
	return fmt.Sprintf("%s (%s fresh, %s cached, %s out)", HumanTokens(u.Total()), HumanTokens(u.Fresh), HumanTokens(u.Cached), HumanTokens(u.Output))
}

func HumanTokens(t float64) string {
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
func WriteFunnel(w io.Writer, r *model.Report, top int, rejected bool) {
	f := r.Funnel
	var clients []string
	for _, c := range r.Clients {
		clients = append(clients, fmt.Sprintf("%s %d", c.Client, c.Sessions))
	}
	fmt.Fprintf(w, "Reviewed %d sessions (%s) and %d tool calls.\n", f.Sessions, strings.Join(clients, ", "), f.Calls)
	fmt.Fprintf(w, "%d requests; %d ran at least two steps a program could replay.\n", f.Requests, f.RequestsWithSteps)
	parts := 0
	for _, rt := range r.Routines {
		if rt.Parent != "" {
			parts++
		}
	}
	fmt.Fprintf(w, "Grouped into %d kinds of request; %d routines recurred (%d+ requests in 2+ sessions), %d of them bounded parts found inside larger work.\n",
		f.Groups, f.Routines, r.Options.MinSupport, parts)
	if f.Merged > 0 {
		fmt.Fprintf(w, "%d routines duplicated another (same role, steps in the same order, scope and task family) and are counted once.\n", f.Merged)
	}
	count := func(m map[string]int, keys ...string) string {
		var out []string
		for _, k := range keys {
			if n := m[k]; n > 0 {
				out = append(out, fmt.Sprintf("%d %s", n, strings.ReplaceAll(k, "_", " ")))
			}
		}
		if len(out) == 0 {
			return "none"
		}
		return strings.Join(out, ", ")
	}
	roles, suits := map[string]int{}, map[string]int{}
	for role, m := range f.ByRole {
		for suit, n := range m {
			roles[role] += n
			suits[suit] += n
		}
	}
	fmt.Fprintf(w, "Who the work is for: %s.\n", count(roles, model.RoleUser, model.RoleScheduled, model.RoleInfrastructure, model.RoleHarness, model.RoleUnknown))
	fmt.Fprintf(w, "Is it a useful procedure: %s.\n", count(suits, model.SuitUseful, model.SuitInsufficient, model.SuitInvestigation, model.SuitInvalid))
	useful := f.ByRole[model.RoleUser][model.SuitUseful]
	fmt.Fprintf(w, "Useful procedures for user tasks: %d; drafts: %s. Scheduled work that is already automated: %d (a baseline, not new automation).\n",
		useful, count(f.ByDraft[model.SuitUseful], model.DraftComplete, model.DraftNeedsAuthor, model.DraftBlocked), f.ByRole[model.RoleScheduled][model.SuitUseful])
	fmt.Fprintf(w, "Outcome evidence: %s. Validation: %s. Value: %s.\n",
		count(f.ByOutcome, model.OutcomeToolOK, model.OutcomeEvUnknown, model.OutcomeEvFailed), count(f.ByValidation, model.ValidationNotRun), count(f.ByValue, model.ValueEstimated, model.ValueUnmeasured))
	fmt.Fprintln(w, "No draft has been executed. \"Structurally complete\" means the package is written and passes publish checks, not that it works: validate it on fresh inputs first.")
	fmt.Fprintln(w, "Savings are estimates from recorded token use, mostly cached input; no primitive run was measured.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Recommended (useful procedures for user tasks; unvalidated):")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tDRAFT\tREQUESTS\tSESSIONS\tWEEKS\tEFFECT\tINPUTS\tSTEPS\tEXAMPLE REQUEST")
	shown := 0
	for _, rt := range r.Routines {
		if rt.Suitability != model.SuitUseful || rt.SourceRole != model.RoleUser || rt.MergedInto != "" || (top > 0 && shown >= top) {
			continue
		}
		shown++
		var ins []string
		for _, in := range rt.Contract.Inputs {
			ins = append(ins, in.Source)
		}
		draft := rt.DraftStatus
		if len(rt.Blockers) > 0 {
			draft += " (" + strings.Join(rt.Blockers, ", ") + ")"
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", shown, trace.OneLine(draft, 40), rt.Requests, rt.Sessions, rt.Weeks, rt.Contract.Effect,
			trace.OneLine(strings.Join(ins, ","), 30), trace.OneLine(model.LabelsOf(rt.Candidate), 60), trace.OneLine(rt.Example, 50))
	}
	tw.Flush()
	if shown == 0 {
		fmt.Fprintln(w, "  none")
	}
	if n := f.ByRole[model.RoleScheduled][model.SuitUseful]; n > 0 {
		fmt.Fprintln(w, "\nScheduled work (already automated; listed as a baseline):")
		for _, rt := range r.Routines {
			if rt.Suitability == model.SuitUseful && rt.SourceRole == model.RoleScheduled && rt.MergedInto == "" {
				fmt.Fprintf(w, "  %d runs: %s\n", rt.Requests, trace.OneLine(model.LabelsOf(rt.Candidate), 100))
			}
		}
	}
	if !rejected {
		fmt.Fprintln(w, "\n(--rejected lists every other routine by the reason it is not recommended.)")
		return
	}
	byReason := map[string][]model.Routine{}
	var reasons []string
	for _, rt := range r.Routines {
		if rt.Suitability == model.SuitUseful || rt.MergedInto != "" {
			continue
		}
		k := rt.Suitability + ": " + strings.SplitN(FirstReason(rt), ":", 2)[0]
		if _, ok := byReason[k]; !ok {
			reasons = append(reasons, k)
		}
		byReason[k] = append(byReason[k], rt)
	}
	sort.Strings(reasons)
	for _, k := range reasons {
		fmt.Fprintf(w, "\n%s (%d):\n", k, len(byReason[k]))
		for _, rt := range byReason[k] {
			fmt.Fprintf(w, "  %d requests, %s: %s\n      %s\n", rt.Requests, rt.SourceRole, trace.OneLine(model.LabelsOf(rt.Candidate), 90), trace.OneLine(strings.Join(rt.Reasons, "; "), 140))
		}
	}
}

func FirstReason(rt model.Routine) string {
	if len(rt.Reasons) == 0 {
		return "unknown"
	}
	return rt.Reasons[0]
}

// Primitives returns the routines ready to save (decision "primitive"), in
// report order.
func ReportPrimitives(r *model.Report) []*model.Routine {
	var out []*model.Routine
	for i := range r.Routines {
		if r.Routines[i].Decision == "primitive" && r.Routines[i].MergedInto == "" {
			out = append(out, &r.Routines[i])
		}
	}
	return out
}

// WriteOpportunities says how many requests the selection pass surfaced,
// by route, and lists the n highest-ranked contract groups with the id of
// the example an authoring brief takes.
func WriteOpportunities(w io.Writer, r *model.Report, n int) {
	by := map[string]int{}
	for _, o := range r.Opportunities {
		by[o.Route]++
	}
	fmt.Fprintf(w, "\nSurfaced %d opportunities from the whole history by mechanical evidence (%d stated template, %d re-run check, %d parametric loop, %d single pass), in %d contract groups.\n",
		len(r.Opportunities), by[model.RouteStatedTemplate], by[model.RouteRerunCheck], by[model.RouteParamLoop], by[model.RouteNamedObject], len(r.OpportunityGroups))
	fmt.Fprintln(w, "Each is an unassessed proposal, not a verified procedure. Brief a group's example with `tap discover brief --opportunity <id> --report <file>`.")
	for i, g := range r.OpportunityGroups {
		if i == n {
			break
		}
		fmt.Fprintf(w, "  %3d. %-16s %3d sessions %4d requests  last %s  %s\n       %s\n",
			i+1, g.Route, g.Sessions, g.Requests, g.Last.Format("2006-01-02"), g.Example.ID, trace.OneLine(g.Contract, 110))
	}
}

// WriteSpanProposals lists structural retrieval candidates separately from
// recommendations. A group is a review aid, not a certified procedure.
func WriteSpanProposals(w io.Writer, r *model.Report, n int) {
	if len(r.LogicFunnels) > 0 {
		fmt.Fprintf(w, "\nFound %d result-flow funnels. A root's branches are observed follow-up operations; repeated branches can become loops over runtime inputs. Funnels are structural, not validated primitives.\n", len(r.LogicFunnels))
		for i, f := range r.LogicFunnels {
			if i == n {
				break
			}
			var branches []string
			for _, b := range f.Branches {
				name := b.Action
				if b.ForEach {
					name += "[*]"
				}
				branches = append(branches, name)
			}
			fmt.Fprintf(w, "  %3d. %3d sessions  %s  %s -> {%s}\n       raw candidate IDs %s\n", i+1, f.Sessions, f.ID, f.Root, strings.Join(branches, ", "), trace.OneLine(strings.Join(f.CandidateIDs, ", "), 100))
		}
	}
	if len(r.LogicCandidates) > 0 {
		fmt.Fprintf(w, "\nFound %d raw execution-logic candidates from %d diagnostic spans. Concrete inputs and outputs are parameters, not admission gates. Candidates still need authoring and validation.\n", len(r.LogicCandidates), len(r.SpanProposals))
		fmt.Fprintln(w, "Brief a candidate with `tap discover brief --logic <id> --report <file> --out <private-dir>`; the brief compares up to three independent executions.")
		for i, c := range r.LogicCandidates {
			if i == n {
				break
			}
			fmt.Fprintf(w, "  %3d. %3d sessions %4d executions  %s  %s\n       possible parameters %s; evidence %s\n",
				i+1, c.Sessions, c.Executions, c.ID, trace.OneLine(strings.Join(c.Actions, " > "), 85),
				trace.OneLine(strings.Join(c.Parameters, ", "), 85), strings.Join(c.Evidence, ", "))
			if len(c.Cautions) > 0 {
				fmt.Fprintf(w, "       check %s\n", strings.Join(c.Cautions, ", "))
			}
		}
		return
	}
	if len(r.SpanProposals) > 0 && r.SpanProposals[0].Review.Source != "" {
		fmt.Fprintf(w, "\nTask-first queue: %d unassessed spans in %d groups; component queue: %d spans in %d groups (missing task contract); diagnostic inventory: %d spans in %d groups. Queue membership is not a useful-procedure verdict.\n", len(r.ReviewSpans), len(r.ReviewGroups), len(r.ComponentSpans), len(r.ComponentGroups), len(r.SpanProposals), len(r.CompositionGroups))
		fmt.Fprintln(w, "Brief an example with `tap discover brief --span <id> --report <file> --out <private-dir>`.")
		queue := r.ReviewGroups
		if len(queue) == 0 {
			queue = r.ComponentGroups
		}
		for i, g := range queue {
			if i == n {
				break
			}
			p := g.Example
			fmt.Fprintf(w, "  %3d. %3d sessions %4d proposals  %-13s %-7s calls %v  %s  %s\n",
				i+1, g.Sessions, g.Proposals, p.Kind, p.Effect, p.Calls, p.ID, trace.OneLine(strings.Join(p.Composition.Actions, " > "), 85))
		}
		return
	}
	if len(r.CompositionGroups) == 0 && len(r.SpanGroups) > 0 {
		fmt.Fprintf(w, "\nFound %d unassessed bounded-span proposals in %d exact-shape groups (older report). These have not passed the useful-procedure gate.\n", len(r.SpanProposals), len(r.SpanGroups))
		fmt.Fprintln(w, "Brief an example with `tap discover brief --span <id> --report <file> --out <private-dir>`.")
		for i, g := range r.SpanGroups {
			if i == n {
				break
			}
			p := g.Example
			fmt.Fprintf(w, "  %3d. %3d sessions %4d proposals  %-13s %-7s calls %v  %s  %s\n",
				i+1, g.Sessions, g.Proposals, p.Kind, p.Effect, p.Calls, p.ID, trace.OneLine(strings.Join(p.Tools, " > "), 85))
		}
		return
	}
	fmt.Fprintf(w, "\nFound %d unassessed bounded-span proposals in %d composition groups (%d exact-shape groups). These have not passed the useful-procedure gate.\n", len(r.SpanProposals), len(r.CompositionGroups), len(r.SpanGroups))
	fmt.Fprintln(w, "Brief an example with `tap discover brief --span <id> --report <file> --out <private-dir>`.")
	for i, g := range r.CompositionGroups {
		if i == n {
			break
		}
		p := g.Example
		fmt.Fprintf(w, "  %3d. %3d sessions %4d proposals  %-13s %-7s calls %v  %s  %s\n",
			i+1, g.Sessions, g.Proposals, p.Kind, p.Effect, p.Calls, p.ID, trace.OneLine(strings.Join(p.Composition.Actions, " > "), 85))
	}
}
