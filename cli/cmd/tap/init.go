package main

import (
	"flag"
	"fmt"

	"telara.dev/tap/internal/scaffold"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	web := fs.Bool("web", false, "scaffold a web (browser-step) package instead of an api package")
	dirFlag := fs.String("dir", "", "target directory (default: the local part of <name>)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"dir": true})); err != nil {
		return newCliError(2, "%v", err)
	}
	if fs.NArg() < 1 {
		return newCliError(2, "usage: tap init <name> [--web] [--dir <path>]")
	}
	name := fs.Arg(0)
	dir, err := scaffold.Scaffold(scaffold.Options{Dir: *dirFlag, Name: name, Web: *web})
	if err != nil {
		return newCliError(3, "%v", err)
	}
	fmt.Printf("scaffolded %s in %s\n", name, dir)
	fmt.Println("next: fill in every REPLACE_* placeholder (see tap-creator SKILL.md), then tap validate " + dir + " && tap test " + dir)
	return nil
}
