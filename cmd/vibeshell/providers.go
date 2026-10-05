package main

import (
	"fmt"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/opencode"
	"j0s.at/vibeshell/internal/adapters/opencodecli"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/provider"
	"j0s.at/vibeshell/internal/system"
)

// Provider implementation kinds. "http" talks to the provider API directly;
// "cli" runs the OpenCode CLI, which is the authenticated client for models the
// HTTP API cannot reach (the Console free tier). The kind is configuration, so
// adding an implementation is additive and changes no shell behavior.
const (
	providerKindHTTP = "http"
	providerKindCLI  = "cli"
)

// defaultCLIBinary is the CLI executable when a cli provider names none.
const defaultCLIBinary = "opencode"

// ownedRoute pairs a parsed route identity with its configuration, so a
// provider can be built from exactly the routes it owns.
type ownedRoute struct {
	ID     domain.RouteID
	Config config.Route
}

// buildProviders constructs one inference implementation per configured
// provider and registers every route with its owner. The returned registry is
// the single gateway the rest of the composition depends on, so adding a
// provider changes only this function and the configuration (hexagonal rule:
// shell behavior and the SSH adapter stay unaware of providers).
func buildProviders(cfg *config.Config, snapshot *config.Snapshot) (*provider.Registry, error) {
	registry := provider.NewRegistry(system.NewClock())
	for _, p := range cfg.Providers {
		routes := routesOfProvider(cfg, p.Name)
		if len(routes) == 0 {
			continue
		}
		switch p.Kind {
		case providerKindCLI:
			adapter := buildCLIProvider(p, routes)
			for _, route := range routes {
				registry.Register(route.ID, adapter)
			}
		case "", providerKindHTTP:
			gw := buildHTTPProvider(cfg, snapshot, routes)
			for _, route := range routes {
				registry.Register(route.ID, gw)
			}
		default:
			// Validation rejects unknown kinds at load; reaching here means the
			// schema and this composition disagree, so fail loudly.
			return nil, fmt.Errorf("provider %q has unknown kind %q", p.Name, p.Kind)
		}
	}
	return registry, nil
}

// routesOfProvider returns the parsed routes whose configured provider is name.
// Ids that fail to parse are skipped; validation rejects them at load.
func routesOfProvider(cfg *config.Config, name string) []ownedRoute {
	var routes []ownedRoute
	for _, r := range cfg.Routes {
		if r.Provider != name {
			continue
		}
		id, err := domain.ParseRouteID(r.ID)
		if err != nil {
			continue
		}
		routes = append(routes, ownedRoute{ID: id, Config: r})
	}
	return routes
}

// allRoutes returns every configured route, in configuration order.
func allRoutes(cfg *config.Config) []ownedRoute {
	var routes []ownedRoute
	for _, r := range cfg.Routes {
		id, err := domain.ParseRouteID(r.ID)
		if err != nil {
			continue
		}
		routes = append(routes, ownedRoute{ID: id, Config: r})
	}
	return routes
}

// buildHTTPProvider assembles the direct-HTTP gateway for the given routes. It
// carries every account, because the router selects the account per request and
// the gateway resolves only the one a request names. Accounts with no secret
// reference (the credential-less accounts of cli providers) are skipped.
func buildHTTPProvider(cfg *config.Config, snapshot *config.Snapshot, routes []ownedRoute) *opencode.Gateway {
	gw := &opencode.Gateway{
		Clock:    system.NewClock(),
		Routes:   map[domain.RouteID]opencode.RouteConfig{},
		Accounts: map[domain.AccountID]opencode.AccountConfig{},
		Secrets:  gatewaySecrets{snapshot: snapshot, cfg: cfg},
		// PLAN 8.1: the Go product route requires a stable x-opencode-session
		// header. The value is an opaque service-instance identity, never a
		// credential; per-conversation granularity would require threading the
		// application session into the gateway.
		SessionID: func() string { return "vibeshell-service" },
	}
	for _, route := range routes {
		product := productName(cfg, route.Config.Provider, route.Config.Product)
		protocol := opencode.Protocol(route.Config.Protocol)
		if protocol == "" {
			protocol = opencode.Protocol(defaultProtocol(cfg, route.Config.Provider, route.Config.Product))
		}
		gw.Routes[route.ID] = opencode.RouteConfig{
			Product:  opencode.Product(product),
			Protocol: protocol,
			Model:    route.Config.Model,
			BaseURL:  productBaseURL(cfg, route.Config.Provider, route.Config.Product),
		}
	}
	for _, account := range cfg.Accounts {
		if account.SecretRef == "" {
			// A credential-less account is served by a cli provider; the HTTP
			// gateway has no secret to resolve for it, so it is not registered.
			continue
		}
		accountID, err := domain.ParseAccountID(account.ID)
		if err != nil {
			continue
		}
		ref := accountKeyRef(account)
		if ref.IsZero() {
			continue
		}
		gw.Accounts[accountID] = opencode.AccountConfig{KeyRefs: []domain.KeyRef{ref}}
	}
	return gw
}

// buildCLIProvider assembles the CLI-backed provider for one provider's routes.
// The CLI owns its own credentials, so no account secret is resolved here.
func buildCLIProvider(p config.Provider, routes []ownedRoute) *opencodecli.Adapter {
	models := make(map[domain.RouteID]string, len(routes))
	for _, route := range routes {
		models[route.ID] = route.Config.Model
	}
	return &opencodecli.Adapter{
		Binary:     cliBinary(p),
		ProviderID: p.Name,
		Models:     models,
		Clock:      system.NewClock(),
	}
}

// cliBinary returns the CLI executable a cli provider names. A provider has one
// binary; the first product's base_url carries it, and an empty value selects
// the default from PATH.
func cliBinary(p config.Provider) string {
	for _, product := range p.Products {
		if product.BaseURL != "" {
			return product.BaseURL
		}
	}
	return defaultCLIBinary
}
