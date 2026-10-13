package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

func TestPublicDeployExamplesUseGenericNamespaceAndRefuseUndeclaredDelete(t *testing.T) {
	for _, example := range []struct{ name, namespaceInput string }{
		{"deploy-check-py", `value["namespace"]`},
		{"deploy-check-js", `input.namespace`},
	} {
		t.Run(example.name, func(t *testing.T) {
			dir := filepath.Join("..", "examples", example.name)
			raw, err := os.ReadFile(filepath.Join(dir, "primitive.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			m, err := mf.Parse(raw)
			if err != nil {
				t.Fatalf("parse manifest: %v", err)
			}
			if problems := m.RunProblems(); len(problems) > 0 {
				t.Fatalf("manifest cannot run: %v", problems)
			}
			if m.Metadata.Version != "0.1.1" {
				t.Fatalf("public example version = %q, want 0.1.1", m.Metadata.Version)
			}
			if _, ok := m.Interface.InputSchema["properties"].(map[string]any)["namespace"]; !ok {
				t.Fatal("manifest does not expose the namespace input")
			}

			// The command broker accepts a caller-selected namespace under the
			// generic -n <any> declaration, without requiring Telara's namespace.
			args := []string{"get", "deploy", "-n", "customer-workloads", "--context", "minikube", "-o", "json"}
			decl, effect := resolve(m, "kubectl", args)
			if decl == nil || effect != "read" {
				t.Fatalf("generic namespace request was not declared read-only: decl=%v effect=%q", decl, effect)
			}

			// Even --approve cannot authorize an invocation with no manifest
			// declaration. The broker must refuse it before looking up kubectl.
			var journal bytes.Buffer
			got := runCommand(context.Background(), bridge.Proc{}, m, request{
				Command: "kubectl",
				Args:    []string{"delete", "pod", "x", "-n", "customer-workloads", "--context", "minikube"},
			}, true, &journal)
			if got.Refused != "command not declared in primitive.yaml" || got.Gated {
				t.Fatalf("approved undeclared delete = %#v; want an undeclared-command refusal", got)
			}
			if !strings.Contains(journal.String(), `"outcome":"refused_undeclared"`) {
				t.Fatalf("undeclared delete was not recorded as refused: %s", journal.String())
			}

			source, err := os.ReadFile(filepath.Join(dir, m.Execution.Entrypoint))
			if err != nil {
				t.Fatal(err)
			}
			text := string(source)
			if strings.Contains(text, "telara-agents") || !strings.Contains(text, `"default"`) || !strings.Contains(text, example.namespaceInput) {
				t.Fatalf("example must use a generic default namespace, source contains internal namespace or no default: %s", text)
			}
		})
	}
}
