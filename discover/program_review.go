package discover

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"

	"gitlab.com/telara-labs/tap-runtime/discover/pack"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
)

const OriginDiscoverGenerated = "discover_generated"

//go:embed skill/tap-primitive-refine/SKILL.md
var generatedRefineSkill []byte

type generatedDecision struct {
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
	selectionDecision := programSelectionDecision(graph)
	digest := ""
	if pkg != nil {
		digest = pkg.Digest
	} else {
		digest = unresolvedGraphDigest(graph)
	}
	decisions, err := readGeneratedDecisions(stateDir)
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
	fmt.Fprintf(out, "Does: %s\n", programSummary(graph))
	fmt.Fprintf(out, "Evidence: %d disjoint execution(s) in %d session(s); %d source span(s), including overlaps\n", graph.Executions, graph.Sessions, len(graph.Sources))
	if len(evidence) > 0 {
		directSessions := map[string]bool{}
		causalSessions := map[string]bool{}
		repeated := 0
		for _, span := range evidence {
			if span.Review.Ready {
				directSessions[span.Client+"\x00"+span.Session] = true
			}
			if span.Review.Component || generatedCausalComponent(span) {
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
			fmt.Fprintf(out, "     %s <- %s%s\n", strings.Join(arg.Path, "."), programValueLabel(arg.Value), note)
		}
		for _, note := range programSharedResultBindings(step) {
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
		where, unchanged, err := saveGeneratedPackage(pkg, skillRoot)
		if err != nil {
			return err
		}
		if err := appendGeneratedDecision(stateDir, generatedDecision{graph.CandidateID, digest, "accept"}); err != nil {
			return fmt.Errorf("installed at %s but could not record acceptance: %w", where, err)
		}
		if unchanged {
			fmt.Fprintf(out, "Already accepted privately: %s\n", where)
		} else {
			fmt.Fprintf(out, "Accepted privately: %s\n", where)
		}
	case "d", "deny":
		if err := appendGeneratedDecision(stateDir, generatedDecision{graph.CandidateID, digest, "deny"}); err != nil {
			return err
		}
		fmt.Fprintln(out, "Denied locally. The unchanged proposal will not be shown again.")
	case "r", "refine":
		where, err := writeGeneratedHandoff(graph, pkg, stateDir, evidence)
		if err != nil {
			return err
		}
		if err := appendGeneratedDecision(stateDir, generatedDecision{graph.CandidateID, digest, "refine"}); err != nil {
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
func programSelectionDecision(g *codegen.ProgramGraph) string {
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
func programSharedResultBindings(step codegen.ProgramStep) []string {
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

func programSummary(g *codegen.ProgramGraph) string {
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

func programValueLabel(v codegen.ProgramValue) string {
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

func saveGeneratedPackage(p *codegen.GeneratedPackage, root string) (string, bool, error) {
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

func readGeneratedDecisions(dir string) ([]generatedDecision, error) {
	data, err := os.ReadFile(filepath.Join(dir, "decisions.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var decisions []generatedDecision
	if err := json.Unmarshal(data, &decisions); err != nil {
		return nil, fmt.Errorf("invalid local Discover decisions: %w", err)
	}
	return decisions, nil
}

func appendGeneratedDecision(dir string, decision generatedDecision) error {
	decisions, err := readGeneratedDecisions(dir)
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

func writeGeneratedHandoff(g *codegen.ProgramGraph, p *codegen.GeneratedPackage, stateDir string, evidence []model.SpanProposal) (string, error) {
	digest := unresolvedGraphDigest(g)
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
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), generatedRefineSkill, 0o600); err != nil {
		return "", err
	}
	handoff := "# TAP primitive refinement\n\nGive this folder to your chosen coding agent and ask it to use SKILL.md. EVIDENCE.json identifies the selected local requests and call spans; inspect the original client sessions when the graph leaves a decision open. The agent should return changed code and capability reach for a new user decision. No agent is invoked by Discover.\n\nCandidate: " + g.CandidateID + "\nDigest: " + digest + "\nSources: " + strings.Join(g.Sources, ", ") + "\n"
	if decision := programSelectionDecision(g); decision != "" {
		handoff += "\nUnresolved selection: " + decision + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "HANDOFF.md"), []byte(handoff), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

func unresolvedGraphDigest(g *codegen.ProgramGraph) string {
	data, _ := json.Marshal(g)
	sum := sha256.Sum256(data)
	return "unresolved-" + hex.EncodeToString(sum[:8])
}
