package main

import (
	"testing"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/opencode"
	"j0s.at/vibeshell/internal/adapters/opencodecli"
	"j0s.at/vibeshell/internal/domain"
)

// mustRouteID parses a route id in a test or fails it.
func mustRouteID(t *testing.T, raw string) domain.RouteID {
	t.Helper()
	id, err := domain.ParseRouteID(raw)
	if err != nil {
		t.Fatalf("parse route id %q: %v", raw, err)
	}
	return id
}

// TestBuildProvidersRegistersEachRouteWithItsKind verifies the composition
// groups routes by their configured provider and builds the implementation the
// provider's kind selects: the direct-HTTP gateway for http, the CLI adapter
// for cli. Each gateway must see only its own routes.
func TestBuildProvidersRegistersEachRouteWithItsKind(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{
				Name: "opencode",
				Products: []config.Product{{
					Name:      "console",
					BaseURL:   "https://opencode.example.internal",
					Protocols: []config.Protocol{config.ProtocolChat},
				}},
			},
			{
				Name: "opencode-cli",
				Kind: providerKindCLI,
				Products: []config.Product{{
					Name:      "console",
					Protocols: []config.Protocol{config.ProtocolChat},
				}},
			},
		},
		Routes: []config.Route{
			{ID: "rte_0123456789ABCDEFGHJKMNPQRS", Provider: "opencode", Product: "console", Model: "http-model", Protocol: config.ProtocolChat},
			{ID: "rte_0123456789ABCDEFGHJKMNPQRY", Provider: "opencode-cli", Product: "console", Model: "cli-model", Protocol: config.ProtocolChat},
		},
		Accounts: []config.Account{
			{ID: "acc_0123456789ABCDEFGHJKMNPQRS", QuotaGroup: "team-a", PermittedProducts: []string{"console"}, SecretRef: "{env:VIBESHELL_TEST_KEY}"},
			// Credential-less: it permits only a cli product, so no secret.
			{ID: "acc_0123456789ABCDEFGHJKMNPQRY", QuotaGroup: "team-b", PermittedProducts: []string{"console"}},
		},
	}
	registry, err := buildProviders(cfg, &config.Snapshot{Config: cfg})
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	if registry.Len() != 2 {
		t.Fatalf("registry.Len() = %d, want 2", registry.Len())
	}

	httpRoute := mustRouteID(t, "rte_0123456789ABCDEFGHJKMNPQRS")
	cliRoute := mustRouteID(t, "rte_0123456789ABCDEFGHJKMNPQRY")

	httpGateway, ok := registry.Provider(httpRoute)
	if !ok {
		t.Fatal("no provider registered for the http route")
	}
	gw, ok := httpGateway.(*opencode.Gateway)
	if !ok {
		t.Fatalf("http route provider is %T, want *opencode.Gateway", httpGateway)
	}
	if _, ok := gw.Routes[httpRoute]; !ok {
		t.Fatal("http gateway is missing its own route")
	}
	if _, ok := gw.Routes[cliRoute]; ok {
		t.Fatal("http gateway must not carry the cli provider's route")
	}
	if len(gw.Accounts) != 1 {
		t.Fatalf("http gateway has %d accounts, want only the credentialed one", len(gw.Accounts))
	}

	cliGateway, ok := registry.Provider(cliRoute)
	if !ok {
		t.Fatal("no provider registered for the cli route")
	}
	adapter, ok := cliGateway.(*opencodecli.Adapter)
	if !ok {
		t.Fatalf("cli route provider is %T, want *opencodecli.Adapter", cliGateway)
	}
	if adapter.ProviderID != "opencode-cli" {
		t.Fatalf("cli adapter ProviderID = %q, want opencode-cli", adapter.ProviderID)
	}
	if adapter.Binary != defaultCLIBinary {
		t.Fatalf("cli adapter Binary = %q, want the default %q", adapter.Binary, defaultCLIBinary)
	}
	if got := adapter.Models[cliRoute]; got != "cli-model" {
		t.Fatalf("cli adapter model = %q, want cli-model", got)
	}
}

// TestBuildProvidersCLIUsesConfiguredBinary verifies a cli provider's product
// base_url is taken as the CLI executable path rather than an HTTP endpoint.
func TestBuildProvidersCLIUsesConfiguredBinary(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "opencode-cli",
			Kind: providerKindCLI,
			Products: []config.Product{{
				Name:      "console",
				BaseURL:   "/usr/local/bin/opencode",
				Protocols: []config.Protocol{config.ProtocolChat},
			}},
		}},
		Routes: []config.Route{{ID: "rte_0123456789ABCDEFGHJKMNPQRS", Provider: "opencode-cli", Product: "console", Model: "cli-model"}},
	}
	registry, err := buildProviders(cfg, &config.Snapshot{Config: cfg})
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	gateway, ok := registry.Provider(mustRouteID(t, "rte_0123456789ABCDEFGHJKMNPQRS"))
	if !ok {
		t.Fatal("no provider registered for the cli route")
	}
	adapter, ok := gateway.(*opencodecli.Adapter)
	if !ok {
		t.Fatalf("provider is %T, want *opencodecli.Adapter", gateway)
	}
	if adapter.Binary != "/usr/local/bin/opencode" {
		t.Fatalf("cli adapter Binary = %q, want the configured path", adapter.Binary)
	}
}

// TestBuildProvidersRejectsUnknownKind verifies the composition fails loudly
// rather than silently serving no provider when the schema and the switch
// disagree.
func TestBuildProvidersRejectsUnknownKind(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:     "weird",
			Kind:     "smoke-signals",
			Products: []config.Product{{Name: "p", Protocols: []config.Protocol{config.ProtocolChat}}},
		}},
		Routes: []config.Route{{ID: "rte_0123456789ABCDEFGHJKMNPQRS", Provider: "weird", Product: "p", Model: "m"}},
	}
	if _, err := buildProviders(cfg, &config.Snapshot{Config: cfg}); err == nil {
		t.Fatal("expected an error for an unknown provider kind")
	}
}
