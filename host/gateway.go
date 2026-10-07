package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// gatewayOperation is a live operation advertised by a connected host tool.
// Its name and schema participate in ordinary binding; its transport stays
// private to the host. No connector credentials or business mappings live here.
type gatewayOperation struct {
	tool          bind.Tool
	operationTool bind.Tool
	dispatch      telaraDispatch
	schema        map[string]any
}

func operationKey(t bind.Tool) string { return t.Server + "\x00" + t.Name }

// A server chooser cannot distinguish two different operations on that same
// gateway. Require a narrower capability/contract instead of picking by name.
func ambiguousGatewayOperations(candidates []bind.Candidate, routes map[string]gatewayOperation) string {
	if len(candidates) == 0 {
		return ""
	}
	byServer := map[string][]bind.Candidate{}
	for _, c := range candidates {
		if c.Score != candidates[0].Score {
			continue
		}
		byServer[c.Tool.Server] = append(byServer[c.Tool.Server], c)
	}
	for _, c := range candidates {
		matches := byServer[c.Tool.Server]
		if len(matches) < 2 {
			continue
		}
		for _, match := range matches {
			if _, gateway := routes[operationKey(match.Tool)]; gateway {
				return fmt.Sprintf("multiple operations on %q fit equally well; narrow the capability or argument contract", c.Tool.Server)
			}
		}
	}
	return ""
}

// catalogCall borrows the same client session as execution, respecting its
// permission rules. Admission never auto-approves a discovery tool.
func catalogCall(br bridge.Bridge, t bind.Tool, args map[string]any) (string, error) {
	if t.Annotated != bind.Read {
		return "", fmt.Errorf("%s/%s is not annotated read-only", t.Server, t.Name)
	}
	denied, err := br.Denied(t)
	if err != nil {
		return "", err
	}
	if denied {
		return "", fmt.Errorf("%s/%s is denied", t.Server, t.Name)
	}
	if asker, ok := br.(bridge.Asker); ok {
		asks, err := asker.Asks(t)
		if err != nil {
			return "", err
		}
		if asks {
			return "", fmt.Errorf("%s/%s needs approval for discovery", t.Server, t.Name)
		}
	}
	return br.Call(t, args)
}

var gatewayHeading = regexp.MustCompile(`(?m)^(?:-\s+\*\*([^*]+)\*\*|##\s+(\S+))\s+\((read|write|delete|destructive|financial|identity-admin)\)`)
var gatewayEnvelope = regexp.MustCompile(`(?m)^\s*(?:Run it with: )?telara_execute_action\s+(\{[^\n]+\})\s*$`)
var gatewayIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)
var gatewaySchema = regexp.MustCompile("(?s)### Parameters\\s*```json\\s*(.*?)\\s*```")

type advertisedOperation struct {
	name, effect, integration, action string
}

// The adapter reads the gateway's explicit dispatch envelope. Splitting a
// prefixed tool name cannot establish its integration or authority.
func advertisedOperations(raw string) []advertisedOperation {
	var out []advertisedOperation
	headings := gatewayHeading.FindAllStringSubmatchIndex(raw, -1)
	for i, h := range headings {
		end := len(raw)
		if i+1 < len(headings) {
			end = headings[i+1][0]
		}
		name := ""
		if h[2] >= 0 {
			name = raw[h[2]:h[3]]
		} else {
			name = raw[h[4]:h[5]]
		}
		effect := raw[h[6]:h[7]]
		if effect == "delete" {
			effect = string(bind.Destructive)
		}
		envelopes := gatewayEnvelope.FindAllStringSubmatch(raw[h[1]:end], -1)
		if len(envelopes) != 1 {
			continue
		}
		var call struct {
			Integration string         `json:"integration"`
			Action      string         `json:"action"`
			Params      map[string]any `json:"params"`
		}
		dec := json.NewDecoder(strings.NewReader(envelopes[0][1]))
		dec.DisallowUnknownFields()
		if dec.Decode(&call) != nil || !gatewayIdentifier.MatchString(call.Integration) || !gatewayIdentifier.MatchString(call.Action) || call.Params == nil || len(call.Params) != 0 {
			continue
		}
		if name != "telara_"+call.Integration+"_"+call.Action {
			continue
		}
		out = append(out, advertisedOperation{name, effect, call.Integration, call.Action})
	}
	return out
}

// operationInventory expands the gateway's live catalog on demand for one
// capability. Other gateway protocols can provide the same operation records
// through their own host adapter; ordinary MCP tools need no adapter.
func operationInventory(br bridge.Bridge, inv []bind.Tool, capability string) ([]bind.Tool, map[string]gatewayOperation, error) {
	out := append([]bind.Tool(nil), inv...)
	routes := map[string]gatewayOperation{}
	query := strings.Join(bind.Tokens(mf.CapabilityName(capability)), " ")
	var failures []string
	for _, via := range inv {
		if !isTelaraDispatcher(via) {
			continue
		}
		search, hasSearch := findTool(inv, via.Server, "telara_tool_search")
		describe, hasDescribe := findTool(inv, via.Server, "telara_tool_describe")
		if !hasSearch || !hasDescribe {
			continue
		}
		// Preview is deliberately inventory-only and cannot call the catalog.
		if _, preview := br.(inventoryBridge); preview {
			continue
		}
		denied, err := br.Denied(via)
		if err != nil {
			return nil, nil, err
		}
		if denied {
			continue
		}
		raw, err := catalogCall(br, search, map[string]any{"query": query, "limit": 20})
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", via.Server, err))
			continue
		}
		operations := advertisedOperations(raw)
		counts := map[string]int{}
		for _, op := range operations {
			counts[op.name]++
		}
		for _, op := range operations {
			if counts[op.name] != 1 {
				continue
			}
			candidate := bind.Tool{Server: via.Server, Name: op.integration + "_" + op.action, Annotated: bind.Effect(op.effect)}
			operationTool := bind.Tool{Server: via.Server, Name: op.name, Annotated: bind.Effect(op.effect)}
			denied, err := false, error(nil)
			if permissions, ok := br.(bridge.OperationDenier); ok {
				denied, err = permissions.OperationDenied(operationTool)
			} else {
				denied, err = br.Denied(operationTool)
			}
			if err != nil {
				return nil, nil, err
			}
			if denied {
				continue
			}
			if _, direct := findTool(inv, via.Server, candidate.Name); direct {
				continue
			}
			// Fetch schemas only for actions that could satisfy this operation.
			if c := bind.Resolve(capability, bind.Destructive, []bind.Tool{candidate}); c.Bound == nil {
				continue
			}
			detail, err := catalogCall(br, describe, map[string]any{"name": op.name})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: %v", via.Server, op.name, err))
				continue
			}
			confirmed := advertisedOperations(detail)
			if len(confirmed) != 1 || confirmed[0] != op {
				continue
			}
			matches := gatewaySchema.FindAllStringSubmatch(detail, -1)
			if len(matches) != 1 {
				continue
			}
			var schema map[string]any
			if json.Unmarshal([]byte(matches[0][1]), &schema) != nil || schema["type"] != "object" {
				continue
			}
			if _, err := jsonschema.CompileString("gateway-operation.json", matches[0][1]); err != nil {
				continue
			}
			candidate.Schema = schema
			key := operationKey(candidate)
			routes[key] = gatewayOperation{tool: via, operationTool: operationTool, schema: schema, dispatch: telaraDispatch{Integration: op.integration, Action: op.action, WrapArgs: true, PlainArgs: true}}
			out = append(out, candidate)
		}
	}
	if len(failures) > 0 && len(routes) == 0 {
		return out, routes, fmt.Errorf("gateway discovery failed: %s", strings.Join(failures, "; "))
	}
	return out, routes, nil
}
