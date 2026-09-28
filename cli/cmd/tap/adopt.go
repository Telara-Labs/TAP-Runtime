package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	agentsProto "gitlab.com/telara-labs/telara-proto/protos-go/agents/agent_service"
)

// runAdopt is a minimal wrapper over AdoptPrimitive (B5's runtime handshake),
// added by B-EXEC alongside `tap get`/`tap list` so the live execute proof
// can adopt a primitive through the real handshake (fail-closed authority
// intersection + trust x effect matrix) before ExecutePrimitive's own
// adoption/preflight check will let it run — same env-driven auth/target
// convention as `tap publish`/`tap get` (TAP_REGISTRY_*).
//
//	tap adopt <publisher>/<name>@<version> --bind <slot>=<credential_id>[,...] [--agent-id X] [--json]
func runAdopt(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	bind := fs.String("bind", "", "comma-separated slot=credential_id pairs, e.g. gitlab=abc123")
	agentID := fs.String("agent-id", "", "caller/installation identity (blank = tenant-wide)")
	mcpConfigID := fs.String("mcp-config-id", "", "MCP config the action-gate policy check resolves against (required for any non-empty verdict other than blocked)")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"bind": true, "agent-id": true, "mcp-config-id": true})); err != nil {
		return newCliError(2, "%v", err)
	}
	if fs.NArg() < 1 {
		return newCliError(2, "usage: tap adopt <publisher>/<name>@<version> --bind <slot>=<credential_id>[,...] [--agent-id X] [--json]")
	}
	publisher, name, version, err := parsePrimitiveRef(fs.Arg(0))
	if err != nil {
		return newCliError(2, "%v", err)
	}
	if version == "" {
		return newCliError(2, "adopt requires an explicit @version (the handshake binds to one exact published version)")
	}

	var bindings []*agentsProto.PrimitiveSlotBinding
	if *bind != "" {
		for _, pair := range strings.Split(*bind, ",") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
				return newCliError(2, "invalid --bind entry %q; expected slot=credential_id", pair)
			}
			bindings = append(bindings, &agentsProto.PrimitiveSlotBinding{Slot: kv[0], CredentialId: kv[1]})
		}
	}

	tenantID := os.Getenv("TAP_REGISTRY_TENANT_ID")
	if tenantID == "" {
		return newCliError(2, "TAP_REGISTRY_TENANT_ID must be set")
	}
	addr := registryAddr()
	warnIfLooksLikeProd(addr)
	conn, err := dialRegistry(addr)
	if err != nil {
		return newCliError(3, "dial registry: %v", err)
	}
	defer conn.Close()
	client := agentsProto.NewAgentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = authContext(ctx, tenantID)

	resp, err := client.AdoptPrimitive(ctx, &agentsProto.AdoptPrimitiveRequest{
		TenantId:    tenantID,
		AgentId:     *agentID,
		Publisher:   publisher,
		Name:        name,
		Version:     version,
		McpConfigId: *mcpConfigID,
		Bindings:    bindings,
	})
	if err != nil {
		return newCliError(3, "adopt rpc failed: %v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	} else {
		fmt.Printf("adopt %s/%s@%s: verdict=%s installation_id=%s execution_mode=%s effective_gate=%s\n",
			publisher, name, version, resp.GetVerdict(), resp.GetInstallationId(), resp.GetExecutionMode(), resp.GetEffectiveGate())
	}
	if resp.GetVerdict() != "adopted" {
		return newCliError(1, "adopt did not reach verdict=adopted (got %q)", resp.GetVerdict())
	}
	return nil
}
