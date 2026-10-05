package main

import (
	"testing"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/discovery"
)

// fakeCatalogue is a discovery.Catalogue over a fixed model list.
type fakeCatalogue struct{ models []discovery.Model }

func (f fakeCatalogue) Models() []discovery.Model { return f.models }

// TestBuildRoutingConfigExpandsAutoFreeTier proves the composition actually
// feeds an auto_free tier through the discovery planner and mints a route for
// each suitable-free model, rather than copying auto_free and ignoring it.
func TestBuildRoutingConfigExpandsAutoFreeTier(t *testing.T) {
	planner := &discovery.Planner{
		Catalogue: fakeCatalogue{models: []discovery.Model{{
			Product: "go", ID: "space-bunny-free", Found: true,
			ToolCall: true, TextInput: true, TextOutput: true,
			Context: 200000, CostKnown: true, ZeroCost: true,
			ProviderNPM: "@opencode/go",
		}}},
	}
	cfg := &config.Config{Tiers: []config.Tier{{Name: "free", AutoFree: true}}}

	out := buildRoutingConfig(cfg, planner)
	if len(out.Policy.Tiers) != 1 || len(out.Policy.Tiers[0].RouteIDs) != 1 {
		t.Fatalf("auto_free tier route ids = %+v, want exactly one discovered route", out.Policy.Tiers)
	}
	if len(out.Routes) != 1 || out.Routes[0].Product != "go" {
		t.Fatalf("discovered routes = %+v, want one go route", out.Routes)
	}
	if out.Policy.Tiers[0].RouteIDs[0] != out.Routes[0].ID {
		t.Fatalf("the tier route id %v does not match the discovered spec %v", out.Policy.Tiers[0].RouteIDs[0], out.Routes[0].ID)
	}
}

// TestBuildRoutingConfigWithoutPlannerKeepsExplicitRoutes proves a nil planner
// (no auto_free configuration) leaves the tier's explicit routes unchanged.
func TestBuildRoutingConfigWithoutPlannerKeepsExplicitRoutes(t *testing.T) {
	cfg := &config.Config{Tiers: []config.Tier{{Name: "free", AutoFree: true}}}
	out := buildRoutingConfig(cfg, nil)
	if len(out.Policy.Tiers) != 1 || len(out.Policy.Tiers[0].RouteIDs) != 0 {
		t.Fatalf("tier route ids = %+v, want none without a planner", out.Policy.Tiers)
	}
}
