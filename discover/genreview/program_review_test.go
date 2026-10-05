package genreview_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/genreview"

	"github.com/Telara-Labs/TAP-Runtime/discover/codegen"

	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

func reviewableGraph() *codegen.ProgramGraph {
	return &codegen.ProgramGraph{
		CandidateID: "lc_review",
		Sources:     []string{"src_a", "src_b"},
		Inputs:      []codegen.ProgramInput{{Name: "issue_key", Type: "string", Source: "caller"}},
		Steps: []codegen.ProgramStep{{
			Role: "get_issue", Tool: "mcp:telara_jira_get_issue",
			Binding: &codegen.ProgramToolBinding{Server: "telara", Tool: "jira_get_issue"},
			Effect:  "read", Args: []codegen.ProgramArg{
				{Path: []string{"issue_key"}, Value: codegen.ProgramValue{Kind: "input", Input: "issue_key"}},
			},
		}},
	}
}

func TestReviewGeneratedAcceptShowsExactPackageThenInstalls(t *testing.T) {
	graph := reviewableGraph()
	root, state := t.TempDir(), t.TempDir()
	pkg, err := codegen.GenerateProgramPackage(graph)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := genreview.ReviewGenerated(strings.NewReader("accept\n"), &out, graph, root, state); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"Does: get issue", "issue_key: string", "telara/jira_get_issue", "read", pkg.Digest,
		string(pkg.Files["main.py"]), string(pkg.Files["primitive.yaml"])} {
		if !strings.Contains(text, want) {
			t.Fatalf("review did not show %q: %s", want, text)
		}
	}
	path := filepath.Join(root, pkg.Manifest.Metadata.Name)
	for _, name := range []string{"main.py", "primitive.yaml", "README.md", "SKILL.md", pack.SavedMarker} {
		if _, err := os.Stat(filepath.Join(path, name)); err != nil {
			t.Fatalf("accepted package missing %s: %v", name, err)
		}
	}
	decisions, err := genreview.ReadGeneratedDecisions(state)
	if err != nil || len(decisions) != 1 || decisions[0].Choice != "accept" || decisions[0].Digest != pkg.Digest {
		t.Fatalf("acceptance not recorded for exact package: %+v %v", decisions, err)
	}
}

func TestReviewGeneratedDenySuppressesUnchangedDigest(t *testing.T) {
	graph := reviewableGraph()
	root, state := t.TempDir(), t.TempDir()
	var out bytes.Buffer
	if err := genreview.ReviewGenerated(strings.NewReader("deny\n"), &out, graph, root, state); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("deny installed a package: %v", entries)
	}
	out.Reset()
	if err := genreview.ReviewGenerated(strings.NewReader("accept\n"), &out, graph, root, state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "was denied locally") {
		t.Fatalf("unchanged denial was not honored: %s", out.String())
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("second review installed a denied package: %v", entries)
	}
}

func TestReviewGeneratedRefineAndUnresolvedGate(t *testing.T) {
	graph := reviewableGraph()
	graph.Problems = []string{"step 1 needs a branch predicate"}
	root, state := t.TempDir(), t.TempDir()
	var out bytes.Buffer
	if err := genreview.ReviewGenerated(strings.NewReader("accept\n"), &out, graph, root, state); err == nil {
		t.Fatal("unresolved graph accepted")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("unresolved graph installed a package: %v", entries)
	}
	out.Reset()
	if err := genreview.ReviewGenerated(strings.NewReader("refine\n"), &out, graph, root, state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Needs decision") {
		t.Fatalf("missing reason in review: %s", out.String())
	}
	handoff := genreview.HandoffDir(state, graph.CandidateID, genreview.UnresolvedGraphDigest(graph))
	if _, err := os.Stat(filepath.Join(handoff, "program-graph.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(handoff, "HANDOFF.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(handoff, "EVIDENCE.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(handoff, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestReviewGeneratedShowsSharedResultBindings(t *testing.T) {
	graph := reviewableGraph()
	graph.Steps = append(graph.Steps, codegen.ProgramStep{Role: "link", Tool: "mcp:link", Binding: &codegen.ProgramToolBinding{Server: "telara", Tool: "link"}, Effect: "write", Args: []codegen.ProgramArg{
		{Path: []string{"inward", "issue_key"}, Value: codegen.ProgramValue{Kind: "result", Step: 1, ResultPath: ".key"}},
		{Path: []string{"outward", "issue_key"}, Value: codegen.ProgramValue{Kind: "result", Step: 1, ResultPath: ".key"}},
		{Path: []string{"link_type"}, Value: codegen.ProgramValue{Kind: "selector", Selector: "relates"}},
	}})
	var out bytes.Buffer
	if err := genreview.ReviewGenerated(strings.NewReader("q\n"), &out, graph, t.TempDir(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Shared result source: result from step 1 at .key supplies inward.issue_key and outward.issue_key") {
		t.Fatalf("result alias hidden from review: %s", out.String())
	}
}

func TestReviewGeneratedResultListIndexNeedsSelectionDecision(t *testing.T) {
	graph := &codegen.ProgramGraph{CandidateID: "lc_listlookup", Inputs: []codegen.ProgramInput{{Name: "position", Type: "integer", Source: "caller selects position from step 1 result.items"}}, Steps: []codegen.ProgramStep{
		{Role: "list", Tool: "mcp:list", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "list"}, Effect: "read"},
		{Role: "get", Tool: "mcp:get", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "get"}, Effect: "read", Args: []codegen.ProgramArg{
			{Path: []string{"id"}, Value: codegen.ProgramValue{Kind: "collection_index", Step: 1, CollectionPath: ".items", ResultPath: ".id", Input: "position"}},
		}},
	}}
	root, state := t.TempDir(), t.TempDir()
	pkg, err := codegen.GenerateProgramPackage(graph)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := genreview.ReviewGenerated(strings.NewReader("accept\n"), &out, graph, root, state); err == nil || !strings.Contains(err.Error(), "undetermined result selection") {
		t.Fatalf("accepted task despite missing selection rule: %v", err)
	}
	for _, want := range []string{"Needs decision:", "source calls prove list membership", pkg.Digest, string(pkg.Files["main.py"])} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("review omitted %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Choose [a]ccept") {
		t.Fatalf("review offered acceptance without a selection rule: %s", out.String())
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("unresolved selection installed a package: %v", entries)
	}
	if tier, shape := genreview.ProgramReviewShape(graph); tier != -1 || !strings.Contains(shape, "selection rule unknown") {
		t.Fatalf("queue did not label the incomplete task: %d %s", tier, shape)
	}
	out.Reset()
	if err := genreview.ReviewGenerated(strings.NewReader("refine\n"), &out, graph, root, state); err != nil {
		t.Fatal(err)
	}
	handoff := filepath.Join(genreview.HandoffDir(state, graph.CandidateID, pkg.Digest), "HANDOFF.md")
	if strings.Contains(filepath.Base(filepath.Dir(handoff)), ":") {
		t.Fatalf("handoff folder %q holds a ':' (not allowed on Windows)", handoff)
	}
	data, err := os.ReadFile(handoff)
	if err != nil || !strings.Contains(string(data), "Unresolved selection:") {
		t.Fatalf("refinement handoff omitted selection decision: %v %s", err, data)
	}
}

func TestProgramSelectionDecisionKeepsKnownCallerRoleSelection(t *testing.T) {
	graph := &codegen.ProgramGraph{Steps: []codegen.ProgramStep{
		{Role: "create", Loop: "caller_items"},
		{Role: "link", Args: []codegen.ProgramArg{{Value: codegen.ProgramValue{Kind: "indexed_result", Step: 1, ResultPath: ".id", Input: "source_role"}}}},
	}}
	if got := genreview.ProgramSelectionDecision(graph); got != "" {
		t.Fatalf("pre-existing caller item role was treated as an unknown future-list decision: %s", got)
	}
}
