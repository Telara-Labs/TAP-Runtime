package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"telara.dev/tap/internal/explain"
	"telara.dev/tap/internal/model"
)

func runExplain(args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
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
	panel := explain.BuildPanel(pkg.Manifest)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(panel)
	}
	fmt.Print(explain.Render(panel))
	return nil
}
