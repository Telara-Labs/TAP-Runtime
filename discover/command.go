package discover

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Command is `tap discover`: read this machine's agent session history,
// report how it narrowed to primitives, and optionally pick primitives to
// save. It needs no account and sends nothing anywhere. args exclude the
// command name. It returns the process exit code.
func Command(args []string, in io.Reader, out, errOut io.Writer) int {
	// The author path (author.go, validate.go, save.go).
	if len(args) > 0 {
		switch args[0] {
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
	d.Progress = errOut
	if *days > 0 {
		d.Since = time.Now().AddDate(0, 0, -*days)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	readers, err := DefaultReaders(strings.Split(*clients, ","), home)
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
	WriteFunnel(out, rep, *top, *rejected)
	WriteOpportunities(out, rep, *nOps)
	if *patterns {
		fmt.Fprintln(out)
		WriteText(out, rep, *top)
	}
	if !*review {
		return 0
	}
	cwd, _ := os.Getwd()
	root, err := SkillsDir(*saveClient, *saveProject, home, cwd)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 2
	}
	err = Review(in, out, rep, ReviewConfig{Top: *top}, ReviewActions{
		Save: func(dr *Draft) (string, error) {
			path, unchanged, err := dr.Save(root)
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

// DefaultReaders returns readers for the named clients at their usual
// places under home.
func DefaultReaders(clients []string, home string) ([]Reader, error) {
	var out []Reader
	for _, c := range clients {
		switch strings.TrimSpace(c) {
		case "claude-code":
			out = append(out, ClaudeCode{Dir: filepath.Join(home, ".claude", "projects")})
		case "codex":
			out = append(out, Codex{Dir: filepath.Join(home, ".codex", "sessions")})
		case "cursor":
			out = append(out, Cursor{DB: CursorStateDB(home)})
		case "":
		default:
			return nil, fmt.Errorf("unknown client %q (want claude-code, codex or cursor)", c)
		}
	}
	return out, nil
}

// CursorStateDB is where Cursor keeps its chat store on this OS.
func CursorStateDB(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "User", "globalStorage", "state.vscdb")
		}
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}
