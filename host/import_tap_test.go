package main

import (
	"strings"
	"testing"
)

// A program Codex wrote and saved began with "import tap" and failed with
// ModuleNotFoundError when it ran. The runner's SDK is importable as tap.
func TestPythonProgramsCanImportTap(t *testing.T) {
	program := "import tap\nfrom tap import tools\nprint('imported', type(tap.call).__name__, tools() == [])\n"
	res, err := runLimited(t, "main.py", "", program, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "imported method True") {
		t.Fatalf("stdout %q stderr %q", res.Stdout, res.Stderr)
	}
}
