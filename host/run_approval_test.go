package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestApprovalBudgetSpendsByKindAndReportsCompletedCount(t *testing.T) {
	var journal bytes.Buffer
	var asks []Ask
	o := Options{Approve: func(a Ask) Grant {
		asks = append(asks, a)
		if len(asks) == 1 {
			return Grant{OK: true, Limit: 1}
		}
		return Grant{}
	}}
	budget := newApprovalBudget(o, nil, newRootTree(10, "example", "digest"), &journal)
	var approvedCalls int
	op := func(approved bool) reply {
		if !approved {
			return reply{Gated: true}
		}
		approvedCalls++
		return reply{Result: "done"}
	}

	if got := budget.run(op, "write files under out", "out/a.txt", "write", true); got.Gated || got.Result != "done" {
		t.Fatalf("first approved operation = %+v", got)
	}
	if got := budget.run(op, "write files under out", "out/b.txt", "write", true); !got.Gated {
		t.Fatalf("operation beyond the allowance should remain gated: %+v", got)
	}
	if approvedCalls != 1 {
		t.Fatalf("approved operation ran %d times, want exactly once", approvedCalls)
	}
	if len(asks) != 2 || asks[0].Done != 0 || asks[1].Done != 1 || asks[0].Primitive != "example" {
		t.Fatalf("approval requests = %+v", asks)
	}
	if got := journal.String(); strings.Count(got, `"outcome":"approved"`) != 1 || strings.Count(got, `"outcome":"declined"`) != 1 {
		t.Fatalf("approval journal did not record both decisions: %s", got)
	}
}
