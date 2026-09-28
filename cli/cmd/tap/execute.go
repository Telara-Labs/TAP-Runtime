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
	"google.golang.org/protobuf/types/known/structpb"
)

// runExecute is a minimal wrapper over ExecutePrimitive (B-EXEC3's
// activation of the RPC published in telara-proto), added alongside `tap
// adopt` so the real wire surface can be smoke-tested the same way the rest
// of the registry/handshake CLI already is -- same env-driven auth/target
// convention as `tap publish`/`tap get`/`tap adopt` (TAP_REGISTRY_*).
//
//	tap execute <publisher>/<name>[@version] --mcp-config-id X --idempotency-key K [--agent-id X] [--inputs-json '{"k":"v"}'] [--timeout-seconds N] [--json]
func runExecute(args []string) error {
	fs := flag.NewFlagSet("execute", flag.ContinueOnError)
	agentID := fs.String("agent-id", "", "caller/installation identity for the adoption lookup (blank = tenant-wide)")
	mcpConfigID := fs.String("mcp-config-id", "", "MCP config the ephemeral execution agent attaches to")
	idempotencyKey := fs.String("idempotency-key", "", "durable run identity (TENG-2932 / X2; required)")
	inputsJSON := fs.String("inputs-json", "{}", "JSON object matching the manifest's inputSchema")
	timeoutSeconds := fs.Int("timeout-seconds", 0, "0 = use the manifest's execution.timeoutSeconds")
	createdBy := fs.String("created-by", "", "real tenant user UUID (agent_definitions.created_by has an FK to the tenant's users table -- $USER will NOT work)")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"agent-id": true, "mcp-config-id": true, "idempotency-key": true, "inputs-json": true, "timeout-seconds": true, "created-by": true})); err != nil {
		return newCliError(2, "%v", err)
	}
	if fs.NArg() < 1 {
		return newCliError(2, "usage: tap execute <publisher>/<name>[@version] --mcp-config-id X --idempotency-key K [--agent-id X] [--inputs-json '{...}'] [--json]")
	}
	if strings.TrimSpace(*mcpConfigID) == "" {
		return newCliError(2, "--mcp-config-id is required")
	}
	if strings.TrimSpace(*idempotencyKey) == "" {
		return newCliError(2, "--idempotency-key is required (TENG-2932 durable run identity)")
	}
	publisher, name, version, err := parsePrimitiveRef(fs.Arg(0))
	if err != nil {
		return newCliError(2, "%v", err)
	}

	var inputsMap map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(*inputsJSON), &inputsMap); jsonErr != nil {
		return newCliError(2, "invalid --inputs-json: %v", jsonErr)
	}
	inputsStruct, structErr := structpb.NewStruct(inputsMap)
	if structErr != nil {
		return newCliError(2, "--inputs-json does not convert to a struct: %v", structErr)
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx = authContext(ctx, tenantID)

	resp, err := client.ExecutePrimitive(ctx, &agentsProto.ExecutePrimitiveRequest{
		TenantId:       tenantID,
		Publisher:      publisher,
		Name:           name,
		Version:        version,
		Inputs:         inputsStruct,
		AgentId:        *agentID,
		Caller:         *createdBy,
		TimeoutSeconds: int32(*timeoutSeconds),
		McpConfigId:    *mcpConfigID,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		return newCliError(3, "execute rpc failed: %v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	} else {
		fmt.Printf("execute %s/%s@%s: execution_id=%s status=%s output_schema_valid=%v blockers=%v\n",
			publisher, name, version, resp.GetExecutionId(), resp.GetStatus(), resp.GetOutputSchemaValid(), resp.GetBlockers())
	}
	if resp.GetStatus() != "succeeded" {
		return newCliError(1, "execute did not reach status=succeeded (got %q), blockers=%v", resp.GetStatus(), resp.GetBlockers())
	}
	return nil
}
