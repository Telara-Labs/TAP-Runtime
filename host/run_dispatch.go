package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
	"github.com/Telara-Labs/TAP-Runtime/journal"
	"go.opentelemetry.io/otel/trace"
)

// requestDispatcher is the boundary between the guest protocol and host
// actions. It carries the shared run tree, approval ledger, and sandbox
// deadline so children and concurrent requests keep the same policy state.
type requestDispatcher struct {
	ctx       context.Context
	options   Options
	manifest  manifest
	tree      *runTree
	journal   io.Writer
	run       *journal.Journal
	runsRoot  string
	bridge    bridge.Bridge
	admission *admission
	budget    *budget
	chain     []*budget
	telemetry *telemetry
	approval  *approvalBudget

	runSem      chan struct{}
	dispatchMu  sync.Mutex
	dispatches  int
	maxDispatch int
}

func newRequestDispatcher(ctx context.Context, o Options, m manifest, tree *runTree, journalWriter io.Writer, run *journal.Journal, runsRoot string, br bridge.Bridge, adm *admission, bud *budget, chain []*budget, tel *telemetry, approval *approvalBudget) *requestDispatcher {
	return &requestDispatcher{
		ctx: ctx, options: o, manifest: m, tree: tree, journal: journalWriter,
		run: run, runsRoot: runsRoot, bridge: br, admission: adm,
		budget: bud, chain: chain, telemetry: tel, approval: approval,
		runSem:      make(chan struct{}, maxConcurrentRuns),
		maxDispatch: dispatchesFor(m.Execution.Limits),
	}
}

func (d *requestDispatcher) effectOf(rq request) string {
	switch rq.Method {
	case "call":
		if rq.callAssessment != nil && rq.callAssessment.effect != "" {
			return rq.callAssessment.effect
		}
		if d.admission != nil {
			if bd := d.admission.byAlias[rq.Alias]; bd != nil {
				return bd.effective()
			}
		}
		return "write"
	case "exec":
		if _, e := resolve(&d.manifest, rq.Command, rq.Args); e != "" {
			return e
		}
		return "write"
	case "write":
		return "write"
	case "run":
		// The parent declares what its child may do; the child asks about its
		// own changes under the child's manifest.
		for _, u := range d.tree.uses {
			if u.Alias == rq.Alias {
				return u.Declared
			}
		}
		return "write"
	case "fetch":
		if mth := strings.ToUpper(rq.HTTPMethod); mth == "" || mth == "GET" || mth == "HEAD" {
			return "read"
		}
		return "write"
	}
	return "read"
}

// runUse runs a declared child in this process and shares the same deadlines,
// approvals, resume tree, and telemetry context with it.
func (d *requestDispatcher) runUse(rq request) reply {
	info := rq.info
	if info == nil {
		info = &childInfo{}
	}
	var node *useNode
	for _, u := range d.tree.uses {
		if u.Alias == rq.Alias {
			node = u
		}
	}
	if node == nil {
		return reply{Refused: fmt.Sprintf("%q is not one of the primitives this one uses", rq.Alias)}
	}
	if d.tree.depth+1 > maxUseDepth {
		return reply{Refused: fmt.Sprintf("primitives may use others only %d levels down", maxUseDepth)}
	}
	for _, digest := range d.tree.digests {
		if digest == node.Digest {
			return reply{Refused: node.Alias + " would run a primitive that is already running above it"}
		}
	}
	var args []string
	if rq.Input != nil {
		b, _ := json.Marshal(rq.Input)
		args = []string{string(b)}
	}
	if node.Manifest.Interface != nil {
		in := rq.Input
		if in == nil {
			in = map[string]any{}
		}
		if why := checkObject(node.Manifest.Interface.InputSchema, in); why != "" {
			return reply{Refused: fmt.Sprintf("the input to %s does not match its inputSchema: %s", node.Alias, why)}
		}
	}
	select {
	case d.runSem <- struct{}{}:
		defer func() { <-d.runSem }()
	case <-d.ctx.Done():
		return reply{Refused: "the run was stopped before " + node.Alias + " could start"}
	}
	timeCap := d.budget.left()
	for _, b := range d.tree.ancestors {
		if left := b.left(); left < timeCap {
			timeCap = left
		}
	}
	if timeCap <= 0 {
		return reply{Refused: "no time is left in this run for " + node.Alias}
	}
	childID, resume := rq.childRun, rq.resumeChild
	parentRun := ""
	if d.run != nil {
		parentRun = d.run.Header.RunID
	}
	if resume && d.runsRoot != "" {
		switch stored, _, finished, err := journal.FinishedReply(d.runsRoot, childID); {
		case err != nil && errors.Is(err, os.ErrNotExist):
			resume = false // it never got as far as a record
		case err == nil && finished && len(stored) > 0:
			// It finished and the answer was lost on the way back.
			var sr storedResult
			if json.Unmarshal(stored, &sr) == nil {
				info.unknown = sr.Unknown
				info.run = &ChildRun{Alias: node.Alias, Ref: node.Ref, Digest: node.Digest, RunID: childID, Exit: sr.Exit, Unknown: sr.Unknown}
				return childReply(node, sr)
			}
		case err == nil && finished:
			// An older record kept no output. Reading again is safe only for a
			// primitive that cannot change anything.
			if effectRank[node.Effect] > 0 {
				info.unknown = 1
				return reply{Unknown: true, Refused: node.Alias + " finished before an earlier run stopped, and its output was not kept; it can make changes, so whether they happened is unknown"}
			}
			resume = false
			childID = journal.NewRunID(time.Now().UTC())
		}
	}
	auth := childAuth{}
	if d.options.authFor != nil {
		auth = d.options.authFor(node.Digest)
	}
	co := Options{
		Package: node.Path, ExpectedDigest: node.Digest, Args: args,
		Approve: d.options.Approve, Choose: d.options.Choose, AskTool: d.options.AskTool,
		FetchOrigins: auth.Origins, FetchGrants: auth.Grants, RememberGrant: auth.Remember,
		Journal: d.options.Journal, InterpDir: d.options.InterpDir, CacheDir: d.options.CacheDir, PyLib: d.options.PyLib, Client: d.options.Client,
		Bridge: d.bridge, MCPURL: d.options.MCPURL, MCPHeaderFile: d.options.MCPHeaderFile, MCPServerName: d.options.MCPServerName,
		VSCodeSocket: d.options.VSCodeSocket, Proc: d.options.Proc,
		RunsDir: d.runsRoot, NoJournal: d.options.NoJournal, TelemetryPayloads: d.options.TelemetryPayloads,
		catalog: d.options.catalog, authFor: d.options.authFor, childHook: d.options.childHook,
	}
	if resume {
		co.Resume = childID
	}
	co.tree = d.tree.below(node, d.chain, parentRun, rq.ID, childID, timeCap)
	if d.options.childHook != nil {
		d.options.childHook(node.Alias, &co)
	}
	cctx := d.ctx
	if d.telemetry != nil {
		cctx = trace.ContextWithSpan(d.ctx, d.telemetry.root)
	}
	cres, err := Run(cctx, co)
	if err != nil {
		if errors.Is(err, errInterrupted) {
			info.err = err
			return reply{}
		}
		return reply{Refused: fmt.Sprintf("%s could not run: %v", node.Alias, err)}
	}
	if d.options.stopAfterChild {
		info.err = errInterrupted
		return reply{}
	}
	info.unknown = cres.Unknown
	info.run = &ChildRun{
		Alias: node.Alias, Ref: node.Ref, Digest: node.Digest, RunID: cres.RunID, Exit: cres.Exit,
		Calls: cres.Calls, FailedCalls: cres.FailedCalls, Refused: cres.Refused, Unknown: cres.Unknown,
		Elapsed: cres.Elapsed, Children: cres.Children,
	}
	return childReply(node, storedResult{Exit: cres.Exit, Stdout: cres.Stdout, Stderr: cres.Stderr, Unknown: cres.Unknown})
}

func (d *requestDispatcher) dispatch(rq request) reply {
	if rq.Method != "tools" && rq.Method != "canwrite" {
		d.dispatchMu.Lock()
		d.dispatches++
		over := d.dispatches > d.maxDispatch
		d.dispatchMu.Unlock()
		overTree := d.tree.countDispatch()
		if over {
			logf("  REFUSED  %s  (over limits.max_dispatches = %d)", rq.Method, d.maxDispatch)
			return reply{Refused: fmt.Sprintf("the primitive has made its %d requests (limits.max_dispatches)", d.maxDispatch)}
		}
		if overTree {
			logf("  REFUSED  %s  (the run is over its limits.max_dispatches = %d)", rq.Method, d.tree.max)
			return reply{Refused: fmt.Sprintf("the primitives of this run have made their %d requests (limits.max_dispatches of the primitive that was asked for)", d.tree.max)}
		}
	}
	return d.dispatchAllowed(rq)
}

func (d *requestDispatcher) dispatchAllowed(rq request) reply {
	switch rq.Method {
	case "tools":
		rp := reply{Tools: []string{}}
		if d.admission != nil {
			rp.Tools = d.admission.aliases()
		}
		for _, u := range d.tree.uses {
			rp.Uses = append(rp.Uses, u.Alias)
		}
		return rp
	case "run":
		return d.runUse(rq)
	case "call":
		kind, example := "call "+rq.Alias, "call "+rq.Alias
		if d.admission != nil {
			if bd := d.admission.byAlias[rq.Alias]; bd != nil {
				a, _ := json.Marshal(rq.Arguments)
				kind = fmt.Sprintf("call the tool %s / %s", bd.Server, bd.Tool)
				example = fmt.Sprintf("%s with %s", kind, a)
				if bd.Asked {
					example += " (your client is set to ask before using this tool)"
				}
			}
		}
		return d.approval.run(func(approve bool) reply { return callTool(d.admission, d.bridge, rq, approve, d.journal) }, kind, example, d.effectOf(rq), true)
	case "exec":
		kind := "run " + rq.Command
		if decl, _ := resolve(&d.manifest, rq.Command, rq.Args); decl != nil {
			kind = strings.TrimSpace("run " + decl.Command + " " + strings.Join(decl.Args, " "))
		}
		return d.approval.run(func(approve bool) reply {
			return runCommand(d.ctx, d.options.Proc, &d.manifest, rq, approve, d.journal)
		}, kind,
			"run "+strings.TrimSpace(rq.Command+" "+strings.Join(rq.Args, " ")), d.effectOf(rq), true)
	case "read", "write", "canwrite":
		return d.approval.run(func(approve bool) reply { return fileOp(d.options.Proc, &d.manifest, rq, approve, d.journal) },
			"write files under "+declaredRoot(d.options.Proc, &d.manifest, rq.Path), "write the file "+rq.Path, "write", rq.Method == "write")
	case "fetch":
		origin := rq.URL
		if u, err := url.Parse(rq.URL); err == nil {
			origin = u.Scheme + "://" + u.Host
		}
		mth := strings.ToUpper(rq.HTTPMethod)
		return d.approval.run(func(approve bool) reply { return fetchOp(&d.manifest, rq, approve, d.journal) },
			"send "+mth+" requests to "+origin, "send "+mth+" to "+rq.URL, d.effectOf(rq), true)
	}
	return reply{Refused: "unknown request"}
}
