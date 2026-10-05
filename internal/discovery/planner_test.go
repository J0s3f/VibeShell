package discovery

import (
	"testing"

	"j0s.at/vibeshell/internal/adapters/opencode"
)

type fakeCatalogue struct {
	models []Model
}

func (f *fakeCatalogue) Models() []Model { return f.models }

// eligibleModel returns a console model that satisfies every suitability
// gate: metadata present, text in/out, tool support, a 200k context, and
// known zero cost. ProviderNPM is empty, so protocol classification
// resolves to chat.
func eligibleModel(id string, context int) Model {
	return Model{
		Product:    "console",
		ID:         id,
		Found:      true,
		ToolCall:   true,
		TextInput:  true,
		TextOutput: true,
		Context:    context,
		CostKnown:  true,
		ZeroCost:   true,
	}
}

func TestExpandSuitableFreeModel(t *testing.T) {
	p := &Planner{
		Catalogue:    &fakeCatalogue{models: []Model{eligibleModel("glow-free", 200000)}},
		Requirements: Requirements{MinContext: 100000},
	}
	cands, err := p.Expand("free-discovery", nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Product != "console" || c.Model != "glow-free" || c.Protocol != "chat" {
		t.Fatalf("unexpected candidate identity: %+v", c)
	}
	if c.Reason != opencode.ReasonEligibleAutoFree {
		t.Fatalf("reason = %q, want %q", c.Reason, opencode.ReasonEligibleAutoFree)
	}
	if want := RouteKey("console", "glow-free", "chat"); c.Route != want {
		t.Fatalf("route key = %q, want %q", c.Route, want)
	}
}

func TestDecideExcludesModelsFailingRequirements(t *testing.T) {
	notool := eligibleModel("notool-free", 200000)
	notool.ToolCall = false
	p := &Planner{
		Catalogue: &fakeCatalogue{models: []Model{
			eligibleModel("tiny-free", 8000),
			notool,
		}},
		Requirements: Requirements{MinContext: 100000},
	}
	decisions := p.Decide(p.Requirements)
	if len(decisions) != 2 {
		t.Fatalf("got %d decisions, want 2", len(decisions))
	}
	wantReasons := map[string]string{
		"tiny-free":   opencode.ReasonContextTooSmall,
		"notool-free": opencode.ReasonNoToolSupport,
	}
	for _, d := range decisions {
		if d.Eligible {
			t.Fatalf("%s unexpectedly eligible: %+v", d.ID, d)
		}
		if d.Reason != wantReasons[d.ID] {
			t.Fatalf("%s: reason = %q, want %q", d.ID, d.Reason, wantReasons[d.ID])
		}
	}
	cands, err := p.Expand("free-discovery", nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0: %+v", len(cands), cands)
	}
}

func TestExpandHonorsExplicitAllowDeny(t *testing.T) {
	p := &Planner{
		Catalogue: &fakeCatalogue{models: []Model{
			eligibleModel("big-pickle", 200000),
			eligibleModel("glow-free", 200000),
		}},
		Requirements: Requirements{
			ExplicitAllow: map[string]bool{"big-pickle": true},
			ExplicitDeny:  map[string]bool{"glow-free": true},
		},
	}
	cands, err := p.Expand("free-discovery", nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	if cands[0].Model != "big-pickle" {
		t.Fatalf("model = %q, want big-pickle", cands[0].Model)
	}
	if cands[0].Reason != opencode.ReasonEligibleExplicit {
		t.Fatalf("reason = %q, want %q", cands[0].Reason, opencode.ReasonEligibleExplicit)
	}
}

func TestExpandEmptyCatalogue(t *testing.T) {
	p := &Planner{
		Catalogue:    &fakeCatalogue{},
		Requirements: Requirements{MinContext: 100000},
	}
	cands, err := p.Expand("free-discovery", nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0: %+v", len(cands), cands)
	}
}

func TestExpandDeterministicAndCapped(t *testing.T) {
	p := &Planner{
		Catalogue: &fakeCatalogue{models: []Model{
			eligibleModel("delta-free", 200000),
			eligibleModel("alpha-free", 200000),
			eligibleModel("charlie-free", 200000),
			eligibleModel("bravo-free", 200000),
		}},
		Requirements: Requirements{},
		MaxRoutes:    2,
	}
	first, err := p.Expand("free-discovery", nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("got %d candidates, want 2 (cap): %+v", len(first), first)
	}
	wantOrder := []string{
		RouteKey("console", "alpha-free", "chat"),
		RouteKey("console", "bravo-free", "chat"),
	}
	for i, c := range first {
		if c.Route != wantOrder[i] {
			t.Fatalf("candidate %d route = %q, want %q", i, c.Route, wantOrder[i])
		}
	}
	second, err := p.Expand("free-discovery", nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("repeat expansion length = %d, want %d", len(second), len(first))
	}
	for i := range first {
		if second[i] != first[i] {
			t.Fatalf("repeat expansion differs at %d: %+v vs %+v", i, second[i], first[i])
		}
	}
}

func TestExpandSkipsExplicitRouteKeys(t *testing.T) {
	p := &Planner{
		Catalogue: &fakeCatalogue{models: []Model{
			eligibleModel("glow-free", 200000),
			eligibleModel("other-free", 200000),
		}},
		Requirements: Requirements{},
	}
	explicit := []string{RouteKey("console", "glow-free", "chat")}
	cands, err := p.Expand("free-discovery", explicit)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	if cands[0].Model != "other-free" {
		t.Fatalf("model = %q, want other-free", cands[0].Model)
	}
}

func TestExpandNilCatalogue(t *testing.T) {
	p := &Planner{Requirements: Requirements{}}
	if _, err := p.Expand("free-discovery", nil); err == nil {
		t.Fatal("Expand with nil catalogue: got nil error, want error")
	}
}
