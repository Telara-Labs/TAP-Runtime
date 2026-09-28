// host is the runner. Doc 34 section 13. It loads a package whose
// entrypoint is a SOURCE FILE, runs it inside an interpreter that is itself a
// wasm module, and executes each declared host command on the guest's behalf.
//
// The guest gets no filesystem, no environment and no network. NO WithFSConfig.
// NO WithEnv. The one exception is the Python interpreter, which needs its
// standard library: it gets a READ-ONLY mount of that directory and nothing
// else (see guestConfig).
//
// Interpreters are downloaded on first use against a pinned digest and are
// never compiled into this binary (interpreters.go).
//
//	host [--approve] [--journal FILE] <package-dir> [script args...]
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"gitlab.com/telara-labs/tap-runtime/bridge"
	runlog "gitlab.com/telara-labs/tap-runtime/journal"
	mf "gitlab.com/telara-labs/tap-runtime/manifest"
)

// The manifest types live in package manifest. These names are how this
// package has always referred to them.
type (
	manifest  = mf.Manifest
	toolDecl  = mf.Tool
	command   = mf.Command
	fileDecl  = mf.File
	fetchDecl = mf.Fetch
)

type request struct {
	ID         string            `json:"id,omitempty"`
	Method     string            `json:"method"`
	Alias      string            `json:"alias"`
	Arguments  map[string]any    `json:"arguments"`
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Stdout     string            `json:"stdout"`
	Stderr     string            `json:"stderr"`
	Exit       int               `json:"exit"`
	Stdin      string            `json:"stdin"`
	Path       string            `json:"path"`
	URL        string            `json:"url"`
	HTTPMethod string            `json:"http_method"`
	Headers    map[string]string `json:"headers"`
}

type reply struct {
	// Violation is set when a tool's answer is not what its contract
	// promises. Landed adds that the call was a change and has been made.
	Violation bool   `json:"violation,omitempty"`
	Landed    bool   `json:"landed,omitempty"`
	ID        string `json:"id,omitempty"`
	// Unknown is set when a change was in progress as an earlier run stopped,
	// so nobody can say whether it happened.
	Unknown bool     `json:"unknown,omitempty"`
	Refused string   `json:"refused,omitempty"`
	Result  string   `json:"result,omitempty"`
	Tools   []string `json:"tools,omitempty"`
	Stdout  string   `json:"stdout"`
	Stderr  string   `json:"stderr"`
	Exit    int      `json:"exit"`
	Stdin   string   `json:"stdin"`
	Status  int      `json:"status,omitempty"`
	// Gated is set when the only thing missing is a person's agreement.
	Gated bool `json:"gated,omitempty"`
}

// Approver is asked, on a person's behalf, whether a primitive may make a
// kind of change, and how many times. Doc 34 section 11.11: the approval
// carries the ceiling, and reaching it asks again.
type Approver func(ask Ask) Grant

// Ask describes a kind of change a primitive wants to make.
type Ask struct {
	Primitive string
	Effect    string // write, destructive, financial or identity-admin
	Kind      string // what kind of change: one tool, one command, one directory, one origin
	Example   string // the exact change that is waiting
	Done      int    // how many of this kind this run has already made
}

// Grant is the answer. Limit is how many more changes of this kind may be
// made before the person is asked again. Unlimited is for a caller who has
// already agreed to everything, such as --approve on the command line.
type Grant struct {
	OK    bool
	Limit int
}

const Unlimited = -1

// Options is everything a run is given.
type Options struct {
	Package     string
	Args        []string
	Approve     Approver
	Journal     io.Writer
	InterpDir   string
	CacheDir    string
	PyLib       string
	Client      string        // claude, codex, or empty to detect
	Bridge      bridge.Bridge // set by the server, which already knows the client
	ReceiptPath string

	// RunsDir holds one directory per run. Empty means the user cache
	// directory. NoJournal runs without a record, and so without resume.
	RunsDir   string
	NoJournal bool
	// Resume continues the run with this id instead of starting one.
	Resume string
	// RetentionDays is how long the record of a run is kept. Records older
	// than this are removed when a run starts. 0 keeps them for ever.
	RetentionDays int

	// stopAfter and stopDuring end the run the way a crash would, for tests:
	// after that many requests have been answered, or once the request with
	// that id has been recorded as begun and before it is acted on.
	stopAfter  int
	stopDuring string
}

// errInterrupted is what Run returns when a test stops it.
var errInterrupted = errors.New("the run was interrupted")

// Result is what a run produced.
type Result struct {
	RunID     string
	Replayed  int // requests answered from the record of an earlier run
	Unknown   int // changes whose outcome nobody can state
	Exit      int
	Stdout    string
	Stderr    string
	Ran       int
	Refused   int
	Admission *admission
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("tap-runtime", version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "install" {
		os.Exit(installCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "manifest" {
		os.Exit(manifestCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(os.Stdin, os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "host  fatal:", err)
			os.Exit(1)
		}
		return
	}
	approve := flag.Bool("approve", false, "approve write and destructive actions for this run")
	limit := flag.Int("limit", 0, "with --approve: how many changes of each kind may be made; 0 is no limit")
	journalPath := flag.String("journal", "", "append one JSON line per action")
	interpDir := flag.String("interpreters", "", "interpreter store; default is the user cache directory")
	cacheDir := flag.String("cache", "", "directory for the compiled-interpreter cache")
	client := flag.String("client", "", "client whose connections to borrow: claude or codex; detected when empty")
	receiptPath := flag.String("receipt", "", "write the admission record as JSON")
	runsDir := flag.String("runs", "", "directory holding one record per run; default is the user cache directory")
	resume := flag.String("resume", "", "continue the run with this id")
	noJournal := flag.Bool("no-record", false, "keep no record of the run; it cannot be resumed")
	retention := flag.Int("retention-days", 30, "remove the records of runs older than this many days; 0 keeps them for ever")
	pyLib := flag.String("pylib", "", "python standard library directory, mounted read-only")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: host [--approve] [--resume RUN] <package-dir> [args...]\n       host serve\n       host manifest check|complete <package-dir>")
		os.Exit(2)
	}
	var journal io.Writer = io.Discard
	if *journalPath != "" {
		f, err := os.OpenFile(*journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		must(err)
		defer f.Close()
		journal = f
	}
	grant := Grant{OK: *approve, Limit: Unlimited}
	if *limit > 0 {
		grant.Limit = *limit
	}
	res, err := Run(context.Background(), Options{
		Package: flag.Arg(0), Args: flag.Args()[1:], Journal: journal,
		Approve:   func(Ask) Grant { return grant },
		InterpDir: *interpDir, CacheDir: *cacheDir, PyLib: *pyLib, Client: *client, ReceiptPath: *receiptPath,
		RunsDir: *runsDir, Resume: *resume, NoJournal: *noJournal, RetentionDays: *retention,
	})
	must(err)
	if res.Unknown > 0 {
		logf("UNKNOWN    %d change(s) were in progress when an earlier run stopped. Nobody can say whether they happened; a person has to check.", res.Unknown)
	}
	if res.Stderr != "" {
		logf("script stderr:\n%s", strings.TrimRight(res.Stderr, "\n"))
	}
	fmt.Printf("RESULT (exit %d)\n%s", res.Exit, res.Stdout)
	os.Exit(res.Exit)
}

// Run admits a package, runs it in the sandbox and serves its requests.
func Run(ctx context.Context, o Options) (*Result, error) {
	loaded, err := mf.Load(o.Package)
	if err != nil {
		return nil, err
	}
	if problems := loaded.RunProblems(); len(problems) > 0 {
		return nil, fmt.Errorf("primitive.yaml cannot be run:\n  - %s", strings.Join(problems, "\n  - "))
	}
	m := *loaded
	// Ruling 7 and 34 section 5.1: a subprocess is not contained. This
	// runner runs the contained tier only.
	if m.Runtime() != mf.RuntimeWasm {
		return nil, fmt.Errorf("execution.runtime is %s; this runner runs %s only", m.Runtime(), mf.RuntimeWasm)
	}
	script, err := os.ReadFile(filepath.Join(o.Package, m.Execution.Entrypoint))
	if err != nil {
		return nil, err
	}
	rawManifest, _ := os.ReadFile(filepath.Join(o.Package, "primitive.yaml"))
	sumPkg := sha256.Sum256(append(append([]byte{}, rawManifest...), script...))
	pkgDigest := hex.EncodeToString(sumPkg[:])

	var run *runlog.Journal
	args := o.Args
	started := time.Now().UTC()
	if !o.NoJournal {
		root := o.RunsDir
		if root == "" {
			base, err := os.UserCacheDir()
			if err != nil {
				return nil, err
			}
			root = filepath.Join(base, "tap-runtime", "runs")
		}
		if n := runlog.Sweep(root, o.RetentionDays, started); n > 0 {
			logf("removed the records of %d run(s) older than %d days", n, o.RetentionDays)
		}
		if o.Resume != "" {
			run, err = runlog.Open(root, o.Resume)
			if err != nil {
				return nil, err
			}
			if run.Header.PackageDigest != pkgDigest {
				run.Close()
				return nil, fmt.Errorf("run %s was started with a different version of this package; it cannot be continued with this one", o.Resume)
			}
			// A resumed run is the same run, with the same arguments.
			args = run.Header.Args
			done, open := run.Counts()
			logf("resuming   %s: %d request(s) already answered, %d left unanswered", o.Resume, done, open)
		} else {
			abs, _ := filepath.Abs(o.Package)
			run, err = runlog.Create(root, runlog.Header{RunID: runlog.NewRunID(started), Package: abs, PackageDigest: pkgDigest, Args: o.Args, Started: started})
			if err != nil {
				return nil, err
			}
			logf("run        %s", run.Header.RunID)
		}
		defer run.Close()
	} else if o.Resume != "" {
		return nil, fmt.Errorf("a run cannot be resumed without its record")
	}

	store, err := storeDir(o.InterpDir)
	if err != nil {
		return nil, err
	}
	wasmBytes, in, sum, err := obtain(store, m.Execution.Entrypoint)
	if err != nil {
		return nil, err
	}
	kind := in.Kind
	if kind == "ts" {
		js, err := stripTypes(string(script), m.Execution.Entrypoint)
		if err != nil {
			return nil, err
		}
		script, kind = []byte(js), "js"
	}
	journal := o.Journal
	if journal == nil {
		journal = io.Discard
	}

	logf("package    %s  entrypoint %s (source, not compiled)", m.Metadata.Name, m.Execution.Entrypoint)
	logf("interpreter %s (%d bytes) sha256:%s", in.File, len(wasmBytes), sum[:12])
	for _, c := range m.Commands {
		logf("declared   %-8s %-18s %s", c.Command, strings.Join(c.Args, " "), c.Effect)
	}

	var adm *admission
	br := o.Bridge
	if len(m.Tools) > 0 {
		if br == nil {
			c := o.Client
			if c == "" {
				c = detectClient()
			}
			br, err = openBridge(c)
			if err != nil {
				return nil, err
			}
			defer br.Close()
		}
		adm, err = admit(m.Tools, br, m.Capabilities...)
		if err != nil {
			return nil, err
		}
		logf("client     %s %s", adm.Client, adm.Version)
		if !adm.Tested {
			logf("WARNING    this runner was not run against %s %s; proceeding (recorded on the receipt)", adm.Client, adm.Version)
		}
		for _, b := range adm.Bindings {
			note := "contract not checked: the client gives no schema"
			switch {
			case b.ContractChecked:
				note = "contract checked against the tool's schema " + b.Schema[:19]
			case b.Pinned:
				note = "pinned"
			case br.HasSchemas():
				note = "bound by name: the manifest carries no contract for it"
			}
			if b.ResultChecked {
				note += "; answers are checked"
			}
			logf("bound      %-10s %-26s -> %s / %s  score %.3f  declared %s, annotated %s  (%s)", b.Alias, b.Capability, b.Server, b.Tool, b.Score, b.Declared, b.Annotated, note)
			if b.Gated {
				logf("           %-10s its server says nothing about what it does; treated as a write", b.Alias)
			}
		}
		if o.ReceiptPath != "" {
			j, _ := json.MarshalIndent(adm, "", "  ")
			if err := os.WriteFile(o.ReceiptPath, append(j, '\n'), 0o600); err != nil {
				return nil, err
			}
		}
	}

	// The program is stopped by cancelling its context, which the engine
	// checks for. Closing the engine under a running program is a race.
	guestCtx, stopGuest := context.WithCancel(ctx)
	defer stopGuest()
	rc := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if o.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(o.CacheDir)
		if err != nil {
			return nil, err
		}
		defer cache.Close(ctx)
		rc = rc.WithCompilationCache(cache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, rc)
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	t0 := time.Now()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, err
	}
	logf("compiled interpreter in %s", time.Since(t0).Round(time.Millisecond))

	// OS pipes, not io.Pipe: io.Pipe is unbuffered, so a guest flushing stdout
	// while the host writes its reply deadlocks both sides.
	toGuestR, toGuestW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromGuestR, fromGuestW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	var guestErr bytes.Buffer
	cfg := guestConfig(kind, o.PyLib, string(script), args).
		WithStdin(toGuestR).WithStdout(fromGuestW).WithStderr(&guestErr)
	if run != nil {
		cfg = observed(cfg, run.Observed)
	}

	done := make(chan error, 1)
	go func() {
		_, err := rt.InstantiateModule(guestCtx, compiled, cfg)
		fromGuestW.Close()
		done <- err
	}()

	enc := json.NewEncoder(toGuestW)
	if kind == "sh" {
		if err := enc.Encode(map[string]any{"script": string(script), "args": args}); err != nil {
			return nil, err
		}
	}

	// allowance is what a person has agreed to, by kind of change. A kind
	// they declined stays declined for the run. A kind whose allowance is
	// used up is asked about again, with the count so far.
	type allowance struct {
		left     int
		done     int
		declined bool
	}
	var gateMu sync.Mutex
	allowed := map[string]*allowance{}
	audit := func(outcome, kind, effect string, extra map[string]any) {
		e := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "outcome": outcome, "kind": kind, "effect": effect}
		for k, v := range extra {
			e[k] = v
		}
		b, _ := json.Marshal(e)
		journal.Write(append(b, '\n'))
	}
	// gate runs op without approval first. If the only thing missing is a
	// person's agreement, it finds or asks for that agreement and runs op
	// again with it. spend is false for a question that changes nothing.
	gate := func(op func(approve bool) reply, kind, example, effect string, spend bool) reply {
		rp := op(false)
		if !rp.Gated {
			return rp
		}
		gateMu.Lock()
		a := allowed[kind]
		if a == nil {
			a = &allowance{}
			allowed[kind] = a
		}
		if !a.declined && a.left == 0 {
			var g Grant
			if o.Approve != nil {
				g = o.Approve(Ask{Primitive: m.Metadata.Name, Effect: effect, Kind: kind, Example: example, Done: a.done})
			}
			switch {
			case !g.OK:
				a.declined = true
				audit("declined", kind, effect, map[string]any{"after": a.done})
			case g.Limit == Unlimited:
				a.left = Unlimited
				audit("approved", kind, effect, map[string]any{"limit": "none", "after": a.done})
			default:
				if g.Limit < 1 {
					g.Limit = 1
				}
				a.left = g.Limit
				audit("approved", kind, effect, map[string]any{"limit": g.Limit, "after": a.done})
			}
		}
		ok := !a.declined && a.left != 0
		if ok && spend {
			if a.left > 0 {
				a.left--
			}
			a.done++
		}
		gateMu.Unlock()
		if !ok {
			return rp
		}
		return op(true)
	}

	// effectOf says what a request would do, before anything is done.
	effectOf := func(rq request) string {
		switch rq.Method {
		case "call":
			if adm != nil {
				if bd := adm.byAlias[rq.Alias]; bd != nil {
					return bd.effective()
				}
			}
			return "write"
		case "exec":
			if _, e := resolve(&m, rq.Command, rq.Args); e != "" {
				return e
			}
			return "write"
		case "write":
			return "write"
		case "fetch":
			if mth := strings.ToUpper(rq.HTTPMethod); mth == "" || mth == "GET" || mth == "HEAD" {
				return "read"
			}
			return "write"
		}
		return "read"
	}

	act := func(rq request) reply {
		switch rq.Method {
		case "tools":
			rp := reply{Tools: []string{}}
			if adm != nil {
				rp.Tools = adm.aliases()
			}
			return rp
		case "call":
			kind, example := "call "+rq.Alias, "call "+rq.Alias
			if adm != nil {
				if bd := adm.byAlias[rq.Alias]; bd != nil {
					a, _ := json.Marshal(rq.Arguments)
					kind = fmt.Sprintf("call the tool %s / %s", bd.Server, bd.Tool)
					example = fmt.Sprintf("%s with %s", kind, a)
				}
			}
			return gate(func(a bool) reply { return callTool(adm, br, rq, a, journal) }, kind, example, effectOf(rq), true)
		case "exec":
			kind := "run " + rq.Command
			if decl, _ := resolve(&m, rq.Command, rq.Args); decl != nil {
				kind = strings.TrimSpace("run " + decl.Command + " " + strings.Join(decl.Args, " "))
			}
			return gate(func(a bool) reply { return runCommand(&m, rq, a, journal) }, kind,
				"run "+strings.TrimSpace(rq.Command+" "+strings.Join(rq.Args, " ")), effectOf(rq), true)
		case "read", "write", "canwrite":
			return gate(func(a bool) reply { return fileOp(&m, rq, a, journal) },
				"write files under "+declaredRoot(&m, rq.Path), "write the file "+rq.Path, "write", rq.Method == "write")
		case "fetch":
			origin := rq.URL
			if u, err := url.Parse(rq.URL); err == nil {
				origin = u.Scheme + "://" + u.Host
			}
			mth := strings.ToUpper(rq.HTTPMethod)
			return gate(func(a bool) reply { return fetchOp(&m, rq, a, journal) },
				"send "+mth+" requests to "+origin, "send "+mth+" to "+rq.URL, "write", true)
		}
		return reply{Refused: "unknown request"}
	}

	res := &Result{Admission: adm}
	if run != nil {
		res.RunID = run.Header.RunID
	}
	var resMu, encMu sync.Mutex
	var stopped atomic.Bool

	// answer produces the reply to one request: from the record when the run
	// has answered it before, and by acting otherwise.
	answer := func(rq request) (rp reply, replayed bool, err error) {
		// Asking whether a write is allowed changes nothing and is not
		// recorded. Everything else is.
		if run == nil || rq.ID == "" || rq.Method == "canwrite" {
			return act(rq), false, nil
		}
		id := rq.ID
		content := rq
		content.ID = ""
		digest := runlog.Digest(content)
		state, recorded, err := run.Lookup(id, digest)
		if err != nil {
			return reply{}, false, fmt.Errorf("%w; the record cannot be replayed into a program that is not following it", err)
		}
		switch state {
		case runlog.Finished:
			var old reply
			if err := json.Unmarshal(recorded, &old); err != nil {
				return reply{}, false, fmt.Errorf("the record of %s cannot be read: %w", id, err)
			}
			resMu.Lock()
			res.Replayed++
			resMu.Unlock()
			return old, true, nil
		case runlog.Interrupted:
			// Nothing was recorded after it began. A read is asked again.
			// A change may or may not have happened, and doing it again
			// could do it twice: it goes to a person (34 section 11.12).
			if effect := run.Effect(id); effect != "read" {
				logf("  UNKNOWN  %s %s  (%s, in progress when the earlier run stopped)", rq.Method, id, effect)
				rp = reply{Unknown: true, Refused: "the outcome of this " + effect + " is unknown: an earlier run stopped while it was in progress"}
				b, _ := json.Marshal(rp)
				if err := run.End(id, "unknown", b, time.Now().UTC()); err != nil {
					return reply{}, false, err
				}
				resMu.Lock()
				res.Unknown++
				resMu.Unlock()
				return rp, false, nil
			}
		}
		if err := run.Begin(id, rq.Method, digest, effectOf(rq), time.Now().UTC()); err != nil {
			return reply{}, false, err
		}
		if o.stopDuring == id {
			stopped.Store(true)
			return reply{}, false, errInterrupted
		}
		rp = act(rq)
		outcome := "ran"
		if rp.Refused != "" {
			outcome = "refused"
		}
		b, _ := json.Marshal(rp)
		if err := run.End(id, outcome, b, time.Now().UTC()); err != nil {
			return reply{}, false, err
		}
		return rp, false, nil
	}

	rd := bufio.NewReaderSize(fromGuestR, 1<<20)
	var final *request
	var wg sync.WaitGroup
	var firstErr error
	inFlight := make(chan struct{}, 8)
	answered := 0
	for final == nil && !stopped.Load() {
		line, rerr := rd.ReadBytes('\n')
		if len(line) > 0 {
			var rq request
			if jerr := json.Unmarshal(line, &rq); jerr != nil {
				logf("guest      non-protocol output: %s", strings.TrimSpace(string(line)))
			} else if stopped.Load() {
				// The run has stopped. A request that arrives now is not
				// acted on: a machine that has lost power acts on nothing.
				break
			} else if rq.Method == "return" {
				final = &rq
			} else {
				// Requests are acted on as they arrive and answered as they
				// finish. The id on a reply says which request it answers.
				wg.Add(1)
				inFlight <- struct{}{}
				go func(rq request) {
					defer wg.Done()
					defer func() { <-inFlight }()
					rp, replayed, err := answer(rq)
					resMu.Lock()
					defer resMu.Unlock()
					if err != nil {
						if firstErr == nil {
							firstErr = err
						}
						stopped.Store(true)
						toGuestW.Close()
						return
					}
					if stopped.Load() {
						return
					}
					answered++
					if o.stopAfter > 0 && answered >= o.stopAfter {
						// The answer is on record and the program never
						// hears it: the machine stopped in between.
						stopped.Store(true)
						if firstErr == nil {
							firstErr = errInterrupted
						}
						toGuestW.Close()
						return
					}
					if rp.Refused != "" {
						res.Refused++
					} else if !replayed && rq.Method != "tools" && rq.Method != "canwrite" {
						res.Ran++
					}
					rp.ID = rq.ID
					encMu.Lock()
					werr := enc.Encode(rp)
					encMu.Unlock()
					if werr != nil && firstErr == nil {
						firstErr = werr
					}
				}(rq)
			}
		}
		if rerr != nil {
			break
		}
	}
	wg.Wait()
	if firstErr != nil {
		toGuestW.Close()
		go io.Copy(io.Discard, fromGuestR)
		stopGuest()
		<-done
		return nil, firstErr
	}
	toGuestW.Close()
	go io.Copy(io.Discard, fromGuestR)
	if err := <-done; err != nil {
		logf("guest exit %v", err)
	}
	if guestErr.Len() > 0 {
		logf("guest stderr:\n%s", strings.TrimRight(guestErr.String(), "\n"))
	}
	logf("%d call(s) and command(s) run, %d refused, in %s", res.Ran, res.Refused, time.Since(t0).Round(time.Millisecond))
	if final == nil {
		return nil, fmt.Errorf("the primitive ended without a result")
	}
	res.Exit, res.Stdout, res.Stderr = final.Exit, final.Stdout, final.Stderr
	if run != nil {
		outcome := "completed"
		if res.Unknown > 0 {
			outcome = "completed_with_unknown"
		}
		if err := run.Finish(outcome, time.Now().UTC()); err != nil {
			return nil, err
		}
		if res.Replayed > 0 {
			logf("%d request(s) were answered from the record and not made again", res.Replayed)
		}
	}
	return res, nil
}

// guestConfig is the whole of what a guest is given. Kept as one function so a
// test can assert against the real object.
// The preludes are the "SDK preloaded into the interpreter" of doc 34 section
// 13.3. The author's file never contains protocol code: it calls tap.exec and
// prints. print is captured, because stdout is the wire.
const pyPrelude = `
import sys, json, io
_in, _out = sys.stdin, sys.stdout
class _Tap:
    _n = 0
    def _send(self, o):
        _Tap._n += 1; o["id"] = "r" + str(_Tap._n)
        _out.write(json.dumps(o) + "\n"); _out.flush()
        return o["id"]
    def _ask(self, o):
        want = self._send(o)
        while True:
            r = json.loads(_in.readline())
            if r.get("id") == want: return r
    def _value(self, r):
        if r.get("refused"): raise PermissionError(r["refused"])
        if r.get("violation"):
            e = ValueError(r.get("stderr")); e.landed = bool(r.get("landed")); raise e
        if r.get("exit"): raise RuntimeError(r.get("stderr") or "tool call failed")
        try: return json.loads(r.get("result") or "null")
        except ValueError: return r.get("result")
    def call(self, alias, arguments=None):
        return self._value(self._ask({"method": "call", "alias": alias, "arguments": arguments or {}}))
    def call_many(self, calls):
        """Send every call, then read every answer. Answers come back in the
        order the calls were given, whatever order they finished in. A call
        that failed or was refused is returned as its exception."""
        ids = [self._send({"method": "call", "alias": a, "arguments": g or {}}) for a, g in calls]
        got = {}
        while len(got) < len(ids):
            r = json.loads(_in.readline())
            if r.get("id") in ids: got[r["id"]] = r
        out = []
        for i in ids:
            try: out.append(self._value(got[i]))
            except Exception as e: out.append(e)
        return out
    def _cap(self, o):
        r = self._ask(o)
        if r.get("refused"): raise PermissionError(r["refused"])
        if r.get("exit"): raise OSError(r.get("stderr") or "failed")
        return r
    def read(self, path): return self._cap({"method": "read", "path": path}).get("result", "")
    def write(self, path, text): self._cap({"method": "write", "path": path, "stdin": text})
    def fetch(self, url, method="GET", body="", headers=None):
        r = self._cap({"method": "fetch", "url": url, "http_method": method, "stdin": body, "headers": headers or {}})
        return {"status": r.get("status"), "body": r.get("result", "")}
    def tools(self):
        return self._ask({"method": "tools"}).get("tools") or []
    def exec(self, command, args=(), stdin=""):
        return self._ask({"method": "exec", "command": command, "args": list(args), "stdin": stdin})
tap = _Tap()
_buf, _err, _exit = io.StringIO(), io.StringIO(), 0
sys.stdout, sys.stderr = _buf, _err
try:
    exec(compile(%s, "main.py", "exec"), {"tap": tap, "__name__": "__main__"})
except SystemExit as e:
    _exit = int(e.code or 0)
except BaseException as e:
    _err.write(type(e).__name__ + ": " + str(e) + "\n"); _exit = 1
_out.write(json.dumps({"method": "return", "stdout": _buf.getvalue(), "stderr": _err.getvalue(), "exit": _exit}) + "\n"); _out.flush()
`

const jsPrelude = `
import * as std from "qjs:std";
let _buf = "", _err = "", _exit = 0;
const _line = (a) => a.map((x) => (typeof x === "string" ? x : JSON.stringify(x))).join(" ") + "\n";
globalThis.print = (...a) => { _buf += _line(a); };
globalThis.console = { log: (...a) => { _buf += _line(a); }, error: (...a) => { _err += _line(a); } };
let _n = 0;
const _send = (o) => { o.id = "r" + (++_n); std.out.puts(JSON.stringify(o) + "\n"); std.out.flush(); return o.id; };
const _ask = (o) => { const want = _send(o); for (;;) { const r = JSON.parse(std.in.getline()); if (r.id === want) return r; } };
const _value = (r) => {
  if (r.refused) throw new Error(r.refused);
  if (r.violation) { const e = new TypeError(r.stderr); e.landed = !!r.landed; throw e; }
  if (r.exit) throw new Error(r.stderr || "tool call failed");
  try { return JSON.parse(r.result ?? "null"); } catch (e) { return r.result; }
};
globalThis.tap = {
  call(alias, args = {}) { return _value(_ask({ method: "call", alias, arguments: args })); },
  // Sends every call, then reads every answer. Results are in the order the
  // calls were given. A call that failed is returned as its Error.
  callMany(calls) {
    const ids = calls.map(([alias, args]) => _send({ method: "call", alias, arguments: args ?? {} }));
    const got = {};
    while (Object.keys(got).length < ids.length) { const r = JSON.parse(std.in.getline()); if (ids.includes(r.id)) got[r.id] = r; }
    return ids.map((i) => { try { return _value(got[i]); } catch (e) { return e; } });
  },
  tools() { return _ask({ method: "tools" }).tools ?? []; },
  _cap(o) { const r = _ask(o); if (r.refused) throw new Error(r.refused); if (r.exit) throw new Error(r.stderr || "failed"); return r; },
  read(path) { return this._cap({ method: "read", path }).result ?? ""; },
  write(path, text) { this._cap({ method: "write", path, stdin: text }); },
  fetch(url, method = "GET", body = "", headers = {}) { const r = this._cap({ method: "fetch", url, http_method: method, stdin: body, headers }); return { status: r.status, body: r.result ?? "" }; },
  exec(command, args = [], stdin = "") { return _ask({ method: "exec", command, args, stdin }); },
};
globalThis.std = std;
try { (0, eval)(%s); } catch (e) { _err += String(e) + "\n"; _exit = 1; }
std.out.puts(JSON.stringify({ method: "return", stdout: _buf, stderr: _err, exit: _exit }) + "\n"); std.out.flush();
`

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func guestConfig(kind, pyLib, script string, args []string) wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().WithSysWalltime().WithSysNanotime()
	switch kind {
	case "py":
		fsc := wazero.NewFSConfig()
		if pyLib != "" {
			fsc = fsc.WithReadOnlyDirMount(pyLib, "/usr")
		}
		cfg = cfg.WithFSConfig(fsc).
			WithArgs(append([]string{"python", "-c", fmt.Sprintf(pyPrelude, quote(script))}, args...)...)
	case "js":
		cfg = cfg.WithArgs(append([]string{"qjs", "--module", "-e", fmt.Sprintf(jsPrelude, quote(script))}, args...)...)
	default:
		cfg = cfg.WithArgs("sh")
	}
	return cfg
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, "host  "+f+"\n", a...) }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "host  fatal:", err)
		os.Exit(1)
	}
}
