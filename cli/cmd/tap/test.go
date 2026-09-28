package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/testrunner"
)

type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func runTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	caseFilter := fs.String("k", "", "only run cases whose name contains this substring")
	var binds multiFlag
	fs.Var(&binds, "bind", "slot=origin binding for offline origin-slot checks (repeatable); see README DECISION")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"k": true, "bind": true})); err != nil {
		return newCliError(2, "%v", err)
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}

	bindMap, err := testrunner.ParseBindFlags(binds)
	if err != nil {
		return newCliError(2, "%v", err)
	}

	pkg, err := model.LoadPackage(dir)
	if err != nil {
		return newCliError(3, "%v", err)
	}
	if pkg.WorkflowErr != nil {
		return newCliError(3, "workflow.yaml did not parse: %v", pkg.WorkflowErr)
	}

	report, err := testrunner.Run(pkg, testrunner.Options{Binds: bindMap, CaseFilter: *caseFilter})
	if err != nil {
		return newCliError(3, "%v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		if report.Failed() > 0 {
			return newCliError(1, "test FAILED")
		}
		return nil
	}

	fmt.Printf("%s\n", report.Dir)
	for _, c := range report.Cases {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %s (steps_executed=%d)\n", status, c.Name, c.StepsExecuted)
		for _, f := range c.Failures {
			fmt.Printf("        - %s\n", f)
		}
	}
	fmt.Printf("%d passed, %d failed, %d total\n", report.Passed(), report.Failed(), len(report.Cases))
	if report.Failed() > 0 {
		return newCliError(1, "test FAILED: %d/%d cases failed", report.Failed(), len(report.Cases))
	}
	return nil
}
