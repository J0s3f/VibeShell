package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"j0s.at/vibeshell/internal/adapters/opencode"
)

type fakeClock struct{ now int64 }

func (f fakeClock) NowUnixMilli() int64   { return f.now }
func (f fakeClock) MonotonicNanos() int64 { return 0 }

const testCatalogueBody = `{"object":"list","data":[{"id":"glow-free","object":"model"},{"id":"ghost-free","object":"model"}]}`

const testMetadataBody = `{"opencode":{"models":{"glow-free":{"id":"glow-free","tool_call":true,"modalities":{"input":["text"],"output":["text"]},"limit":{"context":200000},"cost":{"input":0,"output":0},"provider":{"npm":"@ai-sdk/openai"}}}}}`

// refreshCatalogueCache returns a cache holding one fresh console snapshot
// with the given body, served by a local test server.
func refreshCatalogueCache(t *testing.T, body string) *opencode.Cache {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cache := &opencode.Cache{}
	if _, err := cache.RefreshCatalog(context.Background(), opencode.ProductConsole, srv.URL); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}
	return cache
}

func TestCacheCatalogueModels(t *testing.T) {
	cache := refreshCatalogueCache(t, testCatalogueBody)
	if err := cache.StoreMetadata([]byte(testMetadataBody), "test"); err != nil {
		t.Fatalf("StoreMetadata: %v", err)
	}
	models := NewCacheCatalogue(cache, fakeClock{now: 1}).Models()
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	glow, ok := byID["glow-free"]
	if !ok {
		t.Fatalf("glow-free missing: %+v", models)
	}
	if glow.Product != "console" {
		t.Fatalf("glow-free product = %q, want console", glow.Product)
	}
	if !glow.Found || !glow.ToolCall || !glow.TextInput || !glow.TextOutput {
		t.Fatalf("glow-free capability facts not mapped: %+v", glow)
	}
	if glow.Context != 200000 || !glow.CostKnown || !glow.ZeroCost {
		t.Fatalf("glow-free metadata not mapped: %+v", glow)
	}
	if glow.ProviderNPM != "@ai-sdk/openai" {
		t.Fatalf("glow-free ProviderNPM = %q", glow.ProviderNPM)
	}
	ghost := byID["ghost-free"]
	if ghost.Found {
		t.Fatalf("ghost-free = %+v, want Found=false", ghost)
	}
}

func TestCacheCatalogueWithoutMetadata(t *testing.T) {
	cache := refreshCatalogueCache(t, testCatalogueBody)
	models := NewCacheCatalogue(cache, fakeClock{now: 1}).Models()
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	for _, m := range models {
		if m.Found {
			t.Fatalf("%s: Found=true without metadata", m.ID)
		}
	}
}

func TestCacheCatalogueWithoutSnapshot(t *testing.T) {
	cache := &opencode.Cache{}
	if err := cache.StoreMetadata([]byte(testMetadataBody), "test"); err != nil {
		t.Fatalf("StoreMetadata: %v", err)
	}
	if models := NewCacheCatalogue(cache, fakeClock{now: 1}).Models(); len(models) != 0 {
		t.Fatalf("got %d models, want 0 without a snapshot: %+v", len(models), models)
	}
}
