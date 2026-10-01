package pyparse_test

import (
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"

	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestInlinePythonEmbeddedShellRemainsUnresolved(t *testing.T) {
	command := "cd /tmp && python3 - <<'PY'\nfrom pathlib import Path\np = Path('a')\ns = p.read_text()\np.write_text(s)\nPY\ngofmt -w a.go"
	s := testkit.NewSession("inline-compound", "Edit and format a file", trace.Call{Tool: "shell", Command: command, Output: "done", Outcome: trace.OutcomeOK})
	spans := retrieval.SelectSpanProposals([]trace.Session{s})
	if len(spans) != 1 || spans[0].CodeShape == "" || spans[0].CodeScope != "embedded" {
		t.Fatalf("compound shell code did not retain an explicit embedded scope: %+v", spans)
	}
}

func TestInlinePythonRepetitionIsRetrievalOnly(t *testing.T) {
	first := testkit.NewSession("inline-one", "Inspect file changes",
		trace.Call{Tool: "shell", Command: "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/one')\ns = p.read_text()\np.write_text(s.replace('a', 'b'))\nPY", Output: "done", Outcome: trace.OutcomeOK})
	second := testkit.NewSession("inline-two", "Make a different edit",
		trace.Call{Tool: "shell", Command: "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/two')\ntext = file.read_text()\nfile.write_text(text.replace('old', 'new'))\nPY", Output: "done", Outcome: trace.OutcomeOK})
	third := testkit.NewSession("inline-three", "Edit another file",
		trace.Call{Tool: "shell", Command: "cd /tmp && python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/three')\ns = p.read_text()\ns = s.replace('first', 'second')\ns = s.replace('second', 'third')\np.write_text(s)\nprint('done')\nPY", Output: "done", Outcome: trace.OutcomeOK})
	one := retrieval.SelectSpanProposals([]trace.Session{first})
	if len(one) != 1 || one[0].Kind != "authored_program" || one[0].CodeShape == "" {
		t.Fatalf("one inline execution was not captured as authored retrieval evidence: %+v", one)
	}
	if got := retrieval.GroupLogicCandidates(one); len(got) != 0 {
		t.Fatalf("one inline execution was called recurring logic: %+v", got)
	}
	spans := retrieval.SelectSpanProposals([]trace.Session{first, second, third})
	groups := retrieval.GroupLogicCandidates(spans)
	if len(groups) != 1 || groups[0].Sessions != 3 || groups[0].Executions != 3 || !strings.Contains(groups[0].Actions[0], "read_text") ||
		!strings.Contains(strings.Join(groups[0].Cautions, ","), "inline_code_shape_variants") ||
		!strings.Contains(strings.Join(groups[0].Cautions, ","), "inline_program_embedded_in_shell") {
		t.Fatalf("cross-session inline logic did not group by operations: %+v", groups)
	}
	graph, err := codegen.SynthesizeProgramGraph(groups[0], spans, []trace.Session{first, second, third})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codegen.GenerateProgramPackage(graph); err == nil {
		t.Fatalf("inline code was offered for installation without internal effect and data-flow proof: %+v", graph)
	}
}
