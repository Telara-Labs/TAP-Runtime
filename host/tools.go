package main

import (
	"encoding/json"
	"fmt"
	agents "gitlab.com/telara-labs/tap-runtime/discover/client"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/satisfy"

	"gitlab.com/telara-labs/tap-runtime/bind"
	"gitlab.com/telara-labs/tap-runtime/bridge"
	mf "gitlab.com/telara-labs/tap-runtime/contract/manifest"
)

// binding is what an alias resolved to, and what the receipt says about it.
type binding struct {
	Alias         string  `json:"alias"`
	Capability    string  `json:"capability"`
	Declared      string  `json:"declared"`
	Server        string  `json:"server"`
	Tool          string  `json:"tool"`
	Annotated     string  `json:"annotated"`
	Score         float64 `json:"score"`
	RunnerUp      string  `json:"runner_up,omitempty"`
	RunnerUpScore float64 `json:"runner_up_score,omitempty"`
	Gated         bool    `json:"treated_as_write"`
	// Asked is set when the person's client is set to ask before using the
	// tool. The runner asks too, as it does for a write.
	Asked  bool `json:"client_asks,omitempty"`
	Pinned bool `json:"pinned"`
	// ContractChecked says the tool's input schema was checked against the
	// capability's contract at admission. ResultChecked says each answer is
	// checked against the contract as it arrives. Schema is the digest of
	// the input schema that was checked.
	ContractChecked bool   `json:"contract_checked"`
	ResultChecked   bool   `json:"result_checked"`
	Schema          string `json:"schema,omitempty"`
	// Candidates are tools whose names ranked ahead of the one bound and
	// whose schemas did not satisfy the contract, each with why.
	Candidates []string       `json:"passed_over,omitempty"`
	tool       bind.Tool      `json:"-"`
	result     map[string]any `json:"-"`
	args       map[string]any `json:"-"` // the contract's arguments, held against every call
}

// admission is the outcome of resolving a manifest against a client.
type admission struct {
	Client   string    `json:"client"`
	Version  string    `json:"client_version"`
	Tested   bool      `json:"client_version_tested"`
	Bindings []binding `json:"bindings"`
	Skipped  []string  `json:"optional_not_bound,omitempty"`
	byAlias  map[string]*binding
	inv      []bind.Tool // the client's tools, for operations dispatched through one
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

// bridgeName turns an agent's registry name or alias (claude-code, claude,
// gemini-cli) into the name the runner's bridges use (claude, codex,
// gemini). Unknown names pass through, and openBridge refuses them.
func bridgeName(name string) string {
	c, ok := agents.Lookup(name)
	if !ok {
		return name
	}
	switch c.ID {
	case "claude-code":
		return "claude"
	case "gemini-cli":
		return "gemini"
	}
	return c.ID
}

// lendsConnections reports whether the runner can borrow this client's
// connections: through a call-back API (openBridge) or a relay hook. The
// discover registry's Bridge flag must agree (TestRegistryBridgeMatchesRunner).
func lendsConnections(client string) bool {
	switch client {
	case "claude", "codex", "goose":
		return true
	}
	return relayClient(client)
}

func openBridge(client string) (bridge.Bridge, error) {
	switch client {
	case "claude":
		return bridge.NewClaude()
	case "codex":
		return bridge.NewCodex()
	case "goose":
		return bridge.NewGoose()
	case "":
		return nil, fmt.Errorf("this primitive declares tools and no client was detected; pass --client claude or --client codex")
	}
	// Ruling 13: a client that lists its tools and cannot dispatch them.
	return nil, fmt.Errorf("client %q cannot lend its connections: it gives no way to call a tool on the caller's behalf", client)
}

// openMCP opens the MCP bridge. The header comes from a file, never a flag:
// a process's arguments are readable by anyone who can list processes.
func openMCP(url, headerFile string) (bridge.Bridge, error) {
	h := http.Header{}
	if headerFile != "" {
		raw, err := os.ReadFile(headerFile)
		if err != nil {
			return nil, fmt.Errorf("--mcp-header-file: %w", err)
		}
		h, err = parseHeaderLines(string(raw))
		if err != nil {
			return nil, fmt.Errorf("--mcp-header-file: %w", err)
		}
	}
	b, err := bridge.NewMCP(url, h)
	if err != nil {
		return nil, fmt.Errorf("MCP server %s: %w", url, err)
	}
	return b, nil
}

// parseHeaderLines reads "Name: value" lines. Blank lines and lines starting
// with # are skipped; any other line without a colon is refused rather than
// silently dropped, since a dropped Authorization reads as an auth failure.
func parseHeaderLines(s string) (http.Header, error) {
	h := http.Header{}
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("line %d is not a header line", i+1)
		}
		h.Add(name, strings.TrimSpace(value))
	}
	return h, nil
}

func validEffect(e string) bool {
	switch e {
	case "read", "write", "destructive", "financial", "identity-admin":
		return true
	}
	return false
}

// admit resolves every declared tool before the guest is started. A required
// tool that does not bind refuses the whole run; an optional one is left out
// and the guest is told.
func admit(decls []toolDecl, b bridge.Bridge, contracts ...mf.Capability) (*admission, error) {
	return admitWith(nil, nil, decls, b, contracts...)
}

// admitWith is admit with a way to settle two servers that fit equally well:
// the choice kept on this machine, then a person to ask (TENG-3100).
func admitWith(store bindingStore, choose Chooser, decls []toolDecl, b bridge.Bridge, contracts ...mf.Capability) (*admission, error) {
	byLabel := map[string]*mf.Capability{}
	for i := range contracts {
		byLabel[contracts[i].Label] = &contracts[i]
	}
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
			return nil, fmt.Errorf("tool %q declares effect %q; it must be read, write, destructive, financial or identity-admin", d.Alias, d.Effect)
		}
		seen[d.Alias] = true
	}
	// Package checks come before anything is asked of the client, so a bad
	// package is refused the same way whatever the client holds.
	inv, err := b.Inventory()
	if err != nil {
		return nil, fmt.Errorf("reading the client's tools: %w", err)
	}
	a.inv = inv
	for _, d := range decls {
		bd := binding{Alias: d.Alias, Capability: d.Capability, Declared: d.Effect}
		refusal := ""
		contract := byLabel[d.Capability]
		if contract != nil {
			// Whatever the client, an answer can be held to the contract.
			bd.result, bd.ResultChecked = contract.Result, len(contract.Result) > 0
			bd.args = contract.Args
		}
		bySchema := contract != nil && b.HasSchemas()
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
			if refusal == "" && bySchema {
				if why := satisfy.Arguments(contract.Args, bd.tool.Schema); len(why) > 0 {
					refusal = "the pinned tool does not satisfy the contract: " + strings.Join(why, "; ")
				} else {
					bd.ContractChecked, bd.Schema = true, satisfy.Digest(bd.tool.Schema)
				}
			}
		} else if bySchema {
			// Ruling 22: the name chooses, and the schema checks. Tools are
			// taken in the order their names rank, and the first whose live
			// input schema satisfies the contract binds. A tool whose name
			// is closest and whose schema does not fit is passed over, and
			// the receipt says so.
			c, ranked := bind.Candidates(mf.CapabilityName(d.Capability), bind.Effect(d.Effect), inv)
			if c.Bound == nil {
				refusal = c.Refused
			}
			var passed []string
			found := false
			var fitting []bind.Candidate
			for _, cand := range ranked {
				why := satisfy.Arguments(contract.Args, cand.Tool.Schema)
				if len(why) > 0 {
					passed = append(passed, fmt.Sprintf("%s / %s (%s)", cand.Tool.Server, cand.Tool.Name, why[0]))
					continue
				}
				fitting = append(fitting, cand)
			}
			if len(fitting) > 0 {
				cand := fitting[0]
				if servers := tiedServers(fitting); len(servers) > 1 {
					server, why := settle(Pick{Alias: d.Alias, Capability: mf.CapabilityName(d.Capability), Client: name, Servers: servers}, store, choose)
					if why != "" {
						refusal = why
					} else {
						for _, c := range fitting {
							if c.Tool.Server == server {
								cand = c
								break
							}
						}
					}
				}
				if refusal == "" {
					bd.tool, bd.Score, bd.Gated = cand.Tool, cand.Score, cand.Tool.Annotated == bind.Unknown
					bd.ContractChecked, bd.Schema = true, satisfy.Digest(cand.Tool.Schema)
					found = true
				}
			}
			bd.Candidates = passed
			if !found && refusal == "" {
				refusal = "no tool with a fitting name satisfies the contract: " + strings.Join(passed, "; ")
			}
			if c.RunnerUp != nil {
				bd.RunnerUp, bd.RunnerUpScore = c.RunnerUp.Server+" / "+c.RunnerUp.Name, c.RunnerUpScore
			}
		} else {
			c, ranked := bind.Candidates(mf.CapabilityName(d.Capability), bind.Effect(d.Effect), inv)
			if c.RunnerUp != nil {
				bd.RunnerUp, bd.RunnerUpScore = c.RunnerUp.Server+" / "+c.RunnerUp.Name, c.RunnerUpScore
			}
			if c.Bound == nil {
				refusal = c.Refused
			} else {
				bd.tool, bd.Score, bd.Gated = *c.Bound, c.Score, c.Gated
				if servers := tiedServers(ranked); len(servers) > 1 {
					server, why := settle(Pick{Alias: d.Alias, Capability: mf.CapabilityName(d.Capability), Client: name, Servers: servers}, store, choose)
					if why != "" {
						refusal = why
					} else {
						for _, cand := range ranked {
							if cand.Tool.Server == server && cand.Score == ranked[0].Score {
								bd.tool, bd.Score, bd.Gated = cand.Tool, cand.Score, cand.Tool.Annotated == bind.Unknown
								break
							}
						}
					}
				}
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
		if refusal == "" {
			// A tool the person's client is set to ask about is asked about
			// here too, whatever the primitive says it does (TENG-3101).
			if ak, ok := b.(bridge.Asker); ok {
				asks, err := ak.Asks(bd.tool)
				if err != nil {
					return nil, fmt.Errorf("reading the user's permission rules: %w", err)
				}
				bd.Asked = asks
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

func rankOf(e bind.Effect) int { return bind.Rank(e) }

// effective is the effect the gate uses. The declaration is an upper bound,
// held at binding (a tool annotated as doing more never binds). Within it
// the tool's own server decides: a tool its server annotates read-only runs
// as a read, whatever was declared, since a primitive generated from history
// declares write when the history could not show the effect. A tool its
// server says nothing about is treated as a write (ruling 20), and so is one
// the person's client is set to ask about (TENG-3101).
func (b *binding) effective() string {
	if b.Gated || b.Asked {
		if b.Declared == string(bind.Read) {
			return string(bind.Write)
		}
		return b.Declared
	}
	if b.tool.Annotated == bind.Read {
		return string(bind.Read)
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
	effect, refused, nested := callEffect(bd, a.inv, rq.Arguments)
	entry["server"], entry["tool"], entry["effect"] = bd.Server, bd.Tool, effect
	if nested != "" {
		entry["dispatches"] = nested
	}
	if refused != "" {
		logf("  REFUSED  call %s -> %s / %s  (%s)", rq.Alias, bd.Server, bd.Tool, refused)
		record("refused_effect", nil)
		return reply{Refused: refused}
	}
	if why := satisfy.CallArguments(bd.args, rq.Arguments); why != "" {
		logf("  REFUSED  call %s -> %s / %s  (arguments outside the contract: %s)", rq.Alias, bd.Server, bd.Tool, why)
		record("refused_arguments", map[string]any{"error": why})
		return reply{Refused: "the arguments are outside what the capability declares: " + why}
	}
	if effect != string(bind.Read) && !approve {
		logf("  GATED    call %s -> %s / %s  (%s, no approval)", rq.Alias, bd.Server, bd.Tool, effect)
		record("gated", nil)
		return reply{Refused: effect + " tool needs approval", Gated: true}
	}
	t0 := time.Now()
	res, err := b.Call(bd.tool, rq.Arguments)
	if err != nil {
		logf("  FAILED   call %s -> %s / %s: %v", rq.Alias, bd.Server, bd.Tool, err)
		record("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	if bd.ResultChecked {
		if why := satisfy.Result(bd.result, res); why != "" {
			// The answer is not what the contract promises. For a read that
			// is a failed call. For a change, the change has been made: the
			// program and the record are told both things (34 section 11.3).
			landed := effect != string(bind.Read)
			logf("  VIOLATES call %s -> %s / %s: %s (landed=%v)", rq.Alias, bd.Server, bd.Tool, why, landed)
			record("output_schema_violation", map[string]any{"error": why, "landed": landed, "result_bytes": len(res)})
			return reply{Exit: 1, Stderr: "output_schema_violation: " + why, Violation: true, Landed: landed}
		}
	}
	logf("  call     %s -> %s / %s  [%s] %dB in, %s", rq.Alias, bd.Server, bd.Tool, effect, len(res), time.Since(t0).Round(time.Millisecond))
	record("ran", map[string]any{"result_bytes": len(res), "ms": time.Since(t0).Milliseconds()})
	return reply{Result: res}
}
