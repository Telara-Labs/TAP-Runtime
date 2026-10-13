package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/Telara-Labs/TAP-Runtime/internal/component"
	runlog "github.com/Telara-Labs/TAP-Runtime/journal"
)

// preparedPackage is the validated, immutable input to one runner invocation.
// Keeping the manifest, source bytes, digest, and use tree together makes the
// admission boundary explicit before the runner opens any runtime resources.
type preparedPackage struct {
	manifest manifest
	raw      []byte
	script   []byte
	digest   string
	tree     *runTree
}

func preparePackage(o Options) (*preparedPackage, error) {
	raw, err := os.ReadFile(filepath.Join(o.Package, "primitive.yaml"))
	if err != nil {
		return nil, err
	}
	loaded, err := mf.Parse(raw)
	if err != nil {
		return nil, err
	}
	if problems := loaded.RunProblems(); len(problems) > 0 {
		return nil, fmt.Errorf("primitive.yaml cannot be run:\n  - %s", strings.Join(problems, "\n  - "))
	}
	if problems := declarationProblems(loaded); len(problems) > 0 {
		return nil, fmt.Errorf("primitive.yaml declares what this runner will not allow:\n  - %s", strings.Join(problems, "\n  - "))
	}
	if why := attestationRefusal(o.Package, loaded); why != "" {
		return nil, errors.New(why)
	}
	m := *loaded
	// A subprocess is not contained. This runner runs the contained tier only.
	if m.Runtime() != mf.RuntimeWasm {
		return nil, fmt.Errorf("execution.runtime is %s; this runner runs %s only", m.Runtime(), mf.RuntimeWasm)
	}
	script, err := os.ReadFile(filepath.Join(o.Package, m.Execution.Entrypoint))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append(append([]byte{}, raw...), script...))
	digest := hex.EncodeToString(sum[:])
	if o.ExpectedDigest != "" && digest != o.ExpectedDigest {
		return nil, fmt.Errorf("the selected primitive changed since discovery; search again")
	}

	// Every primitive this one uses is found and checked against what its user
	// declared before anything runs.
	tree := o.tree
	if tree == nil {
		tree = newRootTree(dispatchesFor(m.Execution.Limits), m.Metadata.Name, digest)
	}
	if len(m.Uses) > 0 {
		if o.relay != nil {
			return nil, fmt.Errorf("%s uses other primitives, which the %s client cannot run yet: its tool calls are made one program at a time", m.Metadata.Name, o.relayClient)
		}
		if o.tree == nil {
			entries, err := localEntries(o.catalog)
			if err != nil {
				return nil, err
			}
			if tree.uses, err = resolveUses(entries, &m, digest); err != nil {
				return nil, err
			}
		}
	}
	return &preparedPackage{manifest: m, raw: raw, script: script, digest: digest, tree: tree}, nil
}

// runJournalState owns the run record and the arguments selected by its
// start/resume mode. The caller retains the same close lifetime as Run had
// before this state boundary was introduced.
type runJournalState struct {
	journal  *runlog.Journal
	runsRoot string
	args     []string
}

func openRunJournal(o Options, pkg *preparedPackage, started time.Time) (*runJournalState, error) {
	s := &runJournalState{args: o.Args}
	if !o.NoJournal {
		root := o.RunsDir
		if root == "" {
			var err error
			if root, err = cacheSub("runs"); err != nil {
				return nil, err
			}
		}
		s.runsRoot = root
		if n := runlog.Sweep(root, o.RetentionDays, started); n > 0 {
			logf("removed the records of %d run(s) older than %d days", n, o.RetentionDays)
		}
		if o.Resume != "" {
			run, err := runlog.Open(root, o.Resume)
			if err != nil {
				return nil, err
			}
			if run.Header.PackageDigest != pkg.digest {
				run.Close()
				return nil, fmt.Errorf("run %s was started with a different version of this package; it cannot be continued with this one", o.Resume)
			}
			s.journal = run
			// A resumed run is the same run, with the same arguments.
			s.args = run.Header.Args
			done, open := run.Counts()
			logf("resuming   %s: %d request(s) already answered, %d left unanswered", o.Resume, done, open)
		} else {
			abs, _ := filepath.Abs(o.Package)
			hdr := runlog.Header{RunID: runlog.NewRunID(started), Package: abs, PackageDigest: pkg.digest, Args: o.Args, Started: started,
				ParentRunID: pkg.tree.parentRun, ParentRequestID: pkg.tree.parentReq}
			if pkg.tree.runID != "" {
				hdr.RunID = pkg.tree.runID
			}
			run, err := runlog.CreateWithManifest(root, hdr, pkg.raw)
			if err != nil {
				return nil, err
			}
			s.journal = run
			logf("run        %s", run.Header.RunID)
		}
	} else if o.Resume != "" {
		return nil, fmt.Errorf("a run cannot be resumed without its record")
	}
	return s, nil
}

func (s *runJournalState) close() {
	if s != nil && s.journal != nil {
		_ = s.journal.Close()
	}
}

// preparedInterpreter describes the immutable executable bytes and runtime
// kind selected after package admission.
type preparedInterpreter struct {
	wasmBytes   []byte
	interpreter interpreter
	sum         string
	format      component.Format
	preview3    bool
	preview3Why string
	script      []byte
	kind        string
}

func prepareInterpreter(o Options, pkg *preparedPackage) (*preparedInterpreter, error) {
	state := &preparedInterpreter{script: pkg.script}
	m := pkg.manifest
	var err error
	if filepath.Ext(m.Execution.Entrypoint) == ".wasm" {
		// The package already contains the compiled program. Building source
		// belongs to authoring; run grants no compiler or native subprocess.
		state.wasmBytes, state.sum = state.script, digest(state.script)
		state.interpreter = interpreter{Kind: "wasm", File: m.Execution.Entrypoint, SHA256: state.sum}
		if state.format, err = component.Detect(state.wasmBytes); err != nil {
			return nil, fmt.Errorf("entrypoint %s: %w", m.Execution.Entrypoint, err)
		}
		if state.format == component.Component {
			if state.preview3, state.preview3Why, err = component.Preview3(state.wasmBytes); err != nil {
				return nil, fmt.Errorf("entrypoint %s: %w", m.Execution.Entrypoint, err)
			}
		}
	} else {
		store, err := storeDir(o.InterpDir)
		if err != nil {
			return nil, err
		}
		state.wasmBytes, state.interpreter, state.sum, err = obtain(store, m.Execution.Entrypoint)
		if err != nil {
			return nil, err
		}
	}
	state.kind = state.interpreter.Kind
	if state.kind == "ts" {
		js, err := stripTypes(string(state.script), m.Execution.Entrypoint)
		if err != nil {
			return nil, err
		}
		state.script, state.kind = []byte(js), "js"
	}

	if state.kind == "wasm" {
		what := "compiled WASI Preview 1 module"
		switch {
		case state.preview3:
			what = "compiled WASI Preview 3 component, run in wasmtime " + wasmtimeVersion + ": " + state.preview3Why
		case state.format == component.Component:
			what = "compiled WASI Preview 2 component"
		}
		logf("package    %s  entrypoint %s (%s)", m.Metadata.Name, m.Execution.Entrypoint, what)
		logf("module     %s (%d bytes) sha256:%s", state.interpreter.File, len(state.wasmBytes), state.sum[:12])
	} else {
		logf("package    %s  entrypoint %s (source, not compiled)", m.Metadata.Name, m.Execution.Entrypoint)
		logf("interpreter %s (%d bytes) sha256:%s", state.interpreter.File, len(state.wasmBytes), state.sum[:12])
	}
	if state.interpreter.SHA256 == "" {
		logf("WARNING    this interpreter has no pinned digest (a runner built from source); it is trusted from the first read. Releases pin it.")
	}
	for _, c := range m.Commands {
		logf("declared   %-8s %-18s %s", c.Command, strings.Join(c.Args, " "), c.Effect)
	}
	return state, nil
}

// preparedAdmission owns only a bridge opened by this invocation. Browser
// tabs opened while resolving its capabilities are closed before that bridge,
// matching the cleanup order of the original Run defers.
type preparedAdmission struct {
	bridge      bridge.Bridge
	admission   *admission
	closeBridge bool
}

func (s *preparedAdmission) close() {
	if s == nil {
		return
	}
	if s.admission != nil {
		s.admission.closeBrowsers()
	}
	if s.closeBridge && s.bridge != nil {
		s.bridge.Close()
	}
}

func prepareAdmission(o Options, pkg *preparedPackage, run *runlog.Journal) (_ *preparedAdmission, err error) {
	m := pkg.manifest
	s := &preparedAdmission{bridge: o.Bridge}
	if len(m.Tools) == 0 {
		return s, nil
	}
	if s.bridge == nil && o.relay != nil {
		// The client does not say which tools it has: the declared capabilities
		// are resolved below (resolve.go).
		s.bridge = newRelayBridge(o.relay, o.relayClient, o.relayVersion, m.Tools, o.Proc.Wd())
	}
	if s.bridge == nil && o.VSCodeSocket != "" {
		s.bridge, err = bridge.NewVSCode(o.VSCodeSocket)
		s.closeBridge = err == nil
		if err != nil {
			return nil, err
		}
	}
	if s.bridge == nil && o.MCPURL != "" {
		s.bridge, err = openMCP(o.MCPURL, o.MCPHeaderFile, o.MCPServerName)
		s.closeBridge = err == nil
		if err != nil {
			return nil, err
		}
	}
	if s.bridge == nil {
		c := o.Client
		if c == "" {
			c = detectClient()
		}
		if c == "codex" && o.Proc.Getenv("CODEX_SANDBOX_NETWORK_DISABLED") == "1" {
			return nil, fmt.Errorf("this command runs inside Codex's sandbox, which has no network, so the runner cannot borrow Codex's connections from here; save the package (tap discover save, approved to run outside the sandbox) and run it with the tap_run tool")
		}
		s.bridge, err = openBridge(c, o.Proc, &m)
		s.closeBridge = err == nil
		if err != nil {
			if why := unlendableTools(c, &m); why != "" {
				return nil, fmt.Errorf("%s", why)
			}
			return nil, err
		}
	}
	defer func() {
		if err != nil {
			s.close()
		}
	}()

	// A client that cannot list its tools (Kilo, the Gemini relay) has each
	// declared capability resolved on its own: pins, a kept mapping, known
	// names on configured servers, then one question.
	decls := m.Tools
	var resolved map[string]resolution
	if pb, ok := s.bridge.(bridge.PinnedOnly); ok {
		decls, resolved, err = resolveUnlisted(&m, pb, newFileBindings(defaultBindingsPath()), o.AskTool)
		if err != nil {
			if run != nil && o.Resume == "" {
				if recordErr := run.Finish("refused", time.Now().UTC()); recordErr != nil {
					err = errors.Join(err, fmt.Errorf("recording admission refusal: %w", recordErr))
				}
			}
			return nil, err
		}
	}
	choose := o.Choose
	if choose != nil {
		choose = func(p Pick) (string, bool) {
			p.Primitive = m.Metadata.Name
			return o.Choose(p)
		}
	}
	s.admission, err = admitWith(newFileBindings(defaultBindingsPath()), choose, decls, s.bridge, m.Capabilities...)
	if err != nil {
		// A fresh run refused admission before any backend call began. A resumed
		// run may already contain an unanswered write; leave its record recoverable.
		if run != nil && o.Resume == "" {
			if recordErr := run.Finish("refused", time.Now().UTC()); recordErr != nil {
				err = errors.Join(err, fmt.Errorf("recording admission refusal: %w", recordErr))
			}
		}
		return nil, err
	}
	noteResolutions(s.admission, resolved)
	logf("client     %s %s", s.admission.Client, s.admission.Version)
	if !s.admission.Tested {
		logf("WARNING    this runner was not run against %s %s; proceeding (recorded on the receipt)", s.admission.Client, s.admission.Version)
	}
	for _, b := range s.admission.Bindings {
		note := "contract not checked: the client gives no schema"
		switch {
		case b.ContractChecked:
			note = "contract checked against the tool's schema " + b.Schema[:19]
		case b.Pinned:
			note = "pinned"
		case s.bridge.HasSchemas():
			note = "bound by name: the manifest carries no contract for it"
		}
		if b.ResultChecked {
			note += "; answers are checked"
		}
		if b.ResolvedBy != "" {
			note += "; resolved by " + b.ResolvedBy
		}
		logf("bound      %-10s %-26s -> %s / %s  score %.3f  declared %s, annotated %s  (%s)", b.Alias, b.Capability, b.Server, b.Tool, b.Score, b.Declared, b.Annotated, note)
		if b.Gated {
			logf("           %-10s its server says nothing about what it does; treated as a write", b.Alias)
		}
	}
	if o.ReceiptPath != "" {
		j, _ := json.MarshalIndent(s.admission, "", "  ")
		if err := os.WriteFile(o.ReceiptPath, append(j, '\n'), 0o600); err != nil {
			return nil, err
		}
	}
	return s, nil
}
