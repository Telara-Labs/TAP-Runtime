package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/bind"
	"gitlab.com/telara-labs/tap-runtime/bridge"
)

// plainWord is a value that can name an operation: a short word, never free
// text, an identifier with spaces, or a structure.
var plainWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,39}$`)

type telaraDispatch struct {
	Integration string
	Action      string
	WrapArgs    bool
}

var telaraIntegrations = []string{
	"asana", "bitbucket", "confluence", "gitlab", "google_workspace",
	"jira", "linear", "microsoft_entra", "notion", "openai_admin", "slack", "teams",
}

func findTool(inv []bind.Tool, server, name string) (bind.Tool, bool) {
	for _, t := range inv {
		if t.Server == server && t.Name == name {
			return t, true
		}
	}
	return bind.Tool{}, false
}

func isTelaraDispatcher(t bind.Tool) bool {
	return t.Server == "telara" && t.Name == "telara_execute_action"
}

// telaraActionFromPin translates an action-specific Telara tool name into the
// generic execute_action route used by Codex. The actual action and its effect
// are verified through telara_tool_search before a read is dispatched.
func telaraActionFromPin(server, tool string) (integration, action string, ok bool) {
	if server != "telara" || !strings.HasPrefix(tool, "telara_") {
		return "", "", false
	}
	rest := strings.TrimPrefix(tool, "telara_")
	for _, candidate := range telaraIntegrations {
		prefix := candidate + "_"
		if strings.HasPrefix(rest, prefix) && len(rest) > len(prefix) {
			return candidate, strings.TrimPrefix(rest, prefix), true
		}
	}
	return "", "", false
}

func wrapTelaraActionArgs(integration, action string, args map[string]any) (map[string]any, error) {
	params := map[string]any{}
	call := map[string]any{"integration": integration, "action": action, "params": params}
	for key, value := range args {
		switch key {
		case "approval_reason":
			call[key] = value
		case "params":
			switch p := value.(type) {
			case map[string]any:
				for k, v := range p {
					params[k] = v
				}
			case string:
				var decoded map[string]any
				if err := json.Unmarshal([]byte(p), &decoded); err != nil {
					return nil, fmt.Errorf("invalid params JSON: %w", err)
				}
				for k, v := range decoded {
					params[k] = v
				}
			default:
				return nil, fmt.Errorf("params must be an object or JSON object string")
			}
		default:
			name := strings.TrimPrefix(key, "params_")
			if name == "" {
				return nil, fmt.Errorf("parameter name is empty")
			}
			params[name] = value
		}
	}
	return call, nil
}

// nestedTool finds the operation a call dispatches through another tool. A
// call dispatches when its arguments are plain-word values beside exactly one
// parameter object (a gateway's integration and action with their params).
// The operation is the client's own tool whose name holds every word of
// those values with the fewest other words (get_issue, not
// get_issue_comments); a tie finds nothing. Nothing here names a gateway or
// a provider.
func nestedTool(args map[string]any, via bind.Tool, inv []bind.Tool) (bind.Tool, bool) {
	objects := 0
	var words []string
	for _, v := range args {
		switch x := v.(type) {
		case map[string]any:
			objects++
		case string:
			t := strings.TrimSpace(x)
			if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
				objects++
			} else if plainWord.MatchString(x) {
				words = append(words, bind.Tokens(x)...)
			}
		}
	}
	if objects != 1 || len(words) == 0 {
		return bind.Tool{}, false
	}
	unique := map[string]bool{}
	for _, w := range words {
		unique[w] = true
	}
	var found []bind.Tool
	fewest := -1
	for _, t := range inv {
		if t.Server == via.Server && t.Name == via.Name {
			continue
		}
		have := map[string]bool{}
		for _, w := range bind.Tokens(t.Name) {
			have[w] = true
		}
		for _, w := range bind.Tokens(t.Server) {
			have[w] = true
		}
		all := true
		for w := range unique {
			all = all && have[w]
		}
		if !all {
			continue
		}
		extra := len(have) - len(unique)
		switch {
		case fewest < 0 || extra < fewest:
			found, fewest = []bind.Tool{t}, extra
		case extra == fewest:
			found = append(found, t)
		}
	}
	if len(found) != 1 {
		return bind.Tool{}, false
	}
	return found[0], true
}

// callEffect is the effect one call is gated by. A dispatched operation the
// client also lists as its own tool contributes its annotation to the gate.
// A read declaration is refused when the operation is effectful; an
// effectful declaration is promoted to the stronger observed effect.
func callEffect(bd *binding, inv []bind.Tool, args map[string]any, br bridge.Bridge) (effect, refused, nested string) {
	if isTelaraDispatcher(bd.tool) && !bd.Asked {
		integration, action := "", ""
		if bd.dispatch != nil && bd.dispatch.Integration != "" {
			integration, action = bd.dispatch.Integration, bd.dispatch.Action
		} else {
			integration, _ = args["integration"].(string)
			action, _ = args["action"].(string)
		}
		if integration == "" || action == "" {
			if bd.Declared == string(bind.Read) {
				return "", "a read through telara_execute_action needs a fixed integration and action", ""
			}
			return bd.effective(), "", ""
		}
		// If this client exposes the dispatched operation directly, its own
		// annotation remains authoritative (the existing nestedTool path).
		if inner, ok := nestedTool(args, bd.tool, inv); ok && inner.Annotated != bind.Unknown {
			nested = inner.Server + " / " + inner.Name
			if bd.Declared == string(bind.Read) && inner.Annotated != bind.Read {
				return "", "the dispatched operation " + nested + " is annotated " + string(inner.Annotated) + " and the primitive declares read", nested
			}
			resolved := inner.Annotated
			if resolved != bind.Read && bind.Rank(bind.Effect(bd.Declared)) > bind.Rank(resolved) {
				resolved = bind.Effect(bd.Declared)
			}
			return string(resolved), "", nested
		}
		resolved, err := telaraActionEffect(br, bd.tool.Server, inv, integration, action)
		if err != nil {
			if bd.Declared != string(bind.Read) {
				// No verified target: retain the broad dispatcher gate.
				return bd.effective(), "", integration + "/" + action
			}
			return "", err.Error(), integration + "/" + action
		}
		if bd.Declared == string(bind.Read) && resolved != string(bind.Read) {
			return "", fmt.Sprintf("Telara declares %s/%s as %s; the primitive declares read", integration, action, resolved), integration + "/" + action
		}
		if resolved != string(bind.Read) && bind.Rank(bind.Effect(bd.Declared)) > bind.Rank(bind.Effect(resolved)) {
			resolved = bd.Declared
		}
		return resolved, "", integration + "/" + action
	}
	effect = bd.effective()
	inner, ok := nestedTool(args, bd.tool, inv)
	if !ok || inner.Annotated == bind.Unknown {
		return effect, "", ""
	}
	nested = inner.Server + " / " + inner.Name
	if bd.Declared == string(bind.Read) && inner.Annotated != bind.Read {
		return effect, "the dispatched operation " + nested + " is annotated " + string(inner.Annotated) + " and the primitive declares " + bd.Declared, nested
	}
	if inner.Annotated == bind.Read && effect != string(bind.Read) && !bd.Asked {
		return string(bind.Read), "", nested
	}
	if bind.Rank(inner.Annotated) > bind.Rank(bind.Effect(effect)) {
		effect = string(inner.Annotated)
	}
	return effect, "", nested
}

// telaraActionEffect checks the generic dispatcher's target against the
// action catalog. A generic execute_action annotation is necessarily broad;
// it cannot authorize a read by itself. Reads through it run only when the
// catalog identifies the exact operation as read-only.
func telaraActionEffect(br bridge.Bridge, server string, inv []bind.Tool, integration, action string) (string, error) {
	search, ok := findTool(inv, server, "telara_tool_search")
	if !ok || search.Annotated != bind.Read {
		return "", fmt.Errorf("cannot verify read effect: %s/telara_tool_search is not available as read-only", server)
	}
	denied, err := br.Denied(search)
	if err != nil {
		return "", fmt.Errorf("cannot verify read effect: checking tool_search permission: %w", err)
	}
	if denied {
		return "", fmt.Errorf("cannot verify read effect: %s/telara_tool_search is denied", server)
	}
	if ak, ok := br.(bridge.Asker); ok {
		asks, err := ak.Asks(search)
		if err != nil {
			return "", fmt.Errorf("cannot verify read effect: checking tool_search approval: %w", err)
		}
		if asks {
			return "", fmt.Errorf("cannot verify read effect: %s/telara_tool_search asks for approval", server)
		}
	}
	result, err := br.Call(search, map[string]any{"integration": integration, "query": action})
	if err != nil {
		return "", fmt.Errorf("cannot verify read effect for %s/%s: %w", integration, action, err)
	}
	toolName := "telara_" + integration + "_" + action
	return parseTelaraActionEffect(result, toolName)
}

var telaraEffectLine = regexp.MustCompile(`(?m)^-\s+\*\*([^*]+)\*\*\s+\((read|write|delete|financial|identity-admin)\)`)

func parseTelaraActionEffect(result, toolName string) (string, error) {
	for _, match := range telaraEffectLine.FindAllStringSubmatch(result, -1) {
		if match[1] != toolName {
			continue
		}
		effect := match[2]
		if effect == "delete" {
			effect = string(bind.Destructive)
		}
		return effect, nil
	}
	return "", fmt.Errorf("Telara did not report an exact effect for %s", toolName)
}
