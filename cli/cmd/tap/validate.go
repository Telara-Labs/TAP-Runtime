package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"telara.dev/tap/internal/diag"
	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/validate"
)

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return newCliError(2, "%v", err)
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}

	pkg, err := model.LoadPackage(dir)
	if err != nil {
		return newCliError(3, "%v", err)
	}
	findings := validate.Validate(pkg)

	if *jsonOut {
		return printJSON(findings, pkg.Dir, !findings.HasErrors())
	}

	printFindingsHuman(pkg.Dir, findings)
	if findings.HasErrors() {
		return newCliError(1, "validate FAILED: %d error(s), %d warning(s)", len(findings.Errors()), len(findings.Warnings()))
	}
	fmt.Printf("validate OK (%s): %d warning(s)\n", pkg.Dir, len(findings.Warnings()))
	return nil
}

func printFindingsHuman(dir string, findings diag.Findings) {
	if len(findings) == 0 {
		return
	}
	fmt.Printf("%s\n", dir)
	for _, f := range findings {
		fmt.Println("  " + f.String())
	}
}

type jsonReport struct {
	Dir      string        `json:"dir"`
	OK       bool          `json:"ok"`
	Findings diag.Findings `json:"findings"`
}

func printJSON(findings diag.Findings, dir string, ok bool) error {
	if findings == nil {
		findings = diag.Findings{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(jsonReport{Dir: dir, OK: ok, Findings: findings}); err != nil {
		return err
	}
	if !ok {
		return newCliError(1, "validate FAILED")
	}
	return nil
}
