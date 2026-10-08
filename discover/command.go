package discover

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/primitive"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"

	"github.com/Telara-Labs/TAP-Runtime/discover/pipeline"

	"github.com/Telara-Labs/TAP-Runtime/discover/genreview"

	"github.com/Telara-Labs/TAP-Runtime/discover/author"

	"github.com/Telara-Labs/TAP-Runtime/discover/routine"

	"github.com/Telara-Labs/TAP-Runtime/discover/pack"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"

	"github.com/Telara-Labs/TAP-Runtime/discover/history"

	"github.com/Telara-Labs/TAP-Runtime/discover/termart"
)

// Command is `tap discover`: read this machine's agent session history,
// report how it narrowed to primitives, and optionally pick primitives to
// save. It needs no account and sends nothing anywhere. args exclude the
// command name. It returns the process exit code.
func Command(args []string, in io.Reader, out, errOut io.Writer) int {
	// With no subcommand, discover always ends in the primitive menu.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return MenuCommand(args, in, out, errOut, nil)
	}
	if args[0] == "migrate-saved" || args[0] == "--migrate-saved" {
		return MigrateCommand(out, errOut)
	}
	// The older narrowing report stays available as `discover report`.
	if args[0] == "evidence" {
		if len(args) != 3 {
			fmt.Fprintln(errOut, "usage: discover evidence <handoff-folder> <execution-id>")
			return 2
		}
		if err := primitive.Resolve(args[1], args[2], out); err != nil {
			fmt.Fprintln(errOut, "discover:", err)
			return 1
		}
		return 0
	}
	if args[0] == "report" {
		args = args[1:]
	} else {
		// The author path (author.go, validate.go, save.go).
		switch args[0] {
		case "build":
			return author.BuildCommand(args[1:], out, errOut)
		case "generate":
			return genreview.GenerateCommand(args[1:], in, out, errOut)
		case "brief", "save":
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintln(errOut, "discover:", err)
				return 1
			}
			if args[0] == "brief" {
				return author.BriefCommand(args[1:], home, out, errOut)
			}
			return author.SaveCommand(args[1:], home, out, errOut)
		case "validate":
			return author.ValidateCommand(args[1:], out, errOut)
		}
	}
	d := pipeline.DefaultOptions()
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(errOut)
	clients := fs.String("client", "detected", "agents whose history to read, comma-separated: "+historyClients()+", all, or detected (installed here)")
	days := fs.Int("days", 0, "only sessions from the last N days (0 = all retained history)")
	top := fs.Int("top", 25, "primitives to list (0 = all)")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	stats := fs.Bool("stats", false, "print, per agent, what was read and what its reader could not parse, then stop")
	outFile := fs.String("out", "", "also write the JSON report to this file")
	review := fs.Bool("review", false, "list the primitives and pick which to save")
	rejected := fs.Bool("rejected", false, "also list what each check removed, and why")
	patterns := fs.Bool("patterns", false, "also run the pattern search (slower)")
	nOps := fs.Int("opportunities", 0, "also list this many surfaced opportunities with their task references")
	nSpans := fs.Int("span-proposals", 0, "also find and list this many model-free bounded-span proposal groups")
	saveClient := fs.String("save-client", "detected", "agents that get a pointer to a saved primitive: "+skillsClients()+", all, none, or detected (installed here and able to run it)")
	saveProject := fs.Bool("save-project", false, "write pointers into this project's skills folders instead of your home's")
	fs.IntVar(&d.Window, "window", d.Window, "most steps allowed between two steps of a pattern")
	fs.IntVar(&d.MinSupport, "min-support", d.MinSupport, "fewest requests or sessions that count as recurring")
	fs.IntVar(&d.MaxLen, "max-len", d.MaxLen, "longest pattern searched")
	fs.IntVar(&d.Permutations, "permutations", d.Permutations, "shuffled corpora per null model")
	fs.Float64Var(&d.Alpha, "fdr", d.Alpha, "false discovery rate")
	fs.Int64Var(&d.Seed, "seed", d.Seed, "seed for the shuffles, so a run can be repeated")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
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

	rep, err := pipeline.Run(d)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	if *stats {
		routine.WriteStats(out, rep)
		return 0
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
	dest, err := pack.NewDestination(*saveClient, *saveProject, home, cwd)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 2
	}
	err = routine.Review(in, out, rep, routine.ReviewConfig{Top: *top}, routine.ReviewActions{
		Save: func(dr *model.Draft) (string, error) {
			path, unchanged, err := pack.SaveDraft(dr, dest.Collection)
			if err != nil {
				return path, err
			}
			ptrs, err := dest.Point(path)
			if unchanged {
				path += " (already saved)"
			}
			return path + "\n" + strings.TrimRight(pack.FormatPointers(ptrs), "\n"), err
		},
	})
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	return 0
}

// MenuCommand reads this machine's history, condenses it into primitives,
// composes them (with known, primitives fetched by a caller such as the
// Telara CLI, plus those accepted here before) and shows the menu. --all
// accepts every proposed primitive without asking.
func MenuCommand(args []string, in io.Reader, out, errOut io.Writer, known []primitive.Known) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(errOut)
	clients := fs.String("client", "detected", "agents whose history to read, comma-separated: "+historyClients()+", all, or detected (installed here)")
	days := fs.Int("days", 0, "only sessions from the last N days (0 = all retained history)")
	all := fs.Bool("all", false, "accept every proposed primitive without asking")
	revisit := fs.Bool("revisit", false, "list your earlier decisions and undo one")
	saveClient := fs.String("save-client", "detected", "agents that get a pointer to an installed primitive: "+skillsClients()+", all, none, or detected (installed here and able to run it)")
	saveProject := fs.Bool("save-project", false, "write pointers into this project's skills folders instead of your home's")
	asJSON := fs.Bool("json", false, "print the condensed result as JSON instead of the menu")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	color := false
	if f, ok := out.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			color = true
		}
	}
	if *revisit {
		if err := primitive.Revisit(in, out, filepath.Join(home, ".tap", "discover"), color); err != nil {
			fmt.Fprintln(errOut, "discover:", err)
			return 1
		}
		return 0
	}
	readers, err := history.DefaultReaders(strings.Split(*clients, ","), home)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 2
	}
	var readIDs []string
	for _, r := range readers {
		readIDs = append(readIDs, r.Client())
	}
	var since time.Time
	if *days > 0 {
		since = time.Now().AddDate(0, 0, -*days)
	}
	// Progress on a terminal: reading and analysing take several seconds.
	// A wide terminal gets the animated logo over one row per agent.
	progress := func(string) {}
	var board *readBoard
	if f, ok := errOut.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && !*asJSON {
			progress = func(msg string) { fmt.Fprint(errOut, "\r\x1b[K"+msg) }
			if termart.Wide(errOut) {
				board = newReadBoard(errOut, readIDs, true)
				progress = func(string) {}
			}
		}
	}
	progress("Reading your agent history…")
	// Repeat runs parse only the session files that changed.
	history.UseCache(filepath.Join(home, ".tap", "discover", "cache"))
	sessions, skipped := readAll(readers, since, board)
	// One agent's store that cannot be read is skipped, never the run.
	reportSkipped := func() {
		for _, s := range skipped {
			fmt.Fprintf(errOut, "discover: skipped %s\n", s)
		}
		for _, n := range history.TakeNotices() {
			fmt.Fprintf(errOut, "discover: %s\n", n)
		}
	}
	if len(sessions) == 0 && !*asJSON {
		board.stop()
		progress("")
		reportSkipped()
		writeNoHistory(out, readIDs, *days)
		return 0
	}
	stateDir := filepath.Join(home, ".tap", "discover")
	known = append(known, primitive.LoadKnown(stateDir)...)
	progress(fmt.Sprintf("Looking for repeated work in %d sessions…", len(sessions)))
	board.analysing(len(sessions))
	var stage primitive.Stage
	if board != nil {
		stage = board.step
	}
	res := primitive.DiscoverStages(sessions, known, stage)
	board.step("checking follow-up steps", 0, 0)
	planPrimitiveFamilies(&res, sessions)
	board.analysed(len(res.Families))
	board.stop()
	progress("")
	reportSkipped()
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(errOut, "discover:", err)
			return 1
		}
		return 0
	}
	cwd, _ := os.Getwd()
	dest, err := pack.NewDestination(*saveClient, *saveProject, home, cwd)
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 2
	}
	cfg := primitive.MenuConfig{StateDir: stateDir, All: *all, Clients: strings.Join(readIDs, ","), Home: home, Sessions: sessions, Color: color,
		Install: primitiveInstaller(sessions, dest),
		Skill:   primitive.Skill{Source: "tap-runtime/discover/genreview/skill/tap-primitive-refine/SKILL.md", Content: genreview.GeneratedRefineSkill}}
	// A terminal on both ends gets the full-screen review; otherwise (a
	// pipe, a test, --all) the line-by-line menu.
	if fin, ok := in.(*os.File); ok && !*all && !*asJSON {
		if fout, ok := out.(*os.File); ok && primitive.Interactive(fin, fout) {
			if err := primitive.RunTUI(fin, fout, res, cfg); err != nil {
				fmt.Fprintln(errOut, "discover:", err)
				return 1
			}
			return 0
		}
	}
	if err := primitive.Menu(in, out, res, cfg); err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	return 0
}

// writeNoHistory says what discover looked for when it found nothing to
// read, and what to do next, instead of an empty report.
func writeNoHistory(out io.Writer, read []string, days int) {
	if len(read) == 0 {
		fmt.Fprintf(out, "No agent history found on this machine.\n\ntap discover reads the session history these agents keep: %s.\nUse one of them for your usual work, then run tap discover again.\n", historyClients())
		return
	}
	window := ""
	if days > 0 {
		window = fmt.Sprintf(" from the last %d days", days)
	}
	fmt.Fprintf(out, "No sessions%s in the history of: %s.\n\nRun tap discover again after you have used these agents for your usual work", window, strings.Join(read, ", "))
	if days > 0 {
		fmt.Fprint(out, ", or drop --days to read all of their history")
	}
	fmt.Fprintf(out, ".\nTo read other agents: tap discover --client <agents> (any of: %s).\n", historyClients())
}

// historyClients and skillsClients list, from the registry, the agents whose
// history discover reads and the agents that have a skills folder.
func historyClients() string { return strings.Join(client.IDs(client.HasHistory), ", ") }
func skillsClients() string  { return strings.Join(client.IDs(client.HasSkills), ", ") }

// MigrateCommand is `tap discover migrate-saved`: primitives saved as full
// packages in agents' skills folders (before the TAP collection) move into
// the collection and leave a pointer behind.
func MigrateCommand(out, errOut io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	coll, err := pack.CollectionDir()
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	cwd, _ := os.Getwd()
	rs, err := pack.MigrateSaved(home, cwd, coll)
	for _, r := range rs {
		if r.Reason != "" {
			fmt.Fprintf(out, "%s %s: %s\n", r.Mode, r.From, r.Reason)
		} else {
			fmt.Fprintf(out, "%s %s -> %s\n", r.Mode, r.From, r.To)
		}
	}
	if err != nil {
		fmt.Fprintln(errOut, "discover:", err)
		return 1
	}
	if len(rs) == 0 {
		fmt.Fprintln(out, "nothing to migrate")
	}
	return 0
}

// readAll reads every agent's history at once; files are parsed under the
// history package's process-wide bound, so the host keeps a CPU. Sessions
// come back in reader order. A reader that fails, or panics, is skipped and
// named in skipped with its reason; the others are still read.
func readAll(readers []trace.Reader, since time.Time, board *readBoard) (sessions []trace.Session, skipped []string) {
	got := make([][]trace.Session, len(readers))
	errs := make([]error, len(readers))
	var wg sync.WaitGroup
	for i, r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					errs[i] = fmt.Errorf("reader failed: %v", p)
					board.skipped(i)
				}
			}()
			board.reading(i)
			var err error
			if pr, ok := r.(trace.ProgressReader); ok && board != nil {
				got[i], err = pr.ReadProgress(since, board.progress(i))
			} else {
				got[i], err = r.Read(since)
			}
			if err != nil {
				errs[i] = err
				board.skipped(i)
				return
			}
			board.read(i, len(got[i]))
		}()
	}
	wg.Wait()
	for i, r := range readers {
		if errs[i] != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", r.Client(), errs[i]))
			continue
		}
		sessions = append(sessions, got[i]...)
	}
	return sessions, skipped
}
