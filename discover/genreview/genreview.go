// Package genreview shows a generated program in full and takes one local decision:
// accept, deny or refine it, and runs the model-free generate command.
package genreview

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"
	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// GenerateCommand is the model-free local path from observed call spans to an
// exact code review. It does not invoke a coding agent or upload transcripts.
func GenerateCommand(args []string, in io.Reader, out, errOut io.Writer) int {
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
	spans := retrieval.SelectSpanProposals(sessions)
	candidates := retrieval.GroupLogicCandidates(spans)
	if *logicID == "" {
		if *generatedOnly {
			configRoot, err := os.UserConfigDir()
			if err != nil {
				fmt.Fprintln(errOut, "discover generate:", err)
				return 1
			}
			decisions, err := ReadGeneratedDecisions(filepath.Join(configRoot, "tap-runtime", "discover"))
			if err != nil {
				fmt.Fprintln(errOut, "discover generate:", err)
				return 1
			}
			rows, err := GeneratedProgramQueue(candidates, spans, sessions, decisions...)
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
				fmt.Fprintf(out, "%d. %s / %s  %d executions/%d sessions  %s  [%s]\n", i+1, row.Candidate.ID, row.Variant.ID,
					row.Variant.Executions, row.Variant.Sessions, strings.Join(row.Candidate.Actions, " -> "), row.Shape)
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
	qualified, ok := GeneratedCandidateTaskEvidence(*selected, bySpan)
	if !ok {
		fmt.Fprintf(errOut, "discover generate: logic candidate %s has no independent task-shaped evidence for generated review\n", *logicID)
		return 1
	}
	variants, err := codegen.GroupProgramVariants(qualified, spans, sessions)
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
	graph, err := codegen.SynthesizeProgramGraph(*chosen, spans, sessions)
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

type GeneratedProgramRow struct {
	Candidate model.LogicCandidate `json:"-"`
	Variant   model.LogicCandidate `json:"-"`
	Tier      int                  `json:"-"`
	Shape     string               `json:"-"`
}

// This is only a review order. A result-dependent terminal write is more
// likely to represent completed reusable work than a follow-up read, but
// neither category establishes task usefulness or safe deployment.
func ProgramReviewShape(g *codegen.ProgramGraph) (int, string) {
	if g == nil || len(g.Steps) == 0 {
		return 0, "unresolved shape"
	}
	if ProgramSelectionDecision(g) != "" {
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

// GeneratedProgramQueue shows at most one implementation per broad logic
// family. Different argument contracts may still need separate review; this
// queue chooses the best-supported compilable variant without counting every
// shape as a new primitive or claiming any family is useful.
func GeneratedProgramQueue(candidates []model.LogicCandidate, spans []model.SpanProposal, sessions []trace.Session, decisions ...GeneratedDecision) ([]GeneratedProgramRow, error) {
	reviewed := make(map[string]bool, len(decisions))
	bySpan := make(map[string]model.SpanProposal, len(spans))
	for _, span := range spans {
		bySpan[span.ID] = span
	}
	for _, d := range decisions {
		reviewed[d.Candidate+"\x00"+d.Digest] = true
	}
	var rows []GeneratedProgramRow
	for _, c := range candidates {
		qualified, ok := GeneratedCandidateTaskEvidence(c, bySpan)
		if !ok {
			continue
		}
		variants, err := codegen.GroupProgramVariants(qualified, spans, sessions)
		if err != nil {
			return nil, err
		}
		for _, v := range variants {
			if !GeneratedVariantHasTaskEvidence(v, bySpan) {
				continue
			}
			graph, err := codegen.SynthesizeProgramGraph(v, spans, sessions)
			if err != nil {
				return nil, err
			}
			if len(graph.Problems) != 0 {
				continue
			}
			pkg, err := codegen.GenerateProgramPackage(graph)
			if err != nil {
				continue
			}
			if reviewed[graph.CandidateID+"\x00"+pkg.Digest] {
				continue
			}
			tier, shape := ProgramReviewShape(graph)
			if GeneratedVariantIsInternalComponent(v, bySpan) {
				shape = "agent component; " + shape
			}
			rows = append(rows, GeneratedProgramRow{Candidate: qualified, Variant: v, Tier: tier, Shape: shape})
			break
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].Variant, rows[j].Variant
		if rows[i].Tier != rows[j].Tier {
			return rows[i].Tier > rows[j].Tier
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		if a.Executions != b.Executions {
			return a.Executions > b.Executions
		}
		return rows[i].Candidate.ID < rows[j].Candidate.ID
	})
	return rows, nil
}

// A direct task-shaped span can stand on its own. A bounded causal component
// inside a larger task needs support from another session; it is presented as
// an agent component, not as the completed user task. Repeated adjacency alone
// is never sufficient. Exact graph synthesis and codegen remain independent
// gates after this coarse evidence filter.
func GeneratedCandidateTaskEvidence(c model.LogicCandidate, bySpan map[string]model.SpanProposal) (model.LogicCandidate, bool) {
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
		if ok && GeneratedCausalComponent(p) {
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
		if !p.Review.Ready && !(p.Review.Component && len(components) >= 2) && !(GeneratedCausalComponent(p) && len(causalSessions) >= 2) && !(inlineFamily && p.CodeShape != "" && len(inlineSessions) >= 2) {
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
	qualified.Executions = codegen.VariantIndependentExecutions(qualified.Members, bySpan)
	sourceSessions := map[string]bool{}
	for _, id := range qualified.Members {
		p := bySpan[id]
		sourceSessions[p.Client+"\x00"+p.Session] = true
	}
	qualified.Sessions = len(sourceSessions)
	return qualified, true
}

func GeneratedCausalComponent(p model.SpanProposal) bool {
	if p.Kind != "result_chain" || len(p.Calls) < 2 || len(p.Composition.Edges) == 0 ||
		p.Review.Source != "user" && p.Review.Source != "scheduled" {
		return false
	}
	for _, reason := range []string{"failed_or_oversized_call", "output_not_observed", "stop_condition_unproven", "synthetic_request"} {
		if retrieval.SpanHasReason(p.Review.Reasons, reason) {
			return false
		}
	}
	return true
}

func GeneratedVariantIsInternalComponent(v model.LogicCandidate, bySpan map[string]model.SpanProposal) bool {
	if len(v.Members) == 0 {
		return false
	}
	causal, direct := false, false
	for _, id := range v.Members {
		p := bySpan[id]
		causal = causal || GeneratedCausalComponent(p)
		direct = direct || p.Review.Ready
	}
	return causal && !direct
}

// Recurring order with a shared resource is a retrieval clue, not proof that
// the two adjacent operations are one user task. Keep such variants available
// for direct inspection, but do not push them into the generated review queue
// unless independent executions establish a task-shaped contract.
func GeneratedVariantHasTaskEvidence(v model.LogicCandidate, bySpan map[string]model.SpanProposal) bool {
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

const OriginDiscoverGenerated = "discover_generated"

//go:embed skill/tap-primitive-refine/SKILL.md
var GeneratedRefineSkill []byte

type GeneratedDecision struct {
	Candidate string `json:"candidate"`
	Digest    string `json:"digest"`
	Choice    string `json:"choice"`
}

// ReviewGenerated shows the entire exact package before taking one local
// decision. Accept installs privately; Deny remembers this candidate/digest;
// Refine makes an inspectable local handoff for the user's chosen coding agent.
func ReviewGenerated(in io.Reader, out io.Writer, graph *codegen.ProgramGraph, skillRoot, stateDir string, evidence ...model.SpanProposal) error {
	if graph == nil {
		return fmt.Errorf("no program graph")
	}
	pkg, generateErr := codegen.GenerateProgramPackage(graph)
	selectionDecision := ProgramSelectionDecision(graph)
	digest := ""
	if pkg != nil {
		digest = pkg.Digest
	} else {
		digest = UnresolvedGraphDigest(graph)
	}
	decisions, err := ReadGeneratedDecisions(stateDir)
	if err != nil {
		return err
	}
	for _, d := range decisions {
		if d.Candidate == graph.CandidateID && d.Digest == digest && d.Choice == "deny" {
			fmt.Fprintf(out, "Candidate %s at digest %s was denied locally. Changed evidence or code produces a new digest.\n", graph.CandidateID, digest)
			return nil
		}
	}
	fmt.Fprintf(out, "Discover proposal %s\n", graph.CandidateID)
	fmt.Fprintf(out, "Does: %s\n", ProgramSummary(graph))
	fmt.Fprintf(out, "Evidence: %d disjoint execution(s) in %d session(s); %d source span(s), including overlaps\n", graph.Executions, graph.Sessions, len(graph.Sources))
	if len(evidence) > 0 {
		directSessions := map[string]bool{}
		causalSessions := map[string]bool{}
		repeated := 0
		for _, span := range evidence {
			if span.Review.Ready {
				directSessions[span.Client+"\x00"+span.Session] = true
			}
			if span.Review.Component || GeneratedCausalComponent(span) {
				causalSessions[span.Client+"\x00"+span.Session] = true
			}
			if span.Kind == "repeated_order" {
				repeated++
			}
		}
		if graph.InlineFileReplace != nil {
			fmt.Fprintf(out, "Code evidence: %d independent session(s) passed the strict same-file AST pattern. This proves the inner transform shape, not user usefulness.\n", graph.Sessions)
			fmt.Fprintln(out, "Usefulness: this is a generic text replacement; source recurrence alone does not show it saves agent work or completes the user's larger task.")
		} else {
			fmt.Fprintf(out, "Task evidence: %d direct user-task session(s); %d causal component session(s); %d span(s) rely on repeated order and a shared resource.\n", len(directSessions), len(causalSessions), repeated)
			if len(directSessions) == 0 && len(causalSessions) >= 2 {
				fmt.Fprintf(out, "Component evidence: %d independent session(s) repeated this result-linked call chain. It may be an agent routine inside a larger user task; source recurrence does not prove task completion or usefulness.\n", len(causalSessions))
			}
			if repeated > 0 && len(directSessions) < 2 {
				fmt.Fprintln(out, "Caution: repeated order is a retrieval clue, not proof these calls form one useful task.")
			}
		}
	}
	fmt.Fprintln(out, "Inputs:")
	for _, input := range graph.Inputs {
		typ := input.Type
		if input.List {
			typ = "list<" + typ + ">"
		}
		if input.Optional {
			typ += " (optional; call omits it when absent)"
		}
		fmt.Fprintf(out, "  %s: %s <- %s\n", input.Name, typ, input.Source)
		for _, field := range input.Fields {
			fmt.Fprintf(out, "    %s: %s -> %s\n", field.Name, field.Type, strings.Join(field.Path, "."))
		}
	}
	fmt.Fprintln(out, "Ordered calls and effects:")
	for i, step := range graph.Steps {
		loop := ""
		if step.Loop != "" {
			loop = " for each " + step.Loop
		} else if step.LoopResultStep != 0 {
			loop = fmt.Sprintf(" for each item in step %d result%s", step.LoopResultStep, step.LoopResultPath)
		}
		binding := "unresolved binding"
		if step.Binding != nil {
			binding = step.Binding.Server + "/" + step.Binding.Tool
		} else if len(step.Pipeline) > 0 {
			var commands []string
			for _, command := range step.Pipeline {
				commands = append(commands, command.Name+" ("+command.Effect+")")
			}
			separator, kind := " | ", "pipeline "
			if len(step.Pipeline) > 1 && step.Pipeline[1].Connector == "and" {
				separator, kind = " && ", "success chain "
			}
			binding = kind + strings.Join(commands, separator)
		} else if step.Command != "" {
			binding = "command " + step.Command
		} else if step.Tool == "tap.read" || step.Tool == "tap.write" || step.Tool == "python.str.replace" {
			binding = step.Tool
		}
		fmt.Fprintf(out, "  %d. %s via %s (%s)%s\n", i+1, step.Role, binding, step.Effect, loop)
		for _, arg := range step.Args {
			note := ""
			if arg.Optional {
				note = " (optional)"
			}
			fmt.Fprintf(out, "     %s <- %s%s\n", strings.Join(arg.Path, "."), ProgramValueLabel(arg.Value), note)
		}
		for _, note := range ProgramSharedResultBindings(step) {
			fmt.Fprintf(out, "     Shared result source: %s\n", note)
		}
		for _, pair := range step.DistinctResultInputs {
			if len(pair) == 2 {
				fmt.Fprintf(out, "     Result index inputs %s and %s must select different items.\n", pair[0], pair[1])
			}
		}
		if step.DistinctLoopSelections {
			fmt.Fprintf(out, "     Selected positions in %s must be distinct.\n", step.Loop)
		}
		if len(step.OptionalProfiles) > 0 {
			var profiles []string
			for _, profile := range step.OptionalProfiles {
				if len(profile) == 0 {
					profiles = append(profiles, "none")
				} else {
					profiles = append(profiles, strings.Join(profile, " + "))
				}
			}
			fmt.Fprintf(out, "     Observed optional combinations: %s\n", strings.Join(profiles, "; "))
		}
	}
	if graph.InlineFileReplace != nil {
		fmt.Fprintln(out, "File reach: read and write anywhere under the invocation working directory (manifest files: .); every write requires runtime approval.")
		if graph.InlineFileReplace.Embedded {
			fmt.Fprintln(out, "Scope: only the inner file transform; surrounding shell commands are excluded.")
		}
		fmt.Fprintln(out, "Output: file path and replacement count.")
	} else {
		fmt.Fprintln(out, "Output: a JSON object with each step result under step_N; loop results are lists.")
	}
	if generateErr != nil {
		fmt.Fprintf(out, "Needs decision: %v\n", generateErr)
	} else {
		fmt.Fprintf(out, "Exact package digest: %s\n", digest)
		for _, name := range []string{"primitive.yaml", "main.py", "README.md"} {
			fmt.Fprintf(out, "\n--- %s ---\n%s\n", name, pkg.Files[name])
		}
	}
	if selectionDecision != "" {
		fmt.Fprintf(out, "Needs decision: %s\n", selectionDecision)
	}
	if pkg == nil || selectionDecision != "" {
		fmt.Fprint(out, "\nChoose [d]eny, [r]efine with my coding agent, or [q]uit > ")
	} else {
		fmt.Fprint(out, "\nChoose [a]ccept privately, [d]eny, [r]efine with my coding agent, or [q]uit > ")
	}
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return sc.Err()
	}
	choice := strings.ToLower(strings.TrimSpace(sc.Text()))
	switch choice {
	case "a", "accept":
		if pkg == nil {
			return fmt.Errorf("cannot accept an unresolved program")
		}
		if selectionDecision != "" {
			return fmt.Errorf("cannot accept a task with an undetermined result selection: %s", selectionDecision)
		}
		where, unchanged, err := SaveGeneratedPackage(pkg, skillRoot)
		if err != nil {
			return err
		}
		if err := AppendGeneratedDecision(stateDir, GeneratedDecision{graph.CandidateID, digest, "accept"}); err != nil {
			return fmt.Errorf("installed at %s but could not record acceptance: %w", where, err)
		}
		if unchanged {
			fmt.Fprintf(out, "Already accepted privately: %s\n", where)
		} else {
			fmt.Fprintf(out, "Accepted privately: %s\n", where)
		}
	case "d", "deny":
		if err := AppendGeneratedDecision(stateDir, GeneratedDecision{graph.CandidateID, digest, "deny"}); err != nil {
			return err
		}
		fmt.Fprintln(out, "Denied locally. The unchanged proposal will not be shown again.")
	case "r", "refine":
		where, err := WriteGeneratedHandoff(graph, pkg, stateDir, evidence)
		if err != nil {
			return err
		}
		if err := AppendGeneratedDecision(stateDir, GeneratedDecision{graph.CandidateID, digest, "refine"}); err != nil {
			return err
		}
		fmt.Fprintf(out, "Local agent handoff: %s\n", where)
	case "q", "quit", "":
		fmt.Fprintln(out, "No decision recorded.")
	default:
		return fmt.Errorf("unknown decision %q", choice)
	}
	return nil
}

// An index into a list returned by this same program is executable, but the
// caller cannot select the desired item by inspecting that future list before
// invocation. It is a useful lower-level composition to inspect or refine,
// not evidence that Discover inferred the selection rule of the source task.
func ProgramSelectionDecision(g *codegen.ProgramGraph) string {
	if g == nil {
		return ""
	}
	for i, step := range g.Steps {
		for _, arg := range step.Args {
			if arg.Value.Kind == "collection_index" || arg.Value.Kind == "collection_index_item" {
				return fmt.Sprintf("step %d selects from step %d result%s by caller-supplied position; source calls prove list membership, not why the item was chosen. Supply an evidenced rule or refine this as an explicitly scoped lookup", i+1, arg.Value.Step, arg.Value.CollectionPath)
			}
		}
	}
	return ""
}

// Equal result bindings in distinct argument paths can be intentional, but
// the review must make the alias visible before a user accepts the package.
func ProgramSharedResultBindings(step codegen.ProgramStep) []string {
	pathsBySource := map[string][]string{}
	for _, arg := range step.Args {
		v := arg.Value
		if v.Kind != "result" && v.Kind != "item_result" && v.Kind != "selected_result" {
			continue
		}
		source := fmt.Sprintf("%s from step %d at %s", v.Kind, v.Step, v.ResultPath)
		pathsBySource[source] = append(pathsBySource[source], strings.Join(arg.Path, "."))
	}
	var notes []string
	for source, paths := range pathsBySource {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		notes = append(notes, source+" supplies "+strings.Join(paths, " and "))
	}
	sort.Strings(notes)
	return notes
}

func ProgramSummary(g *codegen.ProgramGraph) string {
	if len(g.Steps) == 0 {
		return "no executable steps determined"
	}
	parts := make([]string, 0, len(g.Steps))
	for _, step := range g.Steps {
		name := strings.NewReplacer("_", " ", ".", " ").Replace(step.Role)
		if step.Loop != "" {
			name = "for each " + step.Loop + ", " + name
		} else if step.LoopResultStep != 0 {
			name = fmt.Sprintf("for each item in step %d result%s, %s", step.LoopResultStep, step.LoopResultPath, name)
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, "; then ")
}

func ProgramValueLabel(v codegen.ProgramValue) string {
	switch v.Kind {
	case "input", "item":
		return v.Kind + " " + v.Input + v.ResultPath
	case "item_result":
		return "current result item" + v.ResultPath
	case "selected_result":
		return fmt.Sprintf("step %d result%s unique item where %s = input %s, then %s", v.Step, v.CollectionPath, v.PredicatePath, v.Input, v.ResultPath)
	case "indexed_result":
		return fmt.Sprintf("step %d result at zero-based index input %s, then %s", v.Step, v.Input, v.ResultPath)
	case "collection_index":
		return fmt.Sprintf("step %d result%s at zero-based index input %s, then item%s", v.Step, v.CollectionPath, v.Input, v.ResultPath)
	case "collection_index_item":
		return fmt.Sprintf("step %d result%s at each zero-based index in input %s, then item%s", v.Step, v.CollectionPath, v.Input, v.ResultPath)
	case "result":
		return fmt.Sprintf("step %d result%s", v.Step, v.ResultPath)
	case "selector":
		return "operation selector " + v.Selector
	default:
		return "unresolved"
	}
}

func SaveGeneratedPackage(p *codegen.GeneratedPackage, root string) (string, bool, error) {
	if p == nil || p.Graph == nil {
		return "", false, fmt.Errorf("no generated package")
	}
	name := p.Manifest.Metadata.Name
	archive, digest, err := pack.PackFiles(p.Files, func(string) bool { return false })
	if err != nil {
		return "", false, err
	}
	if digest != p.Digest {
		return "", false, fmt.Errorf("package changed after review")
	}
	marker := pack.Marker{Name: p.Manifest.Metadata.Publisher + "/" + name, Digest: digest,
		Validation: model.ValidationNotRun, Origin: OriginDiscoverGenerated}
	desc, _ := json.Marshal("Locally generated TAP program; review its calls and effects before each run")
	skill := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n# %s\n\nGenerated locally by TAP Discover. Package digest: %s.\n\nRead README.md, primitive.yaml and main.py before use. Run this folder through tap_run with one JSON object argument. The TAP runner checks declared tools and commands and gates effects.\n", name, desc, name, digest)
	return pack.Install(root, name, archive, marker, skill)
}

func ReadGeneratedDecisions(dir string) ([]GeneratedDecision, error) {
	data, err := os.ReadFile(filepath.Join(dir, "decisions.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var decisions []GeneratedDecision
	if err := json.Unmarshal(data, &decisions); err != nil {
		return nil, fmt.Errorf("invalid local Discover decisions: %w", err)
	}
	return decisions, nil
}

func AppendGeneratedDecision(dir string, decision GeneratedDecision) error {
	decisions, err := ReadGeneratedDecisions(dir)
	if err != nil {
		return err
	}
	for _, d := range decisions {
		if d == decision {
			return nil
		}
	}
	decisions = append(decisions, decision)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(decisions, "", "  ")
	temp, err := os.CreateTemp(dir, ".decisions-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(dir, "decisions.json"))
}

func WriteGeneratedHandoff(g *codegen.ProgramGraph, p *codegen.GeneratedPackage, stateDir string, evidence []model.SpanProposal) (string, error) {
	digest := UnresolvedGraphDigest(g)
	if p != nil {
		digest = p.Digest
	}
	dir := filepath.Join(stateDir, "handoffs", g.CandidateID+"-"+digest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	graphJSON, _ := json.MarshalIndent(g, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "program-graph.json"), append(graphJSON, '\n'), 0o600); err != nil {
		return "", err
	}
	evidenceJSON, _ := json.MarshalIndent(evidence, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "EVIDENCE.json"), append(evidenceJSON, '\n'), 0o600); err != nil {
		return "", err
	}
	if p != nil {
		for name, data := range p.Files {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				return "", err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), GeneratedRefineSkill, 0o600); err != nil {
		return "", err
	}
	handoff := "# TAP primitive refinement\n\nGive this folder to your chosen coding agent and ask it to use SKILL.md. EVIDENCE.json identifies the selected local requests and call spans; inspect the original client sessions when the graph leaves a decision open. The agent should return changed code and capability reach for a new user decision. No agent is invoked by Discover.\n\nCandidate: " + g.CandidateID + "\nDigest: " + digest + "\nSources: " + strings.Join(g.Sources, ", ") + "\n"
	if decision := ProgramSelectionDecision(g); decision != "" {
		handoff += "\nUnresolved selection: " + decision + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "HANDOFF.md"), []byte(handoff), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

func UnresolvedGraphDigest(g *codegen.ProgramGraph) string {
	data, _ := json.Marshal(g)
	sum := sha256.Sum256(data)
	return "unresolved-" + hex.EncodeToString(sum[:8])
}
