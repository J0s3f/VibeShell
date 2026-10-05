package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/opencode"
	"j0s.at/vibeshell/internal/discovery"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/observability"
	"j0s.at/vibeshell/internal/ports"
)

// discoveryRefreshTimeout bounds the startup catalogue and metadata refresh.
// Discovery is advisory: a timeout leaves expansion empty rather than delaying
// startup.
const discoveryRefreshTimeout = 10 * time.Second

// discoveryMaxBody bounds one fetched catalogue or metadata body.
const discoveryMaxBody = 8 << 20

// buildDiscovery builds the suitable-free planner when a tier asks for
// automatic-free expansion and the discovery group names a metadata URL.
// Otherwise it returns nil, and auto_free tiers contribute nothing — the
// behavior before discovery was wired. The catalogue and metadata are refreshed
// once, best effort: a network failure leaves expansion empty instead of
// inventing models.
func buildDiscovery(ctx context.Context, cfg *config.Config, clock ports.Clock, logger *observability.Logger) *discovery.Planner {
	if cfg.Discovery == nil || cfg.Discovery.MetadataURL == "" || !hasAutoFreeTier(cfg) {
		return nil
	}
	cache := &opencode.Cache{MaxAgeMs: cfg.Discovery.StaleAgeMs}
	refreshCtx, cancel := context.WithTimeout(ctx, discoveryRefreshTimeout)
	defer cancel()
	refreshDiscovery(refreshCtx, cfg, cache, logger)
	return &discovery.Planner{
		Catalogue: discovery.NewCacheCatalogue(cache, clock),
		Requirements: discovery.Requirements{
			MinContext:        cfg.Discovery.MinContextTokens,
			Capabilities:      cfg.Discovery.Capabilities,
			ExplicitAllow:     stringSet(cfg.Discovery.ExplicitAllow),
			ProtocolOverrides: protocolOverrideMap(cfg.Discovery.ProtocolOverrides),
		},
	}
}

// hasAutoFreeTier reports whether any tier asks for automatic-free expansion.
func hasAutoFreeTier(cfg *config.Config) bool {
	for _, tier := range cfg.Tiers {
		if tier.AutoFree {
			return true
		}
	}
	return false
}

// refreshDiscovery loads models.dev metadata and each configured product's
// public catalogue. Every failure is logged and ignored: without a fresh
// snapshot the planner expands nothing.
func refreshDiscovery(ctx context.Context, cfg *config.Config, cache *opencode.Cache, logger *observability.Logger) {
	if body, err := fetchDiscoveryBody(ctx, cfg.Discovery.MetadataURL); err != nil {
		logger.Warn("discovery metadata refresh failed",
			attr(observability.FieldOperation, "discovery"),
			attr(observability.FieldError, err.Error()))
	} else if err := cache.StoreMetadata(body, cfg.Discovery.MetadataURL); err != nil {
		logger.Warn("discovery metadata rejected",
			attr(observability.FieldOperation, "discovery"),
			attr(observability.FieldError, err.Error()))
	}
	for _, provider := range cfg.Providers {
		for _, product := range provider.Products {
			if !strings.HasPrefix(product.BaseURL, "http") {
				continue
			}
			url := strings.TrimRight(product.BaseURL, "/") + "/models"
			if _, err := cache.RefreshCatalog(ctx, opencode.Product(product.Name), url); err != nil {
				logger.Warn("discovery catalogue refresh failed for "+product.Name,
					attr(observability.FieldOperation, "discovery"),
					attr(observability.FieldError, err.Error()))
			}
		}
	}
}

// fetchDiscoveryBody performs one bounded GET for discovery input.
func fetchDiscoveryBody(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, discoveryMaxBody))
}

// mintRouteID derives a stable domain route identity from a discovered route
// key, so the same catalogue entry always maps to the same route ID across
// restarts.
func mintRouteID(key string) (domain.RouteID, error) {
	sum := sha256.Sum256([]byte("route:" + key))
	var encoded []byte
	var acc uint16
	var bits uint
	for _, b := range sum[:16] {
		acc = acc<<8 | uint16(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			encoded = append(encoded, crockfordAlphabet[(acc>>bits)&0x1f])
		}
	}
	if bits > 0 {
		encoded = append(encoded, crockfordAlphabet[(acc<<(5-bits))&0x1f])
	}
	return domain.ParseRouteID(domain.PrefixRoute + "_" + string(encoded))
}

// explicitRouteKeys returns the route keys a tier already names, so discovery
// does not duplicate an explicitly configured product/model/protocol triple.
func explicitRouteKeys(cfg *config.Config, routeIDs []string) []string {
	keys := make([]string, 0, len(routeIDs))
	for _, rid := range routeIDs {
		for _, route := range cfg.Routes {
			if route.ID != rid {
				continue
			}
			protocol := route.Protocol
			if protocol == "" {
				protocol = defaultProtocol(cfg, route.Provider, route.Product)
			}
			keys = append(keys, discovery.RouteKey(route.Product, route.Model, string(protocol)))
		}
	}
	return keys
}

func stringSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func protocolOverrideMap(overrides map[string]config.Protocol) map[string]string {
	if len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(overrides))
	for model, protocol := range overrides {
		out[model] = string(protocol)
	}
	return out
}
