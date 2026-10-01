package pyparse

import (
	"strings"
	"testing"
)

func TestInlinePythonShapeAbstractsValuesButPreservesOperations(t *testing.T) {
	first := "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/one.txt')\ns = p.read_text()\np.write_text(s.replace('before', 'after'))\nPY"
	second := "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/two.txt')\ncontent = file.read_text()\nfile.write_text(content.replace('alpha', 'beta'))\nPY"
	changed := "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/two.txt')\ncontent = file.read_text()\nfile.write_text(content.upper())\nPY"
	a, b, c := InlinePythonShape(first), InlinePythonShape(second), InlinePythonShape(changed)
	if a == "" || a != b || a == c {
		t.Fatalf("inline code identity did not abstract values while retaining operations: %q %q %q", a, b, c)
	}
	_, family, _ := InlinePythonSnippet(first)
	if strings.Contains(a, "/tmp/") || strings.Contains(a, "before") || !strings.Contains(family, "read_text") {
		t.Fatalf("shape leaked values or hid operations: %q %q", a, family)
	}
}

func TestInlinePythonFamilyFoldsUnrolledCallsAndNestedEvaluation(t *testing.T) {
	nested := "python3 - <<'PY'\nfrom pathlib import Path\np = Path('/tmp/a')\ns = p.read_text()\np.write_text(s.replace('a', 'b'))\nPY"
	unrolled := "python3 - <<'PY'\nfrom pathlib import Path\nfile = Path('/tmp/b')\ntext = file.read_text()\ntext = text.replace('x', 'y')\ntext = text.replace('y', 'z')\nfile.write_text(text)\nprint('done')\nPY"
	exactA, familyA, _ := InlinePythonSnippet(nested)
	exactB, familyB, _ := InlinePythonSnippet(unrolled)
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
	if InlinePythonShape(withFake) != InlinePythonShape(withoutFake) {
		t.Fatal("quoted text affected the operation shape")
	}
	for _, command := range []string{"cd /tmp && " + base, base + "\nrm /tmp/a"} {
		shape, _, embedded := InlinePythonSnippet(command)
		if shape != InlinePythonShape(base) || !embedded {
			t.Fatalf("embedded code lost its shape or shell-context warning: %q %v", shape, embedded)
		}
	}
	for _, command := range []string{
		"python3 - <<'PY'\nprint('unclosed)\nPY",
		base + "\npython3 - <<'MORE'\nprint('another')\nMORE",
	} {
		if got := InlinePythonShape(command); got != "" {
			t.Fatalf("unsupported direct program produced shape %q", got)
		}
	}
}
