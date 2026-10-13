// host is the runner. It loads a package whose
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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/Telara-Labs/TAP-Runtime/discover"
	"github.com/Telara-Labs/TAP-Runtime/internal/component"
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
	// Input is what a run request hands the primitive it runs.
	Input map[string]any `json:"input,omitempty"`
	// Only the host may resolve a call's effect. Keep one assessment for
	// its journal, approval and dispatch; the guest cannot supply it.
	callAssessment *callAssessment
	// The host names the run a run request starts, and keeps what it learned.
	childRun    string
	resumeChild bool
	info        *childInfo
}

type callAssessment struct {
	effect, refused, nested string
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
	// Uses lists the aliases of the primitives this one may run.
	Uses   []string `json:"uses,omitempty"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
	Exit   int      `json:"exit"`
	Stdin  string   `json:"stdin"`
	Status int      `json:"status,omitempty"`
	// Gated is set when the only thing missing is a person's agreement.
	Gated bool `json:"gated,omitempty"`
}

// Approver is asked, on a person's behalf, whether a primitive may make a
// kind of change, and how many times. The approval carries the ceiling,
// and reaching it asks again.
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
	Package string
	// ExpectedDigest pins an MCP-discovered package to the bytes the caller
	// selected. The CLI leaves it empty when it runs a local path directly.
	ExpectedDigest string
	Args           []string
	Approve        Approver
	// FetchOrigins are owner-granted exact origins loaded from the digest's
	// trust record by serve. MCP tap_run never accepts these as arguments.
	FetchOrigins []string
	// FetchGrants are read fetch kinds the person approved earlier for this
	// exact digest; RememberGrant keeps a read fetch approved in a prompt.
	FetchGrants   []string
	RememberGrant func(kind string)
	// Choose settles two servers that fit one capability equally well. Nil
	// means nobody can be asked, and the run is refused until a choice is kept
	// (`tap bind`).
	Choose Chooser
	// AskTool asks the client which of its tools fills a capability that
	// nothing else resolved, on a client that does not list its tools. Nil
	// means nobody can be asked, and the run is blocked with the question.
	AskTool   ToolAsker
	Journal   io.Writer
	InterpDir string
	CacheDir  string
	PyLib     string
	Client    string        // claude, codex, or empty to detect
	Bridge    bridge.Bridge // set by the server, which already knows the client
	// MCPURL, when set, has the runner call that MCP server directly instead
	// of borrowing a client's connections: the way a service that runs
	// primitives itself hands them its gateway. MCPHeaderFile holds the header
	// lines sent with every request, such as its Authorization.
	MCPURL        string
	MCPHeaderFile string
	MCPServerName string // caller's connection name, for packages pinning that name
	ReceiptPath   string
	// relay, when set, borrows the connections of a client that makes tool
	// calls when a hook asks it to (relay.go). relayClient names that client.
	relay        *relayRun
	relayClient  string
	relayVersion string
	// VSCodeSocket reaches the TAP extension in VS Code, which calls the
	// editor's tools for the runner (bridge/vscode.go).
	VSCodeSocket string
	// Proc is the agent session the run is for: its working directory, where
	// files resolve and programs start, and its environment. The zero value
	// is this process's own (serve.go, shared.go).
	Proc bridge.Proc

	// RunsDir holds one directory per run. Empty means the user cache
	// directory. NoJournal runs without a record, and so without resume.
	RunsDir   string
	NoJournal bool
	// Resume continues the run with this id instead of starting one.
	Resume string
	// TelemetryPayloads sends what calls were given and what they touched
	// to the OpenTelemetry endpoint, when one is configured. Without it only
	// events are sent.
	TelemetryPayloads bool
	// RetentionDays is how long the record of a run is kept. Records older
	// than this are removed when a run starts. 0 keeps them for ever.
	RetentionDays int

	// stopAfter and stopDuring end the run the way a crash would, for tests:
	// after that many requests have been answered, or once the request with
	// that id has been recorded as begun and before it is acted on.
	stopAfter  int
	stopDuring string

	// tree is this run's place among the primitives that started it. Nil for
	// the primitive that was asked for.
	tree *runTree
	// catalog lists the primitives installed here, where a pinned child is
	// looked for. Nil means the local catalog.
	catalog func() ([]catalogEntry, error)
	// authFor gives a child what was kept for its own digest. Nil means nothing.
	authFor func(digest string) childAuth
	// childHook and stopAfterChild are for tests: the first changes a
	// child's options before it starts, the second ends the run as a crash
	// would, once a child has finished and before its parent heard.
	childHook      func(alias string, co *Options)
	stopAfterChild bool
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
	// Calls counts the tool calls this run made (not replayed, not refused),
	// FailedCalls those that failed, Elapsed the run's wall time and Inputs
	// how many items it was given (runstats.go). They tell the caller what
	// the run cost, which is what an author improves.
	Calls       int
	FailedCalls int
	Elapsed     time.Duration
	Inputs      int
	// Children are the primitives this one ran, with what each did.
	Children []ChildRun
}

// usageText is what `tap` with no arguments, and `tap help`, print.
const usageText = `usage: tap [--approve] [--resume RUN] <package-dir> [args...]
       tap discover [--client AGENTS] [--days N]
       tap setup
       tap remove
       tap install --client <agent>|all|detected [--remove] [--print]
       tap serve
       tap trust PACKAGE-DIR
       tap trust-publisher add|list|remove
       tap bind --client NAME CAPABILITY SERVER[/TOOL]
       tap fetch
       tap diff [--json] <old-package-dir> <new-package-dir>
       tap manifest check|complete <package-dir>
       tap web build --out FILE <package-dir>...
       tap kit export [--out DIR] [<package-dir>...]
       tap hook gemini
       tap cache [prune]
       tap upgrade
       tap version

Start with: tap discover   (finds work you repeat in your agents' history)
Docs: https://github.com/Telara-Labs/TAP-Runtime
`

// upgradeText is what `tap upgrade` prints when the runner was not started
// by the npm package, which is the only install that can replace itself.
const upgradeText = `tap upgrade replaces an npm install of TAP. This runner was not started by npm.
Installed with npm:      npm install -g @telaralabs/tap@latest && tap setup
Built from source:       go install github.com/Telara-Labs/TAP-Runtime/host@latest
Restart your agents afterwards so they start the new runner.
`

// isVersionArg accepts the spellings people try first after an install.
func isVersionArg(arg string) bool {
	switch arg {
	case "version", "--version", "-version", "-v", "-V":
		return true
	}
	return false
}

// unknownCommand reports whether arg reads as a mistyped command rather than
// a package path: a bare word with no path separator that names nothing here.
func unknownCommand(arg string) bool {
	if strings.ContainsAny(arg, `/\.`) {
		return false
	}
	_, err := os.Stat(arg)
	return os.IsNotExist(err)
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "-help") {
		fmt.Print(usageText)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		// The same step the npm package runs after a global install, so
		// every install path has it.
		os.Exit(install(append([]string{"--client", "detected"}, os.Args[2:]...), os.Stdout, os.Stderr, true))
	}
	if len(os.Args) > 1 && os.Args[1] == "remove" {
		// The reverse of setup: disconnect the runner from every agent it
		// can be connected to, installed or not.
		os.Exit(installCommand(append([]string{"--client", "all", "--remove"}, os.Args[2:]...), os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && (os.Args[1] == "upgrade" || os.Args[1] == "update") {
		// The npm package handles this before the runner starts; this runner
		// was started some other way, so it can only say how to replace it.
		fmt.Fprint(os.Stderr, upgradeText)
		os.Exit(2)
	}
	if len(os.Args) > 1 && isVersionArg(os.Args[1]) {
		fmt.Println("tap", version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "discover" {
		// Find recurring work in this machine's agent history and draft it as
		// primitives. Local only; needs no Telara.
		discover.Version = version
		if len(os.Args) > 3 && os.Args[2] == "save" {
			if why := usesCheckDir(os.Args[len(os.Args)-1], nil); why != "" {
				fmt.Fprintln(os.Stderr, "tap:", why)
				os.Exit(1)
			}
		}
		os.Exit(discover.Command(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "trust" {
		os.Exit(trustCommand(takeConfigDir(os.Args[2:]), os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "trust-publisher" {
		os.Exit(trustPublisherCommand(takeConfigDir(os.Args[2:]), os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "bind" {
		os.Exit(bindCommand(takeConfigDir(os.Args[2:]), os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "install" {
		os.Exit(installCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "web" {
		os.Exit(webCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(hookCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "cache" {
		os.Exit(cacheCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "fetch" {
		os.Exit(fetchCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "manifest" {
		os.Exit(manifestCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "kit" {
		os.Exit(kitCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "diff" {
		os.Exit(diffCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "shared-runner" {
		os.Exit(sharedRunnerCommand(os.Args[2:], os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serveEntry(os.Stdin, os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tap:", err)
			os.Exit(1)
		}
		return
	}
	approve := flag.Bool("approve", false, "approve write and destructive actions for this run")
	limit := flag.Int("limit", 0, "with --approve: how many changes of each kind may be made; 0 is no limit")
	journalPath := flag.String("journal", "", "append one JSON line per action")
	interpDir := flag.String("interpreters", "", "interpreter store; default is the user cache directory")
	cacheDir := flag.String("cache", "", "directory for the compiled-interpreter cache")
	client := flag.String("client", "", "agent whose connections to borrow ("+strings.Join(agents.IDs(func(c agents.Client) bool { return c.Bridge }), ", ")+"); detected when empty")
	mcpURL := flag.String("mcp-url", "", "call this MCP server (streamable HTTP) directly instead of borrowing a client's connections")
	mcpHeaderFile := flag.String("mcp-header-file", "", "with --mcp-url: file of header lines to send, such as \"Authorization: Bearer ...\"")
	mcpServerName := flag.String("mcp-server-name", "", "with --mcp-url: logical connection name used by primitive tool pins; defaults to serverInfo.name")
	vscodeSocket := flag.String("vscode-socket", "", "call VS Code's tools through the TAP extension listening on this socket")
	receiptPath := flag.String("receipt", "", "write the admission record as JSON")
	runsDir := flag.String("runs", "", "directory holding one record per run; default is the user cache directory")
	resume := flag.String("resume", "", "continue the run with this id")
	noJournal := flag.Bool("no-record", false, "keep no record of the run; it cannot be resumed")
	otelPayloads := flag.Bool("otel-payloads", false, "with an OpenTelemetry endpoint set: also send what calls were given and what they touched")
	retention := flag.Int("retention-days", 30, "remove the records of runs older than this many days; 0 keeps them for ever")
	pyLib := flag.String("pylib", "", "python standard library directory, mounted read-only")
	// A mistyped flag gets the usage people read, not Go's flag dump.
	flag.Usage = func() { fmt.Fprint(flag.CommandLine.Output(), "\n"+usageText) }
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	if _, err := os.Stat(filepath.Join(flag.Arg(0), "primitive.yaml")); err != nil {
		if unknownCommand(flag.Arg(0)) {
			fmt.Fprintf(os.Stderr, "tap: unknown command %q, and there is no primitive folder by that name.\n\n%s", flag.Arg(0), usageText)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "tap: %s is not a primitive folder (it has no primitive.yaml).\nRun tap with no arguments for usage, or tap discover to find primitives in your agents' history.\n", flag.Arg(0))
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
	askFn := Approver(func(a Ask) Grant {
		// Interactive callers may grant more when a kind's allowance runs
		// out. The CLI cannot ask again: --limit is its total consent for
		// that kind, rather than an allowance to renew on every ask.
		if grant.Limit != Unlimited && a.Done >= grant.Limit {
			return Grant{}
		}
		return grant
	})
	// A call made through VS Code is asked here like any other change. VS Code
	// does not confirm it: called with no invocation token, it ran a tool
	// that is not read-only with nobody asked (live, VS Code 1.140).
	res, err := Run(context.Background(), Options{
		Package: flag.Arg(0), Args: flag.Args()[1:], Journal: journal,
		Approve:   askFn,
		InterpDir: *interpDir, CacheDir: *cacheDir, PyLib: *pyLib, Client: bridgeName(*client), ReceiptPath: *receiptPath,
		MCPURL: *mcpURL, MCPHeaderFile: *mcpHeaderFile, MCPServerName: *mcpServerName, VSCodeSocket: *vscodeSocket,
		RunsDir: *runsDir, Resume: *resume, NoJournal: *noJournal, RetentionDays: *retention, TelemetryPayloads: *otelPayloads,
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
	defer runStarted()()
	pkg, err := preparePackage(o)
	if err != nil {
		return nil, err
	}
	m, script, pkgDigest, tree := pkg.manifest, pkg.script, pkg.digest, pkg.tree
	journalState, err := openRunJournal(o, pkg, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if journalState.journal != nil {
		defer journalState.close()
	}
	run, runsRoot, args := journalState.journal, journalState.runsRoot, journalState.args

	executable, err := prepareInterpreter(o, pkg)
	if err != nil {
		return nil, err
	}
	wasmBytes, sum := executable.wasmBytes, executable.sum
	format, preview3 := executable.format, executable.preview3
	script, kind := executable.script, executable.kind
	journal := o.Journal
	if journal == nil {
		journal = io.Discard
	}

	preparedAdmission, err := prepareAdmission(o, pkg, run)
	if err != nil {
		return nil, err
	}
	defer preparedAdmission.close()
	adm, br := preparedAdmission.admission, preparedAdmission.bridge

	// Events of the run go to an OpenTelemetry endpoint when one is set
	// A collector that cannot be reached does not stop
	// a run: the run is the point, and its record is on this machine.
	runID, clientName := "", ""
	if run != nil {
		runID = run.Header.RunID
	}
	if adm != nil {
		clientName = adm.Client + " " + adm.Version
	}
	markerRef, artifactDigest := readRegistryMarker(o.Package)
	tel, terr := startTelemetry(ctx, runIdentity{
		Publisher: m.Metadata.Publisher, Name: m.Metadata.Name, Version: m.Metadata.Version,
		PackageDigest: pkgDigest, ArtifactDigest: artifactDigest, Ref: markerRef,
	}, runID, clientName, o.TelemetryPayloads)
	if terr != nil {
		logf("telemetry  not started: %v", terr)
	}
	outcome := "failed"
	var telRes *Result
	if tel != nil {
		journal = io.MultiWriter(journal, tel)
		defer func() { tel.stop(outcome, telRes) }()
	}

	// The program is stopped by cancelling its context, which the engine
	// checks for. Closing the engine under a running program is a race.
	guestCtx, stopGuest := context.WithCancel(ctx)
	defer stopGuest()
	rc := wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(guestMemoryPages)
	for _, k := range unenforcedLimits {
		if _, ok := m.Execution.Limits[k]; ok {
			logf("limit      %s is declared and this runner does not enforce it", k)
		}
	}
	pruneOnFirstStart(o.InterpDir, o.CacheDir, map[string]bool{sum: true})
	var rt wazero.Runtime
	if !preview3 {
		if cache, err := compilationCache(o.CacheDir, sum); err == nil {
			rc = rc.WithCompilationCache(cache)
		} else {
			logf("cache      none: %v", err)
		}
		rt = wazero.NewRuntimeWithConfig(ctx, rc)
		defer rt.Close(ctx)
	}

	t0 := time.Now()
	// A component gets its WASI 0.2 interfaces from package component, which
	// supplies the same streams, clocks and random bytes and nothing more.
	// It is not given the Preview 1 host module. A Preview 3 component is
	// compiled by wasmtime, which runs it in a process of its own.
	var compiled wazero.CompiledModule
	var prog *component.Program
	var p3prog *wasmtimeProgram
	if preview3 {
		p3prog, err = prepareWasmtime(o.InterpDir, o.CacheDir, sum, m.Execution.Entrypoint, wasmBytes)
		if err == nil && o.Resume != "" {
			logf("note       a Preview 3 component reads the real clock and random bytes, which are not recorded; this resumed run replays its answered requests but not those readings")
		}
	} else {
		unlock := compileLock(wasmBytes)
		if format == component.Component {
			prog, err = component.Load(ctx, rt, wasmBytes)
		} else {
			wasi_snapshot_preview1.MustInstantiate(ctx, rt)
			compiled, err = rt.CompileModule(ctx, wasmBytes)
		}
		unlock()
	}
	if err != nil {
		return nil, err
	}
	logf("compiled WebAssembly in %s", time.Since(t0).Round(time.Millisecond))

	// OS pipes, not io.Pipe: io.Pipe is unbuffered, so a guest flushing stdout
	// while the host writes its reply deadlocks both sides.
	toGuestR, toGuestW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer toGuestR.Close()
	defer toGuestW.Close()
	fromGuestR, fromGuestW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer fromGuestR.Close()
	defer fromGuestW.Close()
	// Canceling the engine does not interrupt a blocked WASI stdin read.
	// Close its input on cancellation so it can finish before stdout is
	// drained, including when a canceled host command has no reply to send.
	stopInputClose := context.AfterFunc(guestCtx, func() { toGuestW.Close() })
	defer stopInputClose()
	guestErr := guestStderr{cappedBuffer: cappedBuffer{max: maxGuestStderr}}
	cfg := guestConfig(kind, o.PyLib, string(script), args).
		WithStdin(toGuestR).WithStdout(fromGuestW).WithStderr(&guestErr)
	if run != nil {
		cfg = observed(cfg, run.Observed)
	}

	// The clock starts when the program does, not while the interpreter is
	// being compiled.
	limit := timeoutFor(m.Execution.TimeoutSeconds)
	if tree.timeCap > 0 && tree.timeCap < limit {
		limit = tree.timeCap
	}
	bud := newBudget(limit, stopGuest)
	defer bud.stop()
	chain := append(append([]*budget{}, tree.ancestors...), bud)
	done := make(chan error, 1)
	go func() {
		var err error
		if p3prog != nil {
			err = p3prog.run(guestCtx, args, toGuestR, fromGuestW, &guestErr)
		} else if prog != nil {
			err = prog.Run(guestCtx, componentConfig(args, toGuestR, fromGuestW, &guestErr, run))
		} else {
			_, err = rt.InstantiateModule(guestCtx, compiled, cfg)
		}
		stopGuest() // a finished guest has no remaining host work to dispatch
		fromGuestW.Close()
		done <- err
	}()

	enc := json.NewEncoder(toGuestW)
	if kind == "sh" {
		if err := enc.Encode(map[string]any{"script": string(script), "args": args}); err != nil {
			return nil, err
		}
	}

	approval := newApprovalBudget(o, chain, tree, journal)

	dispatcher := newRequestDispatcher(guestCtx, o, m, tree, journal, run, runsRoot, br, adm, bud, chain, tel, approval)

	res := &Result{Admission: adm}
	telRes = res
	if run != nil {
		res.RunID = run.Header.RunID
	}
	session := runSession{
		dispatcher: dispatcher, options: o, result: res,
		guestCtx: guestCtx, stopGuest: stopGuest, toGuest: toGuestW,
		fromGuest: fromGuestR, encoder: enc, done: done, guestErr: &guestErr,
		budget: bud, limit: limit, started: t0,
	}
	result, finalOutcome, runErr := session.serve()
	outcome = finalOutcome
	if result != nil {
		telRes = result
	}
	return result, runErr
}

// guestConfig is the whole of what a guest is given. Kept as one function so a
// test can assert against the real object.
// The preludes are the SDK preloaded into the interpreter. The author's file never contains protocol code: it calls tap.exec and
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
    def _err(self, cls, code, text):
        e = cls(text); e.code = code; return e
    def _value(self, r):
        if r.get("refused"): raise self._err(PermissionError, "refused", r["refused"])
        if r.get("violation"):
            e = self._err(ValueError, "violation", r.get("stderr")); e.landed = bool(r.get("landed")); raise e
        if r.get("exit"): raise self._err(RuntimeError, "failed", r.get("stderr") or "tool call failed")
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
    def run(self, alias, input=None):
        """Run a primitive listed under uses and return its output. A child
        that fails raises, as a failed call does."""
        return self._value(self._ask({"method": "run", "alias": alias, "input": input or {}}))
    def run_many(self, runs):
        """Like call_many, for primitives listed under uses: runs is a list
        of (alias, input) pairs."""
        ids = [self._send({"method": "run", "alias": a, "input": g or {}}) for a, g in runs]
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
        if r.get("refused"): raise self._err(PermissionError, "refused", r["refused"])
        if r.get("exit"): raise self._err(OSError, "failed", r.get("stderr") or "failed")
        return r
    def read(self, path): return self._cap({"method": "read", "path": path}).get("result", "")
    def write(self, path, text): self._cap({"method": "write", "path": path, "stdin": text})
    def fetch(self, url, method="GET", body="", headers=None):
        r = self._cap({"method": "fetch", "url": url, "http_method": method, "stdin": body, "headers": headers or {}})
        return {"status": r.get("status"), "body": r.get("result", "")}
    def tools(self):
        return self._ask({"method": "tools"}).get("tools") or []
    def exec(self, command, args=(), stdin=""):
        """A command that exits non-zero is an ordinary result and is
        returned. A command the host refused to run did not exit at all, so
        it raises."""
        r = self._ask({"method": "exec", "command": command, "args": list(args), "stdin": stdin})
        if r.get("refused"): raise self._err(PermissionError, "refused", r["refused"])
        return r
tap = _Tap()
# Programs write "import tap" (Codex and Claude both did); the runner's SDK is
# that module, so the import finds it instead of failing.
sys.modules["tap"] = tap
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
// Every error carries code: "refused", "violation" or "failed", so a program
// can tell them apart without reading the message.
const _fail = (E, code, text) => { const e = new E(text); e.code = code; return e; };
const _value = (r) => {
  if (r.refused) throw _fail(Error, "refused", r.refused);
  if (r.violation) { const e = _fail(TypeError, "violation", r.stderr); e.landed = !!r.landed; throw e; }
  if (r.exit) throw _fail(Error, "failed", r.stderr || "tool call failed");
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
  // Runs a primitive listed under uses and returns its output. A child that
  // fails throws, as a failed call does.
  run(alias, input = {}) { return _value(_ask({ method: "run", alias, input })); },
  // Sends every run, then reads every answer, as callMany does.
  runMany(runs) {
    const ids = runs.map(([alias, input]) => _send({ method: "run", alias, input: input ?? {} }));
    const got = {};
    while (Object.keys(got).length < ids.length) { const r = JSON.parse(std.in.getline()); if (ids.includes(r.id)) got[r.id] = r; }
    return ids.map((i) => { try { return _value(got[i]); } catch (e) { return e; } });
  },
  tools() { return _ask({ method: "tools" }).tools ?? []; },
  _cap(o) { const r = _ask(o); if (r.refused) throw _fail(Error, "refused", r.refused); if (r.exit) throw _fail(Error, "failed", r.stderr || "failed"); return r; },
  read(path) { return this._cap({ method: "read", path }).result ?? ""; },
  write(path, text) { this._cap({ method: "write", path, stdin: text }); },
  fetch(url, method = "GET", body = "", headers = {}) { const r = this._cap({ method: "fetch", url, http_method: method, stdin: body, headers }); return { status: r.status, body: r.result ?? "" }; },
  // A non-zero exit is a result. A refusal is not an exit, so it throws.
  exec(command, args = [], stdin = "") { const r = _ask({ method: "exec", command, args, stdin }); if (r.refused) throw _fail(Error, "refused", r.refused); return r; },
};
globalThis.std = std;
try { (0, eval)(%s); } catch (e) { _err += String(e) + "\n"; _exit = 1; }
std.out.puts(JSON.stringify({ method: "return", stdout: _buf, stderr: _err, exit: _exit }) + "\n"); std.out.flush();
`

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func guestConfig(kind, pyLib, script string, args []string) wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().WithSysWalltime().WithSysNanotime()
	switch kind {
	case "wasm":
		cfg = cfg.WithArgs(append([]string{"primitive"}, args...)...)
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

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, "tap  "+f+"\n", a...) }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap:", err)
		os.Exit(1)
	}
}

// readFetchKind is a gate kind for a fetch that changes nothing.
func readFetchKind(kind string) bool {
	return strings.HasPrefix(kind, "send GET requests to ") || strings.HasPrefix(kind, "send HEAD requests to ")
}
