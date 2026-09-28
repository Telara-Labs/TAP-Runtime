package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"telara.dev/tap/internal/compile"
	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/validate"
)

// runCompile implements `tap compile [dir] [--out file] [--json]`: it validates
// the package first (compiling an invalid package would emit a WorkflowDefinition
// the executor rejects), then emits the compiled WorkflowDefinition in the
// lowercase-friendly JSON dialect (09-workflow-spec.md §1) to stdout or --out.
func runCompile(args []string) error {
	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	out := fs.String("out", "", "write compiled WorkflowDefinition JSON to this file instead of stdout")
	strict := fs.Bool("strict", false, "treat compiler warnings as failures")
	protoOut := fs.Bool("proto", false, "emit via the real telara-proto WorkflowDefinition message (protojson) instead of the hand-JSON dialect; proves field alignment with the published proto")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"out": true})); err != nil {
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

	// Validate first; a package that does not pass validate cannot compile to a
	// well-formed definition.
	vfindings := validate.Validate(pkg)
	if vfindings.HasErrors() {
		fmt.Fprintf(os.Stderr, "compile ABORTED: package does not validate (%s)\n", pkg.Dir)
		for _, f := range vfindings.Errors() {
			fmt.Fprintln(os.Stderr, "  "+f.String())
		}
		return newCliError(1, "compile FAILED: fix validate errors first")
	}

	def, cfindings, err := compile.Compile(pkg)
	if err != nil {
		return newCliError(1, "compile FAILED: %v", err)
	}
	if cfindings.HasErrors() {
		fmt.Fprintf(os.Stderr, "compile FAILED (%s): compiler-contract violations\n", pkg.Dir)
		for _, f := range cfindings.Errors() {
			fmt.Fprintln(os.Stderr, "  "+f.String())
		}
		return newCliError(1, "compile FAILED: %d error(s)", len(cfindings.Errors()))
	}

	var data []byte
	if *protoOut {
		data, err = compile.MarshalProtoJSON(def)
		if err != nil {
			return newCliError(3, "marshaling definition via proto: %v", err)
		}
	} else {
		data, err = marshalDefinition(def)
		if err != nil {
			return newCliError(3, "marshaling definition: %v", err)
		}
	}

	if *out != "" {
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			return newCliError(3, "writing %s: %v", *out, err)
		}
		fmt.Fprintf(os.Stderr, "compile OK (%s): wrote %s (%d node(s), %d edge(s), %d warning(s))\n",
			pkg.Dir, *out, len(def.Nodes), len(def.Edges), len(cfindings.Warnings()))
	} else {
		fmt.Println(string(data))
		for _, f := range cfindings.Warnings() {
			fmt.Fprintln(os.Stderr, "  "+f.String())
		}
	}

	if *strict && len(cfindings.Warnings()) > 0 {
		return newCliError(1, "compile FAILED (--strict): %d warning(s)", len(cfindings.Warnings()))
	}
	return nil
}

// marshalDefinition renders the definition as stable, indented JSON.
func marshalDefinition(def *compile.Definition) ([]byte, error) {
	return json.MarshalIndent(def, "", "  ")
}
