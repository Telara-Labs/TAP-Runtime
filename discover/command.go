package discover

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/routine"

	"gitlab.com/telara-labs/tap-runtime/discover/pack"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
)

// Command is `tap discover`: read this machine's agent session history,
// report how it narrowed to primitives, and optionally pick primitives to
// save. It needs no account and sends nothing anywhere. args exclude the
// command name. It returns the process exit code.
func Command(args []string, in io.Reader, out, errOut io.Writer) int {
	// The author path (author.go, validate.go, save.go).
	if len(args) > 0 {
		switch args[0] {
		case "generate":
			return generateCommand(args[1:], in, out, errOut)
		case "brief", "save":
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintln(errOut, "discover:", err)
				return 1
			}
			if args[0] == "brief" {
				return briefCommand(args[1:], home, out, errOut)
			}
			return saveCommand(args[1:], home, out, errOut)
		case "validate":
			return validateCommand(args[1:], out, errOut)
		}
	}
	d := DefaultOptions()
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(errOut)
	clients := fs.String("client", "claude-code,codex,cursor", "clients to read, comma-separated")
	days := fs.Int("days", 0, "only sessions from the last N days (0 = all retained history)")
	top := fs.Int("top", 25, "primitives to list (0 = all)")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	outFile := fs.String("out", "", "also write the JSON report to this file")
	review := fs.Bool("review", false, "list the primitives and pick which to save")
	rejected := fs.Bool("rejected", false, "also list what each check removed, and why")
	patterns := fs.Bool("patterns", false, "also run the pattern search (slower)")
	nOps := fs.Int("opportunities", 0, "also list this many surfaced opportunities with their task references")
	nSpans := fs.Int("span-proposals", 0, "also find and list this many model-free bounded-span proposal groups")
	saveClient := fs.String("save-client", "claude-code", "where saved primitives go: claude-code or codex")
	saveProject := fs.Bool("save-project", false, "save into this project's skills directory instead of your home")
	fs.IntVar(&d.Window, "window", d.Window, "most steps allowed between two steps of a pattern")
	fs.IntVar(&d.MinSupport, "min-support", d.MinSupport, "fewest requests or sessions that count as recurring")
	fs.IntVar(&d.MaxLen, "max-len", d.MaxLen, "longest pattern searched")
	fs.IntVar(&d.Permutations, "permutations", d.Permutations, "shuffled corpora per null model")
	fs.Float64Var(&d.Alpha, "fdr", d.Alpha, "false discovery rate")
	fs.Int64Var(&d.Seed, "seed", d.Seed, "seed for the shuffles, so a run can be repeated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d.Patterns = *patterns
	d.Spans = *nSpans > 0
	d.Progress = errOut
	if *days > 0 {
		d.Since = time.Now().AddDate(0, 0, -*days)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	readers, err := history.DefaultReaders(strings.Split(*clients, ","), home)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 2
	}
	d.Readers = readers

	rep, err := Run(d)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	if *outFile != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*outFile, b, 0o600)
		}
		if err != nil {
			fmt.Fprintln(errOut, "discover:", err)
			return 1
		}
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(errOut, "discover:", err)
			return 1
		}
		return 0
	}
	routine.WriteFunnel(out, rep, *top, *rejected)
	routine.WriteOpportunities(out, rep, *nOps)
	if d.Spans {
		routine.WriteSpanProposals(out, rep, *nSpans)
	}
	if *patterns {
		fmt.Fprintln(out)
		routine.WriteText(out, rep, *top)
	}
	if !*review {
		return 0
	}
	cwd, _ := os.Getwd()
	root, err := pack.SkillsDir(*saveClient, *saveProject, home, cwd)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 2
	}
	err = routine.Review(in, out, rep, routine.ReviewConfig{Top: *top}, routine.ReviewActions{
		Save: func(dr *model.Draft) (string, error) {
			path, unchanged, err := pack.SaveDraft(dr, root)
			if unchanged {
				return path + " (already saved)", err
			}
			return path, err
		},
	})
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	return 0
}
