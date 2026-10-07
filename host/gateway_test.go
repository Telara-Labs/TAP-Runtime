package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// This is a client-protocol double. Real connected-account runs are required
// separately; these tests exercise the runner's admission and call boundary.
type operationBridge struct {
	*fakeBridge
	catalog, detail string
	details         map[string]string
	searchErr       bool
	asks            map[string]bool
}

func (b *operationBridge) Asks(t bind.Tool) (bool, error) { return b.asks[t.Server+"/"+t.Name], nil }
func (b *operationBridge) Call(t bind.Tool, args map[string]any) (string, error) {
	b.fakeBridge.Call(t, args)
	switch t.Name {
	case "telara_tool_search":
		if b.searchErr {
			return "", errors.New("catalog unavailable")
		}
		return b.catalog, nil
	case "telara_tool_describe":
		if detail, ok := b.details[fmt.Sprint(args["name"])]; ok {
			return detail, nil
		}
		return b.detail, nil
	}
	return `{"items":[]}`, nil
}

func calendarOperationBridge() *operationBridge {
	b := &operationBridge{fakeBridge: &fakeBridge{deny: map[string]bool{}, schemas: true}, asks: map[string]bool{}}
	for _, name := range []string{"telara_execute_action", "telara_tool_search", "telara_tool_describe"} {
		effect := bind.Read
		if name == "telara_execute_action" {
			effect = bind.Destructive
		}
		b.inv = append(b.inv, bind.Tool{Server: "my gateway", Name: name, Annotated: effect})
	}
	const envelope = `telara_execute_action {"integration":"google_workspace","action":"calendar_list_events","params":{}}`
	b.catalog = "- **telara_google_workspace_calendar_list_events** (read) — Calendar\n  " + envelope + "\n"
	b.detail = "## telara_google_workspace_calendar_list_events (read)\n\nRun it with: " + envelope + "\n\n### Parameters\n```json\n" + `{"type":"object","properties":{"time_min":{"type":"string"},"max_results":{"type":"integer"}},"required":["time_min"]}` + "\n```\n"
	return b
}

func calendarDecl() toolDecl {
	return toolDecl{Alias: "calendar", Capability: "calendar.events.list", Effect: "read"}
}
func calendarContract() mf.Capability {
	return mf.Capability{Label: "calendar.events.list", Args: map[string]any{"type": "object", "properties": map[string]any{"time_min": map[string]any{"type": "string"}, "max_results": map[string]any{"type": "integer"}}, "required": []string{"time_min"}}, Result: map[string]any{"type": "object", "properties": map[string]any{"items": map[string]any{"type": "array"}}, "required": []string{"items"}}}
}

func TestUnpinnedOperationBindsGatewayAndDirectTool(t *testing.T) {
	for _, gateway := range []bool{true, false} {
		t.Run(fmt.Sprint(gateway), func(t *testing.T) {
			b := calendarOperationBridge()
			if !gateway {
				b.inv = []bind.Tool{{Server: "Google Calendar", Name: "list_events", Annotated: bind.Read, Schema: calendarContract().Args}}
			}
			a, err := admit([]toolDecl{calendarDecl()}, b, calendarContract())
			if err != nil {
				t.Fatal(err)
			}
			bd := a.byAlias["calendar"]
			if bd.Pinned || !bd.ContractChecked || !bd.ResultChecked || bd.effective() != "read" {
				t.Fatalf("binding: %+v", bd)
			}
			var journal bytes.Buffer
			r := callTool(a, b, request{Alias: "calendar", Arguments: map[string]any{"time_min": "2026-10-07T09:00:00Z", "max_results": 5}}, false, &journal)
			if r.Refused != "" || r.Exit != 0 {
				t.Fatalf("call: %+v", r)
			}
			last := b.args[len(b.args)-1]
			if gateway {
				params, _ := last["params"].(map[string]any)
				if bd.Tool != "telara_execute_action" || bd.Operation != "google_workspace/calendar_list_events" || last["integration"] != "google_workspace" || last["action"] != "calendar_list_events" || params["max_results"] != 5 {
					t.Fatalf("route: %+v %#v", bd, last)
				}
				if !strings.Contains(journal.String(), "google_workspace/calendar_list_events") {
					t.Fatal("journal lost operation")
				}
			} else if last["time_min"] == nil || last["params"] != nil {
				t.Fatalf("direct arguments were wrapped: %#v", last)
			}
		})
	}
}

func TestGatewayOperationDiscoveryRefusesUnverifiedTargets(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*operationBridge)
	}{
		{"denied search", func(b *operationBridge) { b.deny["my gateway/telara_tool_search"] = true }},
		{"denied describe", func(b *operationBridge) { b.deny["my gateway/telara_tool_describe"] = true }},
		{"denied dispatcher", func(b *operationBridge) { b.deny["my gateway/telara_execute_action"] = true }},
		{"denied operation", func(b *operationBridge) { b.deny["my gateway/telara_google_workspace_calendar_list_events"] = true }},
		{"search needs approval", func(b *operationBridge) { b.asks["my gateway/telara_tool_search"] = true }},
		{"catalog error", func(b *operationBridge) { b.searchErr = true }},
		{"missing schema", func(b *operationBridge) { b.detail = strings.Split(b.detail, "### Parameters")[0] }},
		{"unknown effect", func(b *operationBridge) { b.catalog = strings.ReplaceAll(b.catalog, "(read)", "(unknown)") }},
		{"duplicate operation", func(b *operationBridge) { b.catalog += b.catalog }},
		{"write contradicts read", func(b *operationBridge) {
			b.catalog = strings.ReplaceAll(b.catalog, "(read)", "(write)")
			b.detail = strings.ReplaceAll(b.detail, "(read)", "(write)")
		}},
		{"describe changes route", func(b *operationBridge) {
			b.detail = strings.ReplaceAll(b.detail, `"action":"calendar_list_events"`, `"action":"calendar_delete_event"`)
		}},
		{"describe changes effect", func(b *operationBridge) { b.detail = strings.ReplaceAll(b.detail, "(read)", "(write)") }},
		{"schema mismatch", func(b *operationBridge) {
			b.detail = strings.ReplaceAll(b.detail, `"max_results":{"type":"integer"}`, `"max_results":{"type":"string"}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := calendarOperationBridge()
			test.alter(b)
			if _, err := admit([]toolDecl{calendarDecl()}, b, calendarContract()); err == nil {
				t.Fatal("unverified target admitted")
			}
			for _, call := range b.calls {
				if strings.HasSuffix(call, "/telara_execute_action") {
					t.Fatal("action executed during discovery")
				}
			}
		})
	}
}

func TestGatewayOperationCannotChangeSelectorsOrEscapeArgumentSchema(t *testing.T) {
	b := calendarOperationBridge()
	a, err := admit([]toolDecl{calendarDecl()}, b, calendarContract())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"time_min": "x", "action": "calendar_delete_event"},
		{"time_min": "x", "integration": "jira"},
		{"time_min": "x", "params": map[string]any{"action": "calendar_delete_event"}},
		{"time_min": "x", "max_results": "five"},
		{},
	} {
		r := callTool(a, b, request{Alias: "calendar", Arguments: args}, true, &bytes.Buffer{})
		if r.Refused == "" {
			t.Fatalf("invalid args accepted: %#v", args)
		}
	}
	for _, call := range b.calls {
		if strings.HasSuffix(call, "/telara_execute_action") {
			t.Fatal("invalid operation dispatched")
		}
	}
}

func TestGatewayOperationsAcrossConnectionsRequireChoice(t *testing.T) {
	b := calendarOperationBridge()
	for _, tool := range append([]bind.Tool(nil), b.inv...) {
		tool.Server = "another gateway"
		b.inv = append(b.inv, tool)
	}
	if _, err := admit([]toolDecl{calendarDecl()}, b, calendarContract()); err == nil || !strings.Contains(err.Error(), "does not choose") {
		t.Fatalf("connections picked silently: %v", err)
	}
	a, err := admitWith(nil, func(p Pick) (string, bool) { return "another gateway", true }, []toolDecl{calendarDecl()}, b, calendarContract())
	if err != nil || a.Bindings[0].Server != "another gateway" {
		t.Fatalf("choice lost: %+v %v", a, err)
	}
}

func TestGatewayWriteStillGatedAndReadEffectRechecked(t *testing.T) {
	b := calendarOperationBridge()
	b.catalog = strings.ReplaceAll(b.catalog, "(read)", "(write)")
	b.detail = strings.ReplaceAll(b.detail, "(read)", "(write)")
	d := calendarDecl()
	d.Effect = "write"
	a, err := admit([]toolDecl{d}, b, calendarContract())
	if err != nil {
		t.Fatal(err)
	}
	r := callTool(a, b, request{Alias: "calendar", Arguments: map[string]any{"time_min": "x"}}, false, &bytes.Buffer{})
	if !r.Gated || r.Refused == "" {
		t.Fatalf("write bypassed approval: %+v", r)
	}
	b = calendarOperationBridge()
	a, err = admit([]toolDecl{calendarDecl()}, b, calendarContract())
	if err != nil {
		t.Fatal(err)
	}
	b.catalog = strings.ReplaceAll(b.catalog, "(read)", "(write)")
	r = callTool(a, b, request{Alias: "calendar", Arguments: map[string]any{"time_min": "x"}}, true, &bytes.Buffer{})
	if r.Refused == "" {
		t.Fatal("changed effect dispatched")
	}
}

func TestGatewayOperationRespectsClientActionApproval(t *testing.T) {
	b := calendarOperationBridge()
	b.asks["my gateway/telara_google_workspace_calendar_list_events"] = true
	a, err := admit([]toolDecl{calendarDecl()}, b, calendarContract())
	if err != nil {
		t.Fatal(err)
	}
	r := callTool(a, b, request{Alias: "calendar", Arguments: map[string]any{"time_min": "x"}}, false, &bytes.Buffer{})
	if !r.Gated {
		t.Fatalf("client action approval lost: %+v", r)
	}
}

func TestGatewaySchemasWorkWhenClientHasNoDirectSchemas(t *testing.T) {
	b := calendarOperationBridge()
	b.schemas = false
	b.inv = append(b.inv, bind.Tool{Server: "Google Calendar", Name: "list_events", Annotated: bind.Read})
	a, err := admit([]toolDecl{calendarDecl()}, b, calendarContract())
	if err != nil || a.Bindings[0].Tool != "telara_execute_action" || !a.Bindings[0].ContractChecked {
		t.Fatalf("unverifiable direct tool displaced the catalog schema: %+v %v", a, err)
	}
}

func TestGatewayUsesAdvertisedIntegrationNotCompiledProviderList(t *testing.T) {
	b := calendarOperationBridge()
	b.catalog = strings.ReplaceAll(b.catalog, "google_workspace", "custom_workspace")
	b.detail = strings.ReplaceAll(b.detail, "google_workspace", "custom_workspace")
	a, err := admit([]toolDecl{calendarDecl()}, b, calendarContract())
	if err != nil {
		t.Fatal(err)
	}
	r := callTool(a, b, request{Alias: "calendar", Arguments: map[string]any{"time_min": "x"}}, false, &bytes.Buffer{})
	if r.Refused != "" || b.args[len(b.args)-1]["integration"] != "custom_workspace" {
		t.Fatalf("advertised integration lost: %+v", r)
	}
}

func TestSameGatewayDifferentOperationsAreAmbiguous(t *testing.T) {
	b := calendarOperationBridge()
	b.catalog += strings.ReplaceAll(b.catalog, "google_workspace", "other_workspace")
	b.details = map[string]string{
		"telara_google_workspace_calendar_list_events": b.detail,
		"telara_other_workspace_calendar_list_events":  strings.ReplaceAll(b.detail, "google_workspace", "other_workspace"),
	}
	if _, err := admit([]toolDecl{calendarDecl()}, b, calendarContract()); err == nil || !strings.Contains(err.Error(), "multiple operations") {
		t.Fatalf("same-server operations silently selected: %v", err)
	}
}

type transportAllowListBridge struct {
	*operationBridge
	denyOperation bool
}

func (b *transportAllowListBridge) Denied(t bind.Tool) (bool, error) {
	return strings.HasPrefix(t.Name, "telara_google_workspace_"), nil
}
func (b *transportAllowListBridge) OperationDenied(bind.Tool) (bool, error) {
	return b.denyOperation, nil
}

func TestGatewayBindingWithCallableToolAllowList(t *testing.T) {
	b := &transportAllowListBridge{operationBridge: calendarOperationBridge()}
	if _, err := admit([]toolDecl{calendarDecl()}, b, calendarContract()); err != nil {
		t.Fatal(err)
	}
	b.denyOperation = true
	if _, err := admit([]toolDecl{calendarDecl()}, b, calendarContract()); err == nil {
		t.Fatal("explicit operation deny ignored")
	}
}

func TestGatewayWriteRefusesWhenCatalogBecomesUnavailable(t *testing.T) {
	b := calendarOperationBridge()
	b.catalog = strings.ReplaceAll(b.catalog, "(read)", "(write)")
	b.detail = strings.ReplaceAll(b.detail, "(read)", "(write)")
	d := calendarDecl()
	d.Effect = "write"
	a, err := admit([]toolDecl{d}, b, calendarContract())
	if err != nil {
		t.Fatal(err)
	}
	b.searchErr = true
	r := callTool(a, b, request{Alias: "calendar", Arguments: map[string]any{"time_min": "x"}}, true, &bytes.Buffer{})
	if r.Refused == "" {
		t.Fatal("write executed with an unverifiable current effect")
	}
	for _, call := range b.calls {
		if strings.HasSuffix(call, "/telara_execute_action") {
			t.Fatal("unverified write dispatched")
		}
	}
}

func TestGatewayEffectUsesFixedOperationRatherThanBusinessParameterWords(t *testing.T) {
	b := calendarOperationBridge()
	b.detail = strings.ReplaceAll(b.detail, `"properties":{`, `"properties":{"marker":{"type":"string"},"payload":{"type":"object"},`)
	b.inv = append(b.inv, bind.Tool{Server: "other", Name: "safe", Annotated: bind.Read})
	a, err := admit([]toolDecl{calendarDecl()}, b)
	if err != nil {
		t.Fatal(err)
	}
	b.catalog = strings.ReplaceAll(b.catalog, "(read)", "(write)")
	r := callTool(a, b, request{Alias: "calendar", Arguments: map[string]any{"time_min": "x", "marker": "safe", "payload": map[string]any{}}}, true, &bytes.Buffer{})
	if r.Refused == "" {
		t.Fatal("business parameter words replaced the fixed operation's effect")
	}
}
