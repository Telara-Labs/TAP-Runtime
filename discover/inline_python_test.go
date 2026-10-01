package discover

import (
	"strings"
	"testing"
)

func TestInlinePythonShapeAbstractsValuesButPreservesOperations(t *testing.T) {
	first := "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/one.txt')\ns = p.read_text()\np.write_text(s.replace('before', 'after'))\nPY"
	second := "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/two.txt')\ncontent = file.read_text()\nfile.write_text(content.replace('alpha', 'beta'))\nPY"
	changed := "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/two.txt')\ncontent = file.read_text()\nfile.write_text(content.upper())\nPY"
	a, b, c := inlinePythonShape(first), inlinePythonShape(second), inlinePythonShape(changed)
	if a == "" || a != b || a == c {
		t.Fatalf("inline code identity did not abstract values while retaining operations: %q %q %q", a, b, c)
	}
	_, family, _ := inlinePythonSnippet(first)
	if strings.Contains(a, "/tmp/") || strings.Contains(a, "before") || !strings.Contains(family, "read_text") {
		t.Fatalf("shape leaked values or hid operations: %q %q", a, family)
	}
}

func TestInlinePythonFamilyFoldsUnrolledCallsAndNestedEvaluation(t *testing.T) {
	nested := "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/a')\ns = p.read_text()\np.write_text(s.replace('a', 'b'))\nPY"
	unrolled := "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/b')\ntext = file.read_text()\ntext = text.replace('x', 'y')\ntext = text.replace('y', 'z')\nfile.write_text(text)\nprint('done')\nPY"
	exactA, familyA, _ := inlinePythonSnippet(nested)
	exactB, familyB, _ := inlinePythonSnippet(unrolled)
	if exactA == exactB || familyA == "" || familyA != familyB {
		t.Fatalf("unrolled replacement did not share its broad call family: %q %q / %q %q", exactA, familyA, exactB, familyB)
	}
	if !strings.Contains(familyA, "Path>read_text>replace>write_text") {
		t.Fatalf("nested call order did not follow evaluation order: %s", familyA)
	}
}

func TestInlinePythonShapeMarksShellContextAndIgnoresQuotedFakeCalls(t *testing.T) {
	base := "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/a')\ns = p.read_text()\np.write_text(s)\nPY"
	withFake := "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/a')\ns = p.read_text()\np.write_text(s)\nmessage = 'delete_everything()'\nPY"
	withoutFake := strings.Replace(withFake, "delete_everything()", "harmless words", 1)
	if inlinePythonShape(withFake) != inlinePythonShape(withoutFake) {
		t.Fatal("quoted text affected the operation shape")
	}
	for _, command := range []string{"cd /tmp && " + base, base + "\nrm /tmp/a"} {
		shape, _, embedded := inlinePythonSnippet(command)
		if shape != inlinePythonShape(base) || !embedded {
			t.Fatalf("embedded code lost its shape or shell-context warning: %q %v", shape, embedded)
		}
	}
	for _, command := range []string{
		"python3 - <<'PY'\nprint('unclosed)\nPY",
		base + "\npython3 - <<'MORE'\nprint('another')\nMORE",
	} {
		if got := inlinePythonShape(command); got != "" {
			t.Fatalf("unsupported direct program produced shape %q", got)
		}
	}
}

func TestInlinePythonEmbeddedShellRemainsUnresolved(t *testing.T) {
	command := "cd /tmp && python3 - <<'PY'\nfrom pathlib import Path\np = Path('a')\ns = p.read_text()\np.write_text(s)\nPY\ngofmt -w a.go"
	s := selSession("inline-compound", "Edit and format a file", Call{Tool: "shell", Command: command, Output: "done", Outcome: OutcomeOK})
	spans := SelectSpanProposals([]Session{s})
	if len(spans) != 1 || spans[0].CodeShape == "" || spans[0].CodeScope != "embedded" {
		t.Fatalf("compound shell code did not retain an explicit embedded scope: %+v", spans)
	}
}

func TestInlinePythonRepetitionIsRetrievalOnly(t *testing.T) {
	first := selSession("inline-one", "Inspect file changes",
		Call{Tool: "shell", Command: "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/one')\ns = p.read_text()\np.write_text(s.replace('a', 'b'))\nPY", Output: "done", Outcome: OutcomeOK})
	second := selSession("inline-two", "Make a different edit",
		Call{Tool: "shell", Command: "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/two')\ntext = file.read_text()\nfile.write_text(text.replace('old', 'new'))\nPY", Output: "done", Outcome: OutcomeOK})
	third := selSession("inline-three", "Edit another file",
		Call{Tool: "shell", Command: "cd /tmp && python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/three')\ns = p.read_text()\ns = s.replace('first', 'second')\ns = s.replace('second', 'third')\np.write_text(s)\nprint('done')\nPY", Output: "done", Outcome: OutcomeOK})
	one := SelectSpanProposals([]Session{first})
	if len(one) != 1 || one[0].Kind != "authored_program" || one[0].CodeShape == "" {
		t.Fatalf("one inline execution was not captured as authored retrieval evidence: %+v", one)
	}
	if got := GroupLogicCandidates(one); len(got) != 0 {
		t.Fatalf("one inline execution was called recurring logic: %+v", got)
	}
	spans := SelectSpanProposals([]Session{first, second, third})
	groups := GroupLogicCandidates(spans)
	if len(groups) != 1 || groups[0].Sessions != 3 || groups[0].Executions != 3 || !strings.Contains(groups[0].Actions[0], "read_text") ||
		!strings.Contains(strings.Join(groups[0].Cautions, ","), "inline_code_shape_variants") ||
		!strings.Contains(strings.Join(groups[0].Cautions, ","), "inline_program_embedded_in_shell") {
		t.Fatalf("cross-session inline logic did not group by operations: %+v", groups)
	}
	graph, err := SynthesizeProgramGraph(groups[0], spans, []Session{first, second, third})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateProgramPackage(graph); err == nil {
		t.Fatalf("inline code was offered for installation without internal effect and data-flow proof: %+v", graph)
	}
}
