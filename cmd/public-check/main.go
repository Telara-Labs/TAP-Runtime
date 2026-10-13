package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Telara-Labs/TAP-Runtime/internal/publiccheck"
)

func main() {
	flags := flag.NewFlagSet("public-check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	base := flags.String("commit-base", "", "base commit for the incoming range (required)")
	head := flags.String("commit-head", "", "head commit for the incoming range (required)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *base == "" || *head == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: public-check --commit-base BASE --commit-head HEAD")
		os.Exit(2)
	}
	root, err := publiccheck.RepositoryRoot(context.Background(), ".")
	if err != nil {
		fatal(err)
	}
	violations, err := publiccheck.CheckCommitRange(context.Background(), root, *base, *head)
	if err != nil {
		fatal(err)
	}
	if len(violations) != 0 {
		for _, violation := range violations {
			fmt.Fprintln(os.Stderr, violation)
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "public-check: incoming commit messages are clear")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "public-check:", err)
	os.Exit(2)
}
