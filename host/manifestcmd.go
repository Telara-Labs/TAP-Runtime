package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	mf "gitlab.com/telara-labs/tap-runtime/manifest"
)

// manifestCommand is `host manifest`:
//
//	host manifest check [--publish] <package-dir>
//	host manifest complete [--publisher NAME] [--write] <package-dir>
//
// check says whether a manifest may be run, or published. complete prints
// the publishable manifest the short one grows into, deriving what it can
// and marking with TODO what a person has to write.
func manifestCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: host manifest check [--publish] <dir> | host manifest complete [--publisher NAME] [--write] <dir>")
		return 2
	}
	fs := flag.NewFlagSet("manifest "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	publish := fs.Bool("publish", false, "check against what publishing requires")
	publisher := fs.String("publisher", "", "publisher to write, such as dev.example")
	write := fs.Bool("write", false, "replace primitive.yaml with the completed manifest")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "give one package directory")
		return 2
	}
	dir := fs.Arg(0)
	m, err := mf.Load(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch args[0] {
	case "check":
		level, problems := "run", m.RunProblems()
		if *publish {
			level, problems = "publish", m.PublishProblems()
		}
		if len(problems) == 0 {
			fmt.Fprintf(stdout, "%s: may be %s\n", m.Metadata.Name, map[string]string{"run": "run", "publish": "published"}[level])
			return 0
		}
		fmt.Fprintf(stdout, "%s: cannot be %s, %d problem(s)\n", m.Metadata.Name, map[string]string{"run": "run", "publish": "published"}[level], len(problems))
		for _, p := range problems {
			fmt.Fprintf(stdout, "  - %s\n", p)
		}
		return 1
	case "complete":
		if problems := m.RunProblems(); len(problems) > 0 {
			fmt.Fprintf(stderr, "%s cannot be run yet, so it is not completed:\n", m.Metadata.Name)
			for _, p := range problems {
				fmt.Fprintf(stderr, "  - %s\n", p)
			}
			return 1
		}
		pub := *publisher
		if pub == "" {
			pub = m.Metadata.Publisher
		}
		if pub == "" {
			fmt.Fprintln(stderr, "the manifest names no publisher; pass --publisher")
			return 2
		}
		out := m.Complete(pub).YAML()
		if *write {
			if err := os.WriteFile(filepath.Join(dir, "primitive.yaml"), out, 0o644); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(dir, "primitive.yaml"))
			return 0
		}
		stdout.Write(out)
		return 0
	}
	fmt.Fprintf(stderr, "unknown manifest command %q\n", args[0])
	return 2
}
