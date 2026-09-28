package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/testrunner"
)

func runDev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	caseName := fs.String("case", "", "contract-test case to replay (default: first case)")
	var binds multiFlag
	fs.Var(&binds, "bind", "slot=origin binding for offline origin-slot checks (repeatable)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"case": true, "bind": true})); err != nil {
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
	rep, err := testrunner.Trace(pkg, *caseName, bindMap)
	if err != nil {
		return newCliError(3, "%v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	fmt.Printf("dev trace: %s (case: %s)\n", rep.Dir, rep.CaseName)
	fmt.Println("input:")
	printIndentedJSON(rep.Input)
	for _, s := range rep.Steps {
		status := "ran"
		if s.Skipped {
			status = "skipped"
		}
		if s.Error != "" {
			status = "ERROR"
		}
		fmt.Printf("\nstep %q [%s] -- %s\n", s.StepID, s.Type, status)
		if len(s.Requirements) > 0 {
			fmt.Printf("  requirements exercised: %v\n", s.Requirements)
		}
		if len(s.Bindings) > 0 {
			fmt.Println("  resolved bindings:")
			printIndentedJSON(s.Bindings)
		}
		if s.Error != "" {
			fmt.Printf("  error: %s\n", s.Error)
		} else if s.Output != nil {
			fmt.Println("  output:")
			printIndentedJSON(s.Output)
		}
	}
	fmt.Println("\nfinal output:")
	printIndentedJSON(rep.Output)
	return nil
}

func printIndentedJSON(v interface{}) {
	b, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		fmt.Printf("  <unprintable: %v>\n", err)
		return
	}
	fmt.Println("  " + string(b))
}
