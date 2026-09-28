package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"telara.dev/tap/internal/model"

	agentsProto "gitlab.com/telara-labs/telara-proto/protos-go/agents/agent_service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// runPublish sends a package to the TAP registry (agent-service, B4:
// telara-documentation/architecture/tap/14-registry-design.md §5). The
// registry runs the full 5-stage pipeline server-side and returns the
// signed attestation; this command's only jobs are packaging (tar+gzip the
// directory) and the gRPC round trip. No key management happens here (the
// CLI never signs or seals anything — per 04-cli.md / 14 §3, sealing and
// attestation signing are registry-side operations against Vault transit).
//
// Auth/target are environment-driven, per the "keep it minimal" CLI rule:
//
//	TAP_REGISTRY_ADDR              host:port of agent-service (default: in-cluster DNS name)
//	TAP_REGISTRY_TOKEN              bearer token forwarded as gRPC metadata (authorization: Bearer <token>)
//	TAP_REGISTRY_TENANT_ID           tenant id (required)
//	TAP_REGISTRY_INSECURE_SKIP_VERIFY set to skip TLS verification for local/dev registries
//	TAP_REGISTRY_PLAINTEXT           set to dial without TLS entirely (local dev only)
func runPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	seal := fs.Bool("seal", false, "envelope-encrypt the implementation bundle (14 §3); manifest stays inspectable either way")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return newCliError(2, "%v", err)
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}

	pkg, err := model.LoadPackage(dir)
	if err != nil {
		return newCliError(3, "%v", err)
	}

	addr := os.Getenv("TAP_REGISTRY_ADDR")
	if addr == "" {
		// C0 finding: agent-service's actual gRPC port is 50051 (main.go GRPC_PORT
		// default "8443" is dead code — the Deployment always sets GRPC_PORT=50051
		// via containerPort/env; the k8s Service also only exposes 50051/TCP).
		addr = "agent-service.telara-agents.svc.cluster.local:50051"
	}
	tenantID := os.Getenv("TAP_REGISTRY_TENANT_ID")
	if tenantID == "" {
		return newCliError(2, "TAP_REGISTRY_TENANT_ID must be set")
	}
	publisher := pkg.Manifest.Metadata.Publisher
	if publisher == "" {
		return newCliError(3, "primitive.yaml metadata.publisher is empty")
	}

	tarball, err := buildPackageTarGz(pkg.Dir)
	if err != nil {
		return newCliError(3, "packaging %s: %v", pkg.Dir, err)
	}

	warnIfLooksLikeProd(addr)
	conn, err := dialRegistry(addr)
	if err != nil {
		return newCliError(3, "dial registry %s: %v", addr, err)
	}
	defer conn.Close()

	client := agentsProto.NewAgentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) // publish runs a full lint+test+scan pipeline server-side
	defer cancel()
	if token := os.Getenv("TAP_REGISTRY_TOKEN"); token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	// B-INT follow-up: TenantConnectionInterceptor resolves tenant from the JWT
	// claims first, but also accepts an x-tenant-id metadata fallback — send it
	// alongside the bearer token for defense-in-depth (harmless when the JWT
	// claim already carries tenant_id).
	ctx = metadata.AppendToOutgoingContext(ctx, "x-tenant-id", tenantID)

	resp, err := client.PublishPrimitive(ctx, &agentsProto.PublishPrimitiveRequest{
		TenantId:       tenantID,
		Publisher:      publisher,
		PackageTarball: tarball,
		Seal:           *seal,
		CreatedBy:      os.Getenv("USER"),
	})
	if err != nil {
		return newCliError(3, "publish rpc failed: %v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	} else {
		printPublishResult(resp)
	}

	if !resp.GetAccepted() {
		return newCliError(1, "publish REJECTED: %d blocker(s)", len(resp.GetBlockers()))
	}
	return nil
}

func printPublishResult(resp *agentsProto.PublishPrimitiveResponse) {
	v := resp.GetVersion()
	if resp.GetAccepted() {
		fmt.Printf("publish OK: %s/%s@%s\n", v.GetPublisher(), v.GetName(), v.GetVersion())
		fmt.Printf("  artifact_digest: %s\n", v.GetArtifactDigest())
		fmt.Printf("  trust_class:     %s\n", v.GetTrustClass())
		fmt.Printf("  sealed:          %v\n", v.GetSealed())
		if att := v.GetAttestation(); att != nil {
			fmt.Printf("  scan_status:     %s\n", att.GetScanStatus())
			fmt.Printf("  signed_at:       %s\n", att.GetSignedAt())
		}
		return
	}
	fmt.Println("publish REJECTED:")
	for _, b := range resp.GetBlockers() {
		fmt.Println("  - " + b)
	}
}

// dialRegistry connects to agent-service. Defaults to TLS (agent-service
// terminates TLS on its gRPC port per cmd/server/main.go's
// NewServerTLSConfig); TAP_REGISTRY_PLAINTEXT selects an insecure local dial
// for dev, and TAP_REGISTRY_INSECURE_SKIP_VERIFY skips cert verification
// (self-signed/dev clusters) while still encrypting the channel.
//
// mTLS client cert (B-INT follow-up, C0 fix): agent-service's gRPC server
// requires tls.RequireAndVerifyClientCert unconditionally (every environment,
// no dev bypass — telara-utilities/go/security/tls.go CreateServerTLSConfig).
// Every in-cluster caller presents ITS OWN vault-issued identity cert as the
// client cert for outbound calls (telara-utilities/go/grpc/tls.go
// NewGRPCClient does exactly this — same TLS_CERT/TLS_KEY/TLS_CA triple used
// for both server and client roles); the CA only checks chain-of-trust, not
// caller identity. TAP_REGISTRY_CLIENT_CERT_PATH / _KEY_PATH / _CA_PATH let
// this CLI do the same when run from inside a pod that already has a
// decrypted identity cert on disk (e.g. agent-service's own /tmp/tls.crt,
// /tmp/tls.key, /tmp/ca.crt, written by its vault-agent-templated entrypoint).
func dialRegistry(addr string) (*grpc.ClientConn, error) {
	if os.Getenv("TAP_REGISTRY_PLAINTEXT") != "" {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	tlsCfg := &tls.Config{}
	if os.Getenv("TAP_REGISTRY_INSECURE_SKIP_VERIFY") != "" {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit opt-in for local/dev registries
	}
	certPath := os.Getenv("TAP_REGISTRY_CLIENT_CERT_PATH")
	keyPath := os.Getenv("TAP_REGISTRY_CLIENT_KEY_PATH")
	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load mTLS client cert/key (%s, %s): %w", certPath, keyPath, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if caPath := os.Getenv("TAP_REGISTRY_CA_PATH"); caPath != "" {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read mTLS CA %s: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("no certificates parsed from CA file %s", caPath)
		}
		tlsCfg.RootCAs = pool
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
}

// buildPackageTarGz packs a TAP package directory into the tar+gzip bundle
// format the registry expects (mirrors agent-service's internal/tap
// extractTarGz counterpart).
func buildPackageTarGz(dir string) ([]byte, error) {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)

	go func() {
		defer pw.Close()
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)
		walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if info.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		if walkErr != nil {
			errCh <- walkErr
			return
		}
		if err := tw.Close(); err != nil {
			errCh <- err
			return
		}
		errCh <- gw.Close()
	}()

	data, readErr := io.ReadAll(pr)
	if err := <-errCh; err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	return data, nil
}
