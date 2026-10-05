package config

import (
	"fmt"
	"strings"
)

// RouteInfo describes one model route in the example
// catalogue.
//
// The catalogue is fixture data for this qualification spike,
// mirroring how PLAN 11 expects validation to work: route
// references are checked against a known catalogue without
// making a paid inference call. Real availability is verified
// against the live catalogue at implementation time, and no
// entry here claims permanent availability of any model.
type RouteInfo struct {
	ID       string
	Product  string
	Protocol string
	Pricing  string // "free" or "paid"
	Cheap    bool   // pay-as-you-go and cheap enough for a paid tier
}

// Catalogue is an ordered set of known model routes. Order is
// part of the contract: discovery expansion preserves it, so
// tier route lists are deterministic.
type Catalogue struct {
	routes []RouteInfo
	byID   map[string]RouteInfo
}

// NewCatalogue builds a catalogue preserving the given order.
// Duplicate IDs are rejected because two routes claiming one
// ID would make tier expansion ambiguous.
func NewCatalogue(routes []RouteInfo) (Catalogue, error) {
	cat := Catalogue{routes: routes, byID: make(map[string]RouteInfo, len(routes))}
	for _, route := range routes {
		if _, exists := cat.byID[route.ID]; exists {
			return Catalogue{}, fmt.Errorf("catalogue lists route %q twice", route.ID)
		}
		cat.byID[route.ID] = route
	}
	return cat, nil
}

// MustCatalogue builds a catalogue for fixtures; it panics on
// duplicate IDs because a broken fixture must fail loudly.
func MustCatalogue(routes []RouteInfo) Catalogue {
	cat, err := NewCatalogue(routes)
	if err != nil {
		panic(err)
	}
	return cat
}

// Route looks up one route ID.
func (c Catalogue) Route(id string) (RouteInfo, bool) {
	info, ok := c.byID[id]
	return info, ok
}

// Discover expands a suitable-free pattern for one product:
// every catalogue route of that product whose ID ends in
// "-free", in catalogue order.
func (c Catalogue) Discover(product string) []RouteInfo {
	var matches []RouteInfo
	for _, route := range c.routes {
		if route.Product == product && strings.HasSuffix(route.ID, "-free") {
			matches = append(matches, route)
		}
	}
	return matches
}

// ExampleCatalogue is the fixture catalogue used by the shipped
// example and the tests. Products, protocols, and pricing
// classes are examples only.
func ExampleCatalogue() Catalogue {
	return MustCatalogue([]RouteInfo{
		{ID: "opencode/ling-3.1-flash-free", Product: "opencode", Protocol: "opencode", Pricing: "free"},
		{ID: "opencode/deepseek-v4.1-flash-free", Product: "opencode", Protocol: "opencode", Pricing: "free"},
		{ID: "opencode/qwen3.6-flash-free", Product: "opencode", Protocol: "opencode", Pricing: "free"},
		{ID: "opencode-go/ling-3.1-flash", Product: "opencode-go", Protocol: "opencode-go", Pricing: "paid"},
		{ID: "opencode-go/deepseek-v4.1-flash", Product: "opencode-go", Protocol: "opencode-go", Pricing: "paid"},
		{ID: "zen/gpt-5.1-nano", Product: "zen", Protocol: "zen", Pricing: "paid", Cheap: true},
		{ID: "zen/minimax-m3-turbo", Product: "zen", Protocol: "zen", Pricing: "paid", Cheap: true},
	})
}

// IsDiscoveryPattern reports whether a route reference is a
// suitable-free discovery pattern of the form "<product>/-free".
func IsDiscoveryPattern(route string) bool {
	return len(route) > len("/-free") && strings.Count(route, "/") == 1 && strings.HasSuffix(route, "/-free")
}

// discoveryProduct extracts the product part of a discovery
// pattern, for example "opencode" from "opencode/-free".
func discoveryProduct(pattern string) string {
	return strings.TrimSuffix(pattern, "/-free")
}

// ExpandTierRoutes returns the route IDs a tier resolves to, in
// tier order: exact IDs pass through and discovery patterns
// expand to the matching free routes in catalogue order.
func ExpandTierRoutes(tier TierConfig, cat Catalogue) []string {
	var routes []string
	for _, route := range tier.Routes {
		if IsDiscoveryPattern(route) {
			for _, match := range cat.Discover(discoveryProduct(route)) {
				routes = append(routes, match.ID)
			}
			continue
		}
		routes = append(routes, route)
	}
	return routes
}
