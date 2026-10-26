package redact

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func oauthFixture(flow string) map[string]any {
	value := map[string]any{"authorizationUrl": "https://example.invalid/authorize", "tokenUrl": "https://example.invalid/token", "refreshUrl": "https://example.invalid/refresh", "scopes": map[string]any{"mcp:read": "Read approved data", "authorization": "Authorize operations", "password": "Manage password policy"}}
	return map[string]any{"securitySchemes": map[string]any{"oauth2": map[string]any{"type": "oauth2", "flows": map[string]any{flow: value}}}}
}

func TestOAuthFlowMetadataPreservesCleanBytes(t *testing.T) {
	for _, flow := range oauthFlowNames {
		t.Run(flow, func(t *testing.T) {
			value := oauthFixture(flow)
			raw, _ := json.Marshal(value)
			block, _ := yaml.Marshal(value)
			sources := []string{string(raw)}
			// The frozen plain-text password assignment floor also matches a YAML
			// password mapping header followed by an endpoint name. JSON discovery
			// carries no such ambiguous text span.
			if flow != "password" {
				sources = append(sources, string(block))
			}
			for _, source := range sources {
				if got := Field(nil, source); got != source {
					t.Errorf("clean flow changed %s -> %s", source, got)
				}
				if got := ScanArtifacts(map[string][]byte{"discovery.json": []byte(source)}); len(got) != 0 {
					t.Errorf("clean metadata scanner findings %v", got)
				}
			}
		})
	}
}

func TestOAuthFlowMetadataStillInspectsCredentials(t *testing.T) {
	secret := "syntheticOpaqueValue42"
	provider := "ghp_" + strings.Repeat("A", 36)
	for _, flow := range oauthFlowNames {
		cases := []any{
			map[string]any{flow: secret},
			map[string]any{flow: map[string]any{"scopes": map[string]any{}, "tokenUrl": map[string]any{"dbPassword": secret}}},
			map[string]any{flow: map[string]any{"scopes": map[string]any{"dbPassword": map[string]any{"dbPassword": secret}}, "tokenUrl": "https://example.invalid/token"}},
			map[string]any{flow: map[string]any{"scopes": map[string]any{}, "authorizationUrl": "https://example.invalid/authorize", "tokenUrl": "https://example.invalid/token", "dbPassword": secret}},
			map[string]any{flow: map[string]any{"scopes": map[string]any{"mcp:read": "password: |\n  " + secret}, "authorizationUrl": "https://example.invalid/authorize", "tokenUrl": "https://example.invalid/token"}},
			map[string]any{flow: map[string]any{"scopes": map[string]any{}, "authorizationUrl": "https://example.invalid/authorize?client_secret=" + secret, "tokenUrl": "https://example.invalid/token"}},
			map[string]any{flow: map[string]any{"scopes": map[string]any{"mcp:read": provider}, "authorizationUrl": "https://example.invalid/authorize", "tokenUrl": provider}},
			map[string]any{"credentials": map[string]any{flow: map[string]any{"scopes": map[string]any{"mcp:read": secret}, "authorizationUrl": secret, "tokenUrl": secret}}},
			map[string]any{"properties": map[string]any{"password": map[string]any{"default": map[string]any{flow: map[string]any{"scopes": map[string]any{}, "authorizationUrl": secret, "tokenUrl": secret}}}}},
		}
		for i, value := range cases {
			if i == 0 && !IsSensitiveName(flow) {
				continue
			}
			raw, _ := json.Marshal(value)
			block, _ := yaml.Marshal(value)
			for _, source := range []string{string(raw), string(block)} {
				if got := Field(nil, source); strings.Contains(got, secret) || strings.Contains(got, provider) {
					t.Errorf("%s/%d leaked %s", flow, i, got)
				}
			}
		}
	}
}

func TestOAuthDiscoveryShapeBoundaries(t *testing.T) {
	const secret = "syntheticOpaqueValue42"
	clean := map[string]any{"resource": "https://example.invalid/mcp", "authorization_servers": []any{"https://example.invalid"}, "bearer_methods_supported": []any{"header"}}
	raw, _ := json.Marshal(clean)
	if got := Field(nil, string(raw)); got != string(raw) {
		t.Fatalf("resource metadata changed %s", got)
	}
	for _, value := range []any{
		map[string]any{"authorization_servers": []any{secret}},
		map[string]any{"resource": "https://example.invalid/mcp", "authorization_servers": []any{secret}},
		map[string]any{"resource": "https://example.invalid/mcp", "authorization_servers": []any{"https://example.invalid"}, "bearer_methods_supported": []any{secret}},
		map[string]any{"resource": "https://example.invalid/mcp", "authorization_servers": []any{map[string]any{"dbPassword": secret}}},
		map[string]any{"resource": "https://example.invalid/mcp", "authorization_servers": []any{"https://example.invalid"}, "bearer_methods_supported": map[string]any{"dbPassword": secret}},
		map[string]any{"credentials": clean, "password": secret},
		map[string]any{"issuer": "https://example.invalid", "authorization_endpoint": "https://example.invalid/authorize", "token_endpoint": "https://example.invalid/token", "authorization_response_iss_parameter_supported": map[string]any{"dbPassword": secret}},
		map[string]any{"issuer": "https://example.invalid", "authorization_endpoint": "https://example.invalid/authorize", "token_endpoint": "https://example.invalid/token", "authorization_response_iss_parameter_supported": "password: |\n  " + secret},
		map[string]any{"authorizationCode": map[string]any{"scopes": map[string]any{}, "authorizationUrl": secret, "tokenUrl": secret}},
		map[string]any{"deviceCode": secret},
	} {
		raw, _ := json.Marshal(value)
		block, _ := yaml.Marshal(value)
		for _, source := range []string{string(raw), string(block)} {
			if got := Field(nil, source); strings.Contains(got, secret) {
				t.Errorf("discovery shape bypass %s", got)
			}
		}
	}
}
