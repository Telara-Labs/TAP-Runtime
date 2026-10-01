package discover

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/pack"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// generateCommand is the model-free local path from observed call spans to an
// exact code review. It does not invoke a coding agent or upload transcripts.
func generateCommand(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover generate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	clients := fs.String("client", "claude-code,codex,cursor", "local clients to read")
	days := fs.Int("days", 0, "only sessions from the last N days")
	logicID := fs.String("logic", "", "logic candidate ID to generate and review")
	variantID := fs.String("variant", "", "invocation variant ID within the selected logic candidate")
	top := fs.Int("top", 25, "candidates to list when --logic is omitted")
	generatedOnly := fs.Bool("generated", false, "list one mechanically generated variant per logic family")
	saveClient := fs.String("save-client", "claude-code", "private skill destination: claude-code or codex")
	saveProject := fs.Bool("save-project", false, "install privately in this project's skill folder")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *top < 0 || (*generatedOnly && *logicID != "") {
		fmt.Fprintln(errOut, "discover generate: --top must be nonnegative and --generated is a listing option")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(errOut, "discover generate:", err)
		return 1
	}
	readers, err := history.DefaultReaders(strings.Split(*clients, ","), home)
	if err != nil {
		fmt.Fprintln(errOut, "discover generate:", err)
		return 2
	}
	var since time.Time
	if *days > 0 {
		since = time.Now().AddDate(0, 0, -*days)
	}
	var sessions []trace.Session
	for _, reader := range readers {
		ss, err := reader.Read(since)
		if err != nil {
			fmt.Fprintf(errOut, "discover generate: %s: %v\n", reader.Client(), err)
			return 1
		}
		sessions = append(sessions, ss...)
	}
	spans := SelectSpanProposals(sessions)
	candidates := GroupLogicCandidates(spans)
	if *logicID == "" {
		if *generatedOnly {
			configRoot, err := os.UserConfigDir()
			if err != nil {
				fmt.Fprintln(errOut, "discover generate:", err)
				return 1
			}
			decisions, err := readGeneratedDecisions(filepath.Join(configRoot, "tap-runtime", "discover"))
			if err != nil {
				fmt.Fprintln(errOut, "discover generate:", err)
				return 1
			}
			rows, err := generatedProgramQueue(candidates, spans, sessions, decisions...)
			if err != nil {
				fmt.Fprintln(errOut, "discover generate:", err)
				return 1
			}
			n := *top
			if n == 0 || n > len(rows) {
				n = len(rows)
			}
			fmt.Fprintf(out, "%d unreviewed broad logic families have a mechanically generated variant from %d local sessions; generation is not a usefulness or validation score.\n", len(rows), len(sessions))
			for i, row := range rows[:n] {
				fmt.Fprintf(out, "%d. %s / %s  %d executions/%d sessions  %s  [%s]\n", i+1, row.candidate.ID, row.variant.ID,
					row.variant.Executions, row.variant.Sessions, strings.Join(row.candidate.Actions, " -> "), row.shape)
			}
			fmt.Fprintln(out, "Review exact code and effects with: tap discover generate --logic <candidate-id> --variant <variant-id>")
			return 0
		}
		if *top == 0 || *top > len(candidates) {
			*top = len(candidates)
		}
		inlineFamilies := 0
		for _, c := range candidates {
			if strings.HasPrefix(c.Key, "inline_python_family=") {
				inlineFamilies++
			}
		}
		fmt.Fprintf(out, "%d recurring logic candidates from %d local sessions (%d inline code families); these are retrieval proposals, not validated primitives.\n", len(candidates), len(sessions), inlineFamilies)
		for i, c := range candidates[:*top] {
			status := ""
			if strings.HasPrefix(c.Key, "inline_python_family=") {
				status = "  [structural code family; internal data flow and effects unproven]"
			}
			fmt.Fprintf(out, "%d. %s  %d executions/%d sessions  %s%s\n", i+1, c.ID, c.Executions, c.Sessions, strings.Join(c.Actions, " -> "), status)
		}
		fmt.Fprintln(out, "Attempt exact program review with: tap discover generate --logic <candidate-id>. Inline code is compilable only for a strictly parsed same-file read/replace/write pattern; other code remains a retrieval lead.")
		return 0
	}
	var selected *model.LogicCandidate
	for i := range candidates {
		if candidates[i].ID == *logicID {
			selected = &candidates[i]
			break
		}
	}
	if selected == nil {
		fmt.Fprintf(errOut, "discover generate: logic candidate %s was not found in current local history\n", *logicID)
		return 1
	}
	bySpan := make(map[string]model.SpanProposal, len(spans))
	for _, p := range spans {
		bySpan[p.ID] = p
	}
	qualified, ok := generatedCandidateTaskEvidence(*selected, bySpan)
	if !ok {
		fmt.Fprintf(errOut, "discover generate: logic candidate %s has no independent task-shaped evidence for generated review\n", *logicID)
		return 1
	}
	variants, err := GroupProgramVariants(qualified, spans, sessions)
	if err != nil {
		fmt.Fprintln(errOut, "discover generate:", err)
		return 1
	}
	if *variantID == "" && len(variants) > 1 {
		fmt.Fprintf(out, "%s has %d compatible program variants. Select one; variable values do not split a variant, and optional arguments merge only when a complete program can be generated.\n", selected.ID, len(variants))
		for i, v := range variants {
			fmt.Fprintf(out, "%d. %s  %d executions/%d sessions  %s\n", i+1, v.ID, v.Executions, v.Sessions, v.Key)
		}
		fmt.Fprintf(out, "Review one with: tap discover generate --logic %s --variant <variant-id>\n", selected.ID)
		return 0
	}
	var chosen *model.LogicCandidate
	if *variantID == "" && len(variants) == 1 {
		chosen = &variants[0]
	} else {
		for i := range variants {
			if variants[i].ID == *variantID {
				chosen = &variants[i]
				break
			}
		}
	}
	if chosen == nil {
		fmt.Fprintf(errOut, "discover generate: invocation variant %s was not found under %s\n", *variantID, selected.ID)
		return 1
	}
	graph, err := SynthesizeProgramGraph(*chosen, spans, sessions)
	if err != nil {
		fmt.Fprintln(errOut, "discover generate:", err)
		return 1
	}
	cwd, _ := os.Getwd()
	skillRoot, err := pack.SkillsDir(*saveClient, *saveProject, home, cwd)
	if err != nil {
		fmt.Fprintln(errOut, "discover generate:", err)
		return 2
	}
	configRoot, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintln(errOut, "discover generate:", err)
		return 1
	}
	stateDir := filepath.Join(configRoot, "tap-runtime", "discover")
	var evidence []model.SpanProposal
	for _, id := range chosen.Members {
		if p, ok := bySpan[id]; ok {
			evidence = append(evidence, p)
		}
	}
	if err := ReviewGenerated(in, out, graph, skillRoot, stateDir, evidence...); err != nil {
		fmt.Fprintln(errOut, "discover generate:", err)
		return 1
	}
	return 0
}

type generatedProgramRow struct {
	candidate model.LogicCandidate
	variant   model.LogicCandidate
	tier      int
	shape     string
}

// This is only a review order. A result-dependent terminal write is more
// likely to represent completed reusable work than a follow-up read, but
// neither category establishes task usefulness or safe deployment.
func programReviewShape(g *ProgramGraph) (int, string) {
	if g == nil || len(g.Steps) == 0 {
		return 0, "unresolved shape"
	}
	if programSelectionDecision(g) != "" {
		return -1, "compiled lookup; selection rule unknown"
	}
	last := g.Steps[len(g.Steps)-1]
	dependent := last.LoopResultStep > 0
	for _, arg := range last.Args {
		switch arg.Value.Kind {
		case "result", "indexed_result", "collection_index", "collection_index_item", "item_result", "selected_result":
			dependent = true
		}
	}
	if last.Effect == "write" {
		if dependent {
			return 3, "result-dependent write"
		}
		return 2, "write sequence"
	}
	if dependent {
		return 1, "result-dependent read"
	}
	return 0, "read sequence"
}

// generatedProgramQueue shows at most one implementation per broad logic
// family. Different argument contracts may still need separate review; this
// queue chooses the best-supported compilable variant without counting every
// shape as a new primitive or claiming any family is useful.
func generatedProgramQueue(candidates []model.LogicCandidate, spans []model.SpanProposal, sessions []trace.Session, decisions ...generatedDecision) ([]generatedProgramRow, error) {
	reviewed := make(map[string]bool, len(decisions))
	bySpan := make(map[string]model.SpanProposal, len(spans))
	for _, span := range spans {
		bySpan[span.ID] = span
	}
	for _, d := range decisions {
		reviewed[d.Candidate+"\x00"+d.Digest] = true
	}
	var rows []generatedProgramRow
	for _, c := range candidates {
		qualified, ok := generatedCandidateTaskEvidence(c, bySpan)
		if !ok {
			continue
		}
		variants, err := GroupProgramVariants(qualified, spans, sessions)
		if err != nil {
			return nil, err
		}
		for _, v := range variants {
			if !generatedVariantHasTaskEvidence(v, bySpan) {
				continue
			}
			graph, err := SynthesizeProgramGraph(v, spans, sessions)
			if err != nil {
				return nil, err
			}
			if len(graph.Problems) != 0 {
				continue
			}
			pkg, err := GenerateProgramPackage(graph)
			if err != nil {
				continue
			}
			if reviewed[graph.CandidateID+"\x00"+pkg.Digest] {
				continue
			}
			tier, shape := programReviewShape(graph)
			if generatedVariantIsInternalComponent(v, bySpan) {
				shape = "agent component; " + shape
			}
			rows = append(rows, generatedProgramRow{candidate: qualified, variant: v, tier: tier, shape: shape})
			break
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].variant, rows[j].variant
		if rows[i].tier != rows[j].tier {
			return rows[i].tier > rows[j].tier
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		if a.Executions != b.Executions {
			return a.Executions > b.Executions
		}
		return rows[i].candidate.ID < rows[j].candidate.ID
	})
	return rows, nil
}

// A direct task-shaped span can stand on its own. A bounded causal component
// inside a larger task needs support from another session; it is presented as
// an agent component, not as the completed user task. Repeated adjacency alone
// is never sufficient. Exact graph synthesis and codegen remain independent
// gates after this coarse evidence filter.
func generatedCandidateTaskEvidence(c model.LogicCandidate, bySpan map[string]model.SpanProposal) (model.LogicCandidate, bool) {
	components := map[string]bool{}
	causalSessions := map[string]bool{}
	inlineSessions := map[string]bool{}
	inlineFamily := strings.HasPrefix(c.Key, "inline_python_family=") && strings.HasSuffix(c.Key, ":open>read>replace>open>write")
	for _, id := range c.Members {
		p, ok := bySpan[id]
		if ok && inlineFamily && p.CodeShape != "" && (p.Review.Source == "user" || p.Review.Source == "scheduled") {
			inlineSessions[p.Client+"\x00"+p.Session] = true
		}
		if ok && p.Review.Component && (p.Review.Source == "user" || p.Review.Source == "scheduled") {
			components[p.Client+"\x00"+p.Session] = true
		}
		if ok && generatedCausalComponent(p) {
			causalSessions[p.Client+"\x00"+p.Session] = true
		}
	}
	qualified := c
	qualified.Members = nil
	qualified.Example = model.SpanProposal{}
	qualified.Proposals, qualified.Executions, qualified.Sessions = 0, 0, 0
	for _, id := range c.Members {
		p, ok := bySpan[id]
		if !ok || (p.Review.Source != "user" && p.Review.Source != "scheduled") {
			continue
		}
		if !p.Review.Ready && !(p.Review.Component && len(components) >= 2) && !(generatedCausalComponent(p) && len(causalSessions) >= 2) && !(inlineFamily && p.CodeShape != "" && len(inlineSessions) >= 2) {
			continue
		}
		qualified.Members = append(qualified.Members, id)
		if qualified.Example.ID == "" || p.EvidenceScore > qualified.Example.EvidenceScore {
			qualified.Example = p
		}
	}
	if len(qualified.Members) == 0 {
		return model.LogicCandidate{}, false
	}
	qualified.Proposals = len(qualified.Members)
	qualified.Executions = variantIndependentExecutions(qualified.Members, bySpan)
	sourceSessions := map[string]bool{}
	for _, id := range qualified.Members {
		p := bySpan[id]
		sourceSessions[p.Client+"\x00"+p.Session] = true
	}
	qualified.Sessions = len(sourceSessions)
	return qualified, true
}

func generatedCausalComponent(p model.SpanProposal) bool {
	if p.Kind != "result_chain" || len(p.Calls) < 2 || len(p.Composition.Edges) == 0 ||
		p.Review.Source != "user" && p.Review.Source != "scheduled" {
		return false
	}
	for _, reason := range []string{"failed_or_oversized_call", "output_not_observed", "stop_condition_unproven", "synthetic_request"} {
		if spanHasReason(p.Review.Reasons, reason) {
			return false
		}
	}
	return true
}

func generatedVariantIsInternalComponent(v model.LogicCandidate, bySpan map[string]model.SpanProposal) bool {
	if len(v.Members) == 0 {
		return false
	}
	causal, direct := false, false
	for _, id := range v.Members {
		p := bySpan[id]
		causal = causal || generatedCausalComponent(p)
		direct = direct || p.Review.Ready
	}
	return causal && !direct
}

// Recurring order with a shared resource is a retrieval clue, not proof that
// the two adjacent operations are one user task. Keep such variants available
// for direct inspection, but do not push them into the generated review queue
// unless independent executions establish a task-shaped contract.
func generatedVariantHasTaskEvidence(v model.LogicCandidate, bySpan map[string]model.SpanProposal) bool {
	allRepeatedOrder := len(v.Members) > 0
	readySessions := map[string]bool{}
	sourceSessions := map[string]bool{}
	for _, id := range v.Members {
		span, ok := bySpan[id]
		if !ok {
			return false
		}
		sourceSessions[span.Client+"\x00"+span.Session] = true
		if span.Kind != "repeated_order" {
			allRepeatedOrder = false
		}
		if span.Review.Ready || span.Review.Component {
			readySessions[span.Client+"\x00"+span.Session] = true
		}
	}
	if len(sourceSessions) >= 2 && len(v.Members) > 0 && bySpan[v.Members[0]].CodeShape != "" {
		return true // strict AST compiler still has to prove every source body
	}
	if len(sourceSessions) < 2 && len(readySessions) == 0 {
		return false
	}
	return !allRepeatedOrder || len(readySessions) >= 2
}
