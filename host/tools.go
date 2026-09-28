package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"gitlab.com/telara-labs/tap-runtime/bind"
	"gitlab.com/telara-labs/tap-runtime/bridge"
)

// toolDecl is one entry of the manifest's tools block.
type toolDecl struct {
	Alias      string `yaml:"alias"`
	Capability string `yaml:"capability"`
	Effect     string `yaml:"effect"`
	Optional   bool   `yaml:"optional"`
	Pin        *struct {
		Server string `yaml:"server"`
		Tool   string `yaml:"tool"`
	} `yaml:"pin"`
}

// binding is what an alias resolved to, and what the receipt says about it.
type binding struct {
	Alias           string    `json:"alias"`
	Capability      string    `json:"capability"`
	Declared        string    `json:"declared"`
	Server          string    `json:"server"`
	Tool            string    `json:"tool"`
	Annotated       string    `json:"annotated"`
	Score           float64   `json:"score"`
	RunnerUp        string    `json:"runner_up,omitempty"`
	RunnerUpScore   float64   `json:"runner_up_score,omitempty"`
	Gated           bool      `json:"treated_as_write"`
	Pinned          bool      `json:"pinned"`
	ContractChecked bool      `json:"contract_checked"`
	tool            bind.Tool `json:"-"`
}

// admission is the outcome of resolving a manifest against a client.
type admission struct {
	Client   string    `json:"client"`
	Version  string    `json:"client_version"`
	Tested   bool      `json:"client_version_tested"`
	Bindings []binding `json:"bindings"`
	Skipped  []string  `json:"optional_not_bound,omitempty"`
	byAlias  map[string]*binding
}

// detectClient names the client that started this process, from what that
// client puts in the environment of its children. Nothing is set here.
func detectClient() string {
	switch {
	case os.Getenv("CLAUDECODE") != "":
		return "claude"
	case os.Getenv("CODEX_SANDBOX") != "":
		return "codex"
	}
	return ""
}

func openBridge(client string) (bridge.Bridge, error) {
	switch client {
	case "claude":
		return bridge.NewClaude()
	case "codex":
		return bridge.NewCodex()
	case "":
		return nil, fmt.Errorf("this primitive declares tools and no client was detected; pass --client claude or --client codex")
	}
	// Ruling 13: a client that lists its tools and cannot dispatch them.
	return nil, fmt.Errorf("client %q cannot lend its connections: it gives no way to call a tool on the caller's behalf", client)
}

func validEffect(e string) bool {
	return e == string(bind.Read) || e == string(bind.Write) || e == string(bind.Destructive)
}

// admit resolves every declared tool before the guest is started. A required
// tool that does not bind refuses the whole run; an optional one is left out
// and the guest is told.
func admit(decls []toolDecl, b bridge.Bridge) (*admission, error) {
	name, version := b.Client()
	a := &admission{Client: name, Version: version, Tested: bridge.Tested(name, version), byAlias: map[string]*binding{}}
	seen := map[string]bool{}
	for _, d := range decls {
		switch {
		case d.Alias == "":
			return nil, fmt.Errorf("a tool has no alias")
		case seen[d.Alias]:
			return nil, fmt.Errorf("alias %q is declared twice", d.Alias)
		case !validEffect(d.Effect):
			return nil, fmt.Errorf("tool %q declares effect %q; it must be read, write or destructive", d.Alias, d.Effect)
		}
		seen[d.Alias] = true
	}
	// Package checks come before anything is asked of the client, so a bad
	// package is refused the same way whatever the client holds.
	inv, err := b.Inventory()
	if err != nil {
		return nil, fmt.Errorf("reading the client's tools: %w", err)
	}
	for _, d := range decls {
		bd := binding{Alias: d.Alias, Capability: d.Capability, Declared: d.Effect}
		refusal := ""
		if d.Pin != nil {
			bd.Pinned = true
			found := false
			for _, t := range inv {
				if t.Server == d.Pin.Server && t.Name == d.Pin.Tool {
					bd.tool, found = t, true
				}
			}
			switch {
			case !found:
				refusal = fmt.Sprintf("the pinned tool %s / %s is not on this client", d.Pin.Server, d.Pin.Tool)
			case bd.tool.Annotated != bind.Unknown && rankOf(bd.tool.Annotated) > rankOf(bind.Effect(d.Effect)):
				refusal = fmt.Sprintf("the pinned tool is annotated %s and the primitive declares %s", bd.tool.Annotated, d.Effect)
			}
			bd.Score = 1
			bd.Gated = bd.tool.Annotated == bind.Unknown
		} else {
			c := bind.Resolve(d.Capability, bind.Effect(d.Effect), inv)
			if c.RunnerUp != nil {
				bd.RunnerUp, bd.RunnerUpScore = c.RunnerUp.Server+" / "+c.RunnerUp.Name, c.RunnerUpScore
			}
			if c.Bound == nil {
				refusal = c.Refused
			} else {
				bd.tool, bd.Score, bd.Gated = *c.Bound, c.Score, c.Gated
			}
		}
		if refusal == "" {
			denied, err := b.Denied(bd.tool)
			if err != nil {
				return nil, fmt.Errorf("reading the user's permission rules: %w", err)
			}
			if denied {
				refusal = fmt.Sprintf("the user has denied %s / %s in their client", bd.tool.Server, bd.tool.Name)
			}
		}
		if refusal != "" {
			if d.Optional {
				a.Skipped = append(a.Skipped, d.Alias)
				logf("optional   %-10s not bound: %s", d.Alias, refusal)
				continue
			}
			return nil, fmt.Errorf("tool %q (%s): %s", d.Alias, d.Capability, refusal)
		}
		bd.Server, bd.Tool, bd.Annotated = bd.tool.Server, bd.tool.Name, string(bd.tool.Annotated)
		a.Bindings = append(a.Bindings, bd)
	}
	for i := range a.Bindings {
		a.byAlias[a.Bindings[i].Alias] = &a.Bindings[i]
	}
	return a, nil
}

func rankOf(e bind.Effect) int {
	switch e {
	case bind.Write:
		return 1
	case bind.Destructive:
		return 2
	}
	return 0
}

// effective is the effect the gate uses: what was declared, raised to write
// when the tool's own server said nothing about it (ruling 20).
func (b *binding) effective() string {
	if b.Gated && b.Declared == string(bind.Read) {
		return string(bind.Write)
	}
	return b.Declared
}

func (a *admission) aliases() []string {
	out := make([]string, 0, len(a.byAlias))
	for k := range a.byAlias {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// callTool dispatches one guest call. The guest names an alias; it never
// learns the server, the tool name or how the client authenticated.
func callTool(a *admission, b bridge.Bridge, rq request, approve bool, journal io.Writer) reply {
	entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "alias": rq.Alias}
	record := func(outcome string, extra map[string]any) {
		entry["outcome"] = outcome
		for k, v := range extra {
			entry[k] = v
		}
		j, _ := json.Marshal(entry)
		journal.Write(append(j, '\n'))
	}
	if a == nil {
		record("refused_undeclared", nil)
		return reply{Refused: "this primitive declares no tools"}
	}
	bd, ok := a.byAlias[rq.Alias]
	if !ok {
		logf("  REFUSED  call %s  (not a bound alias)", rq.Alias)
		record("refused_undeclared", nil)
		return reply{Refused: "alias not declared, or declared optional and not bound"}
	}
	entry["server"], entry["tool"], entry["effect"] = bd.Server, bd.Tool, bd.effective()
	if bd.effective() != string(bind.Read) && !approve {
		logf("  GATED    call %s -> %s / %s  (%s, no approval)", rq.Alias, bd.Server, bd.Tool, bd.effective())
		record("gated", nil)
		return reply{Refused: bd.effective() + " tool needs approval", Gated: true}
	}
	t0 := time.Now()
	res, err := b.Call(bd.tool, rq.Arguments)
	if err != nil {
		logf("  FAILED   call %s -> %s / %s: %v", rq.Alias, bd.Server, bd.Tool, err)
		record("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	logf("  call     %s -> %s / %s  [%s] %dB in, %s", rq.Alias, bd.Server, bd.Tool, bd.effective(), len(res), time.Since(t0).Round(time.Millisecond))
	record("ran", map[string]any{"result_bytes": len(res), "ms": time.Since(t0).Milliseconds()})
	return reply{Result: res}
}
