package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"telara.dev/tap/internal/diffcmd"
	"telara.dev/tap/internal/model"
)

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return newCliError(2, "%v", err)
	}
	if fs.NArg() < 2 {
		return newCliError(2, "usage: tap diff <dirA> <dirB>")
	}
	a, err := model.LoadPackage(fs.Arg(0))
	if err != nil {
		return newCliError(3, "%v", err)
	}
	b, err := model.LoadPackage(fs.Arg(1))
	if err != nil {
		return newCliError(3, "%v", err)
	}
	result := diffcmd.Diff(a, b)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Printf("%s@%s -> %s@%s\n\n", result.NameA, result.VersionA, result.NameB, result.VersionB)
	if result.DescriptionChanged {
		fmt.Println("description changed:")
		fmt.Printf("  - %s\n", result.DescriptionA)
		fmt.Printf("  + %s\n", result.DescriptionB)
		fmt.Println()
	}
	printSetDiff("input properties", result.InputPropsDiff)
	printSetDiff("output properties", result.OutputPropsDiff)

	fmt.Println("PERMISSIONS:")
	if len(result.Credentials.SlotsAdded) > 0 {
		fmt.Printf("  + credential slots added: %v\n", result.Credentials.SlotsAdded)
	}
	if len(result.Credentials.SlotsRemoved) > 0 {
		fmt.Printf("  - credential slots removed: %v\n", result.Credentials.SlotsRemoved)
	}
	for slot, sd := range result.Credentials.ActionsBySlot {
		if len(sd.Added) > 0 {
			fmt.Printf("  + slot %s actions added: %v\n", slot, sd.Added)
		}
		if len(sd.Removed) > 0 {
			fmt.Printf("  - slot %s actions removed: %v\n", slot, sd.Removed)
		}
	}
	printSetDiff("egress hosts", result.EgressHosts)
	printSetDiff("origin slots", result.OriginSlots)
	printSetDiff("reasoning capabilities", result.ReasoningCaps)
	if result.MaxTokensDelta != 0 {
		fmt.Printf("  reasoning maxTokens delta: %+d\n", result.MaxTokensDelta)
	}
	if result.EffectsClassA != result.EffectsClassB {
		fmt.Printf("  effects.class: %s -> %s\n", result.EffectsClassA, result.EffectsClassB)
	}

	if result.PermissionExpansion {
		fmt.Println("\n[permission-expansion] this version requests MORE authority than the previous one -- requires --acknowledge-expansion at publish and reviewer sign-off (04-cli.md §3)")
		return newCliError(1, "permission-expansion detected")
	}
	fmt.Println("\nno permission expansion detected")
	return nil
}

func printSetDiff(label string, sd diffcmd.SetDiff) {
	if sd.Empty() {
		return
	}
	if len(sd.Added) > 0 {
		fmt.Printf("  + %s added: %v\n", label, sd.Added)
	}
	if len(sd.Removed) > 0 {
		fmt.Printf("  - %s removed: %v\n", label, sd.Removed)
	}
}
