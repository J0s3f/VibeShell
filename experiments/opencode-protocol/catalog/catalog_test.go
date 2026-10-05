package catalog

import (
	"os"
	"testing"

	"j0s.at/vibeshell/experiments/opencode-protocol"
)

func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCatalogueObservationShape(t *testing.T) {
	console, err := ParseCatalogue(loadTestdata(t, "opencode-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if console.Object != "list" {
		t.Fatalf("object = %q", console.Object)
	}
	if console.Count != 82 {
		t.Fatalf("console count = %d, want 82", console.Count)
	}
	if got := console.Fields; len(got) != 4 || got[0] != "created" {
		t.Fatalf("fields = %v", got)
	}

	goCat, err := ParseCatalogue(loadTestdata(t, "opencode-go-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if goCat.Count != 43 {
		t.Fatalf("go count = %d, want 43", goCat.Count)
	}

	// Hashes pin the exact observed bytes for receipts.
	t.Logf("console sha256=%s", SHA256Hex(loadTestdata(t, "opencode-models.json")))
	t.Logf("go sha256=%s", SHA256Hex(loadTestdata(t, "opencode-go-models.json")))
}

func testOptions() Options {
	return Options{
		MinContext:        100000,
		ExplicitAllow:     map[string]bool{"big-pickle": true},
		StructuredOnly:    map[string]bool{"jev-1.13-free": true},
		ExplicitDeny:      map[string]bool{},
		ProtocolOverrides: map[string]gateway.Protocol{},
	}
}

func loadMetas(t *testing.T) map[gateway.Product]map[string]Meta {
	t.Helper()
	metas, err := LoadMetadata(loadTestdata(t, "models-dev-opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(metas[gateway.ProductConsole]) != 116 {
		t.Fatalf("console metadata = %d, want 116", len(metas[gateway.ProductConsole]))
	}
	if len(metas[gateway.ProductGo]) != 33 {
		t.Fatalf("go metadata = %d, want 33", len(metas[gateway.ProductGo]))
	}
	return metas
}

func TestCatalogueMetadataJoinCounts(t *testing.T) {
	metas := loadMetas(t)
	console, err := ParseCatalogue(loadTestdata(t, "opencode-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	matched, missing := 0, []string{}
	for _, id := range console.IDs {
		if _, ok := metas[gateway.ProductConsole][id]; ok {
			matched++
		} else {
			missing = append(missing, id)
		}
	}
	t.Logf("console catalogue=%d metadata=%d matched=%d missing=%v",
		console.Count, len(metas[gateway.ProductConsole]), matched, missing)
	// jev models are catalogue-observed but metadata-unknown: they must be
	// explicit operator decisions, never silently zero-cost.
	found := false
	for _, m := range missing {
		if m == "jev-1.13-free" || m == "jev-1.13" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected jev IDs among metadata-missing, got %v", missing)
	}
}

func TestSuitabilityCases(t *testing.T) {
	metas := loadMetas(t)
	console := metas[gateway.ProductConsole]
	opts := testOptions()

	t.Run("big pickle explicit exception", func(t *testing.T) {
		d := SuitableFree(gateway.ProductConsole, "big-pickle", console, opts)
		if !d.Eligible || d.Reason != ReasonEligibleExplicit {
			t.Fatalf("%+v", d)
		}
		withoutAllow := opts
		withoutAllow.ExplicitAllow = map[string]bool{}
		d2 := SuitableFree(gateway.ProductConsole, "big-pickle", console, withoutAllow)
		if d2.Eligible {
			t.Fatalf("big-pickle eligible without explicit allow: %+v", d2)
		}
	})

	t.Run("jev free structured decision excluded", func(t *testing.T) {
		// jev-1.13-free ends in -free but is a structured-decision model,
		// not a shell text generator.
		d := SuitableFree(gateway.ProductConsole, "jev-1.13-free", console, opts)
		if d.Eligible || d.Reason != ReasonStructuredOnly {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("unknown metadata needs operator", func(t *testing.T) {
		d := SuitableFree(gateway.ProductConsole, "jev-1.13", map[string]Meta{}, opts)
		if d.Eligible || d.Reason != ReasonUnknownMetadata {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("known paid free-suffix excluded", func(t *testing.T) {
		paid := map[string]Meta{
			"flaky-free": {ID: "flaky-free", ToolCall: true, TextInput: true, TextOutput: true, Context: 200000, CostKnown: true, ZeroCost: false},
		}
		d := SuitableFree(gateway.ProductConsole, "flaky-free", paid, opts)
		if d.Eligible || d.Reason != ReasonKnownPaid {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("unknown cost is explicit not zero", func(t *testing.T) {
		unknown := map[string]Meta{
			"mystery-free": {ID: "mystery-free", ToolCall: true, TextInput: true, TextOutput: true, Context: 200000},
		}
		d := SuitableFree(gateway.ProductConsole, "mystery-free", unknown, opts)
		if d.Eligible || d.Reason != ReasonUnknownCost {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("tool support required", func(t *testing.T) {
		noTools := map[string]Meta{
			"plain-free": {ID: "plain-free", TextInput: true, TextOutput: true, Context: 200000, CostKnown: true, ZeroCost: true},
		}
		d := SuitableFree(gateway.ProductConsole, "plain-free", noTools, opts)
		if d.Eligible || d.Reason != ReasonNoToolSupport {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("minimum context enforced", func(t *testing.T) {
		small := map[string]Meta{
			"tiny-free": {ID: "tiny-free", ToolCall: true, TextInput: true, TextOutput: true, Context: 32000, CostKnown: true, ZeroCost: true},
		}
		d := SuitableFree(gateway.ProductConsole, "tiny-free", small, opts)
		if d.Eligible || d.Reason != ReasonContextTooSmall {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("non-free suffix needs explicit allow", func(t *testing.T) {
		d := SuitableFree(gateway.ProductConsole, "gpt-5", console, opts)
		if d.Eligible || d.Reason != ReasonNotFreeSuffix {
			t.Fatalf("%+v", d)
		}
	})
}

func TestProtocolClassification(t *testing.T) {
	metas := loadMetas(t)
	console := metas[gateway.ProductConsole]
	cases := []struct {
		id   string
		want gateway.Protocol
	}{
		{"claude-sonnet-4-5", gateway.ProtocolMessages},
		{"gemini-3-flash", gateway.ProtocolGemini},
		{"gpt-5", gateway.ProtocolChat},
		{"big-pickle", gateway.ProtocolChat},
	}
	for _, c := range cases {
		meta, ok := console[c.id]
		if !ok {
			t.Fatalf("metadata missing for %s", c.id)
		}
		if got := ProtocolFor(meta, ok, nil); got != c.want {
			t.Fatalf("%s: protocol = %s, want %s", c.id, got, c.want)
		}
	}
	if got := ProtocolFor(Meta{}, false, nil); got != "" {
		t.Fatalf("unknown model protocol = %q, want empty (fail closed)", got)
	}
}

func TestEligibleNonChatNeverHitsChatEndpoint(t *testing.T) {
	metas := loadMetas(t)
	opts := testOptions()
	eligible, _ := ExpandFree(gateway.ProductConsole,
		mustShape(t, "opencode-models.json").IDs, metas[gateway.ProductConsole], opts)
	if len(eligible) == 0 {
		t.Fatal("no eligible models; discovery found nothing")
	}
	chatURL, _ := gateway.EndpointFor(gateway.ProductConsole, gateway.ProtocolChat)
	nonChat := 0
	for _, d := range eligible {
		u, err := EndpointForDecision(d)
		if err != nil {
			t.Fatal(err)
		}
		if d.Protocol != gateway.ProtocolChat {
			nonChat++
			if u == chatURL {
				t.Fatalf("%s: non-chat protocol resolves to chat URL", d.ID)
			}
		}
	}
	t.Logf("eligible=%d non-chat=%d", len(eligible), nonChat)
	// A synthetic non-chat eligible model proves the separation property
	// directly: its endpoint must differ from the chat URL.
	synth := map[string]Meta{
		"synth-claude-free": {ID: "synth-claude-free", ToolCall: true, TextInput: true, TextOutput: true, Context: 200000, CostKnown: true, ZeroCost: true, ProviderNPM: "@ai-sdk/anthropic"},
		"synth-gemini-free": {ID: "synth-gemini-free", ToolCall: true, TextInput: true, TextOutput: true, Context: 200000, CostKnown: true, ZeroCost: true, ProviderNPM: "@ai-sdk/google"},
	}
	for id, want := range map[string]gateway.Protocol{"synth-claude-free": gateway.ProtocolMessages, "synth-gemini-free": gateway.ProtocolGemini} {
		d := SuitableFree(gateway.ProductConsole, id, synth, opts)
		if !d.Eligible || d.Protocol != want {
			t.Fatalf("%s: %+v", id, d)
		}
		u, err := EndpointForDecision(d)
		if err != nil {
			t.Fatal(err)
		}
		if u == chatURL {
			t.Fatalf("%s: non-chat protocol resolves to chat URL", id)
		}
		t.Logf("%s -> %s", id, u)
	}
	// Ineligible decisions never resolve to an endpoint.
	bad := SuitableFree(gateway.ProductConsole, "jev-1.13-free", metas[gateway.ProductConsole], opts)
	if _, err := EndpointForDecision(bad); err == nil {
		t.Fatal("expected endpoint refusal for ineligible model")
	}
}

func mustShape(t *testing.T, name string) Shape {
	t.Helper()
	shape, err := ParseCatalogue(loadTestdata(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return shape
}
