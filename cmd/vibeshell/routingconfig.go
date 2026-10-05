package main

import (
	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/discovery"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/routing"
)

// buildRoutingConfig reduces the configuration snapshot to the router's
// static view: the ordered tiers, the route specs, the accounts, and the
// account pools. It performs no I/O and no catalogue lookup; automatic-free
// expansion remains the discovery task's responsibility and is not wired in
// this build (auto_free tiers therefore contribute only their explicit
// routes).
func buildRoutingConfig(cfg *config.Config, planner *discovery.Planner) routing.Config {
	out := routing.Config{
		Policy: domain.RoutePolicy{
			AccountPools:   map[string][]domain.AccountID{},
			MaxAttempts:    defaultMaxAttempts,
			TurnDeadlineMs: defaultTurnDeadlineMs,
		},
		Routes:   []routing.RouteSpec{},
		Accounts: []domain.Account{},
	}

	var discovered []routing.RouteSpec
	for _, tier := range cfg.Tiers {
		tierConfig := domain.TierConfig{
			Name:        tier.Name,
			AutoFree:    tier.AutoFree,
			AccountPool: tier.AccountPool,
			Enabled:     true,
		}
		for _, rid := range tier.Routes {
			id, err := domain.ParseRouteID(rid)
			if err != nil {
				continue
			}
			tierConfig.RouteIDs = append(tierConfig.RouteIDs, id)
		}
		if tier.AutoFree && planner != nil {
			candidates, err := planner.Expand(tier.Name, explicitRouteKeys(cfg, tier.Routes))
			if err == nil {
				for _, candidate := range candidates {
					id, merr := mintRouteID(candidate.Route)
					if merr != nil {
						continue
					}
					tierConfig.RouteIDs = append(tierConfig.RouteIDs, id)
					discovered = append(discovered, routing.RouteSpec{ID: id, Product: candidate.Product, Paid: false})
				}
			}
		}
		out.Policy.Tiers = append(out.Policy.Tiers, tierConfig)
	}

	for _, route := range cfg.Routes {
		id, err := domain.ParseRouteID(route.ID)
		if err != nil {
			continue
		}
		// A route is treated as paid only when its product is not declared
		// free-like; without pricing metadata the router labels unknown cost
		// explicitly by not reserving (PLAN 9.3), which is the safe default.
		out.Routes = append(out.Routes, routing.RouteSpec{
			ID:       id,
			Product:  route.Product,
			Paid:     false,
			Purposes: route.Purposes,
		})
	}

	out.Routes = append(out.Routes, discovered...)

	for _, account := range cfg.Accounts {
		id, err := domain.ParseAccountID(account.ID)
		if err != nil {
			continue
		}
		ref := accountKeyRef(account)
		acc := domain.Account{
			ID:                id,
			QuotaGroup:        account.QuotaGroup,
			PermittedProducts: account.PermittedProducts,
			Enabled:           true,
		}
		if !ref.IsZero() {
			acc.KeyRefs = []domain.KeyRef{ref}
		}
		out.Accounts = append(out.Accounts, acc)
	}

	for name, ids := range cfg.AccountPools {
		pool := make([]domain.AccountID, 0, len(ids))
		for _, raw := range ids {
			id, err := domain.ParseAccountID(raw)
			if err != nil {
				continue
			}
			pool = append(pool, id)
		}
		out.Policy.AccountPools[name] = pool
	}

	if cfg.Limits != nil {
		if cfg.Limits.MaxAttempts > 0 {
			out.Policy.MaxAttempts = cfg.Limits.MaxAttempts
		}
		if cfg.Limits.TurnDeadlineMs > 0 {
			out.Policy.TurnDeadlineMs = int64(cfg.Limits.TurnDeadlineMs)
		}
	}
	if cfg.Health != nil {
		out.Policy.ProbeBudget = domain.ProbeBudget{
			MaxConcurrentProbes: cfg.Health.MaxConcurrentProbes,
			ProbeIntervalMs:     cfg.Health.ProbeIntervalMs,
			MaxProbesPerHour:    cfg.Health.MaxProbesPerHour,
		}
	}
	return out
}

// Default routing bounds used when the configuration leaves them unset.
const (
	defaultMaxAttempts    = 3
	defaultTurnDeadlineMs = int64(120000)
)

// productName returns the configured product name for a provider/product
// pair, falling back to the product name itself.
func productName(cfg *config.Config, provider, product string) string {
	for _, p := range cfg.Providers {
		if p.Name != provider {
			continue
		}
		for _, prod := range p.Products {
			if prod.Name == product {
				return prod.Name
			}
		}
	}
	return product
}

// productBaseURL returns the configured base URL for a provider/product pair.
func productBaseURL(cfg *config.Config, provider, product string) string {
	for _, p := range cfg.Providers {
		if p.Name != provider {
			continue
		}
		for _, prod := range p.Products {
			if prod.Name == product {
				return prod.BaseURL
			}
		}
	}
	return ""
}

// defaultProtocol returns the configured default protocol for a
// provider/product pair, or the product's only protocol when it declares one.
func defaultProtocol(cfg *config.Config, provider, product string) config.Protocol {
	for _, p := range cfg.Providers {
		if p.Name != provider {
			continue
		}
		for _, prod := range p.Products {
			if prod.Name != product {
				continue
			}
			if prod.DefaultProtocol != "" {
				return prod.DefaultProtocol
			}
			if len(prod.Protocols) == 1 {
				return prod.Protocols[0]
			}
		}
	}
	return ""
}
