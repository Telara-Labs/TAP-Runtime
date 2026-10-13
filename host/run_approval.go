package main

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

type allowance struct {
	left     int
	done     int
	declined bool
}

// approvalBudget owns the per-run consent ledger. It serializes questions and
// spends only the allowance for the matching kind of change.
type approvalBudget struct {
	options     Options
	chain       []*budget
	tree        *runTree
	journal     io.Writer
	mu          sync.Mutex
	allowed     map[string]*allowance
	preapproved map[string]bool
}

func newApprovalBudget(o Options, chain []*budget, tree *runTree, journal io.Writer) *approvalBudget {
	g := &approvalBudget{
		options: o, chain: chain, tree: tree, journal: journal,
		allowed: map[string]*allowance{}, preapproved: map[string]bool{},
	}
	for _, origin := range o.FetchOrigins {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			g.preapproved["send "+method+" requests to "+origin] = true
		}
	}
	for _, kind := range o.FetchGrants {
		if readFetchKind(kind) {
			g.preapproved[kind] = true
		}
	}
	return g
}

func (g *approvalBudget) audit(outcome, kind, effect string, extra map[string]any) {
	e := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "outcome": outcome, "kind": kind, "effect": effect}
	for k, v := range extra {
		e[k] = v
	}
	b, _ := json.Marshal(e)
	_, _ = g.journal.Write(append(b, '\n'))
}

// run retries an operation only when the operation reports that a person's
// agreement is the sole missing condition. The allowance is spent after the
// first successful approval and returned through an unapproved gate reply.
func (g *approvalBudget) run(op func(approve bool) reply, kind, example, effect string, spend bool) reply {
	rp := op(false)
	if !rp.Gated {
		return rp
	}
	g.mu.Lock()
	a := g.allowed[kind]
	if a == nil {
		a = &allowance{}
		g.allowed[kind] = a
	}
	if !a.declined && a.left == 0 {
		var grant Grant
		if g.preapproved[kind] {
			grant = Grant{OK: true, Limit: Unlimited}
		} else if g.options.Approve != nil {
			// A person deciding does not consume the primitive's running time.
			// The tree has one asker, so questions take turns across its runs.
			for _, b := range g.chain {
				b.pause()
			}
			g.tree.askMu.Lock()
			grant = g.options.Approve(Ask{Primitive: g.tree.asker(), Effect: effect, Kind: forPrompt(kind), Example: forPrompt(example), Done: a.done})
			g.tree.askMu.Unlock()
			for _, b := range g.chain {
				b.resume()
			}
		}
		// A read fetch approved for this digest can be reused next run; writes
		// are still asked about at the start of each run.
		if grant.OK && !g.preapproved[kind] && readFetchKind(kind) && g.options.RememberGrant != nil {
			g.options.RememberGrant(kind)
		}
		switch {
		case !grant.OK:
			a.declined = true
			g.audit("declined", kind, effect, map[string]any{"after": a.done})
		case grant.Limit == Unlimited:
			a.left = Unlimited
			g.audit("approved", kind, effect, map[string]any{"limit": "none", "after": a.done})
		default:
			if grant.Limit < 1 {
				grant.Limit = 1
			}
			a.left = grant.Limit
			g.audit("approved", kind, effect, map[string]any{"limit": grant.Limit, "after": a.done})
		}
	}
	ok := !a.declined && a.left != 0
	if ok && spend {
		if a.left > 0 {
			a.left--
		}
		a.done++
	}
	g.mu.Unlock()
	if !ok {
		return rp
	}
	return op(true)
}
