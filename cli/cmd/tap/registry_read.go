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
	"google.golang.org/grpc/metadata"
)

// runGet and runList are minimal read-side wrappers over GetPrimitive /
// ListPrimitives (B4's registry RPCs), added by C1 alongside the least-
// privilege publish-pipeline refactor so the multi-publish/multi-version
// correctness e2e (13-execution-plan.md C1 deliverable) can prove published
// rows are actually retrievable through the registry API, not just visible
// in `tap publish`'s own response. Same env-driven auth/target convention as
// `tap publish` (TAP_REGISTRY_*); reuses dialRegistry.
//
//	tap get <publisher>/<name>[@version] [--json]   # version empty = latest active
//	tap list [--status <status>] [--json]

func runGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return newCliError(2, "%v", err)
	}
	if fs.NArg() < 1 {
		return newCliError(2, "usage: tap get <publisher>/<name>[@version] [--json]")
	}
	publisher, name, version, err := parsePrimitiveRef(fs.Arg(0))
	if err != nil {
		return newCliError(2, "%v", err)
	}

	tenantID := os.Getenv("TAP_REGISTRY_TENANT_ID")
	if tenantID == "" {
		return newCliError(2, "TAP_REGISTRY_TENANT_ID must be set")
	}
	conn, err := dialRegistry(registryAddr())
	if err != nil {
		return newCliError(3, "dial registry: %v", err)
	}
	defer conn.Close()
	client := agentsProto.NewAgentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = authContext(ctx, tenantID)

	resp, err := client.GetPrimitive(ctx, &agentsProto.GetPrimitiveRequest{
		TenantId:  tenantID,
		Publisher: publisher,
		Name:      name,
		Version:   version,
	})
	if err != nil {
		return newCliError(3, "get rpc failed: %v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	v := resp.GetVersion()
	if v == nil {
		fmt.Println("not found")
		return nil
	}
	fmt.Printf("%s/%s@%s\n  id: %s\n  digest: %s\n  status: %s\n  trust_class: %s\n  sealed: %v\n",
		v.GetPublisher(), v.GetName(), v.GetVersion(), v.GetId(), v.GetArtifactDigest(), v.GetStatus(), v.GetTrustClass(), v.GetSealed())
	if att := v.GetAttestation(); att != nil {
		fmt.Printf("  validated: %v\n  scan_status: %s\n  signed_at: %s\n", att.GetValidated(), att.GetScanStatus(), att.GetSignedAt())
	}
	return nil
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	statusFilter := fs.String("status", "", "filter by lifecycle status (active|deprecated|revoked|advisory)")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return newCliError(2, "%v", err)
	}

	tenantID := os.Getenv("TAP_REGISTRY_TENANT_ID")
	if tenantID == "" {
		return newCliError(2, "TAP_REGISTRY_TENANT_ID must be set")
	}
	conn, err := dialRegistry(registryAddr())
	if err != nil {
		return newCliError(3, "dial registry: %v", err)
	}
	defer conn.Close()
	client := agentsProto.NewAgentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = authContext(ctx, tenantID)

	resp, err := client.ListPrimitives(ctx, &agentsProto.ListPrimitivesRequest{
		TenantId:     tenantID,
		StatusFilter: *statusFilter,
	})
	if err != nil {
		return newCliError(3, "list rpc failed: %v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	for _, p := range resp.GetPrimitives() {
		fmt.Printf("%s/%s  latest=%s  status=%s\n", p.GetPublisher(), p.GetName(), p.GetLatestVersion(), p.GetStatus())
	}
	return nil
}

// registryAddr mirrors runPublish's TAP_REGISTRY_ADDR default resolution.
func registryAddr() string {
	if addr := os.Getenv("TAP_REGISTRY_ADDR"); addr != "" {
		return addr
	}
	return "agent-service.telara-agents.svc.cluster.local:50051"
}

// warnIfLooksLikeProd is a UX-only guardrail (server-side enforcement is the
// real gate -- see agent-service's TAP_ENABLED fail-closed check, which will
// reject every one of these calls in a disabled/prod environment regardless
// of this warning). It cannot reliably distinguish "inside a prod cluster"
// from "inside a local minikube cluster" when both resolve the SAME
// in-cluster DNS name from within their own cluster -- so it only flags the
// cases that are actually distinguishable from outside: an address that
// names a "prod" environment explicitly, or the public telara.dev domain
// (as opposed to in-cluster .svc.cluster.local / localhost / an explicit
// local override). Prints to stderr; never blocks the call.
func warnIfLooksLikeProd(addr string) {
	lower := strings.ToLower(addr)
	looksProd := strings.Contains(lower, "prod") ||
		(strings.Contains(lower, "telara.dev") && !strings.Contains(lower, "svc.cluster.local"))
	if !looksProd {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: TAP_REGISTRY_ADDR %q looks like it may point at a production environment -- TAP publish/adopt/execute will be rejected there unless TAP_ENABLED=true is explicitly set on that deployment\n", addr)
}

// authContext mirrors runPublish's bearer-token + x-tenant-id metadata setup.
func authContext(ctx context.Context, tenantID string) context.Context {
	if token := os.Getenv("TAP_REGISTRY_TOKEN"); token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	return metadata.AppendToOutgoingContext(ctx, "x-tenant-id", tenantID)
}

// parsePrimitiveRef splits "publisher/name@version" (version optional) the
// same way 15-adoption-surfaces.md's primitive_ref convention is documented.
func parsePrimitiveRef(ref string) (publisher, name, version string, err error) {
	rest := ref
	for i := 0; i < len(rest); i++ {
		if rest[i] == '@' {
			version = rest[i+1:]
			rest = rest[:i]
			break
		}
	}
	slash := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 {
		return "", "", "", fmt.Errorf("ref %q must be of the form publisher/name[@version]", ref)
	}
	publisher = rest[:slash]
	name = rest[slash+1:]
	if publisher == "" || name == "" {
		return "", "", "", fmt.Errorf("ref %q must be of the form publisher/name[@version]", ref)
	}
	return publisher, name, version, nil
}
