package discovery

import (
	"time"

	"j0s.at/vibeshell/internal/adapters/opencode"
	"j0s.at/vibeshell/internal/ports"
)

// cacheCatalogue adapts the OpenCode catalogue cache to the Catalogue
// port. It performs no I/O: catalogue refreshes and metadata loads happen
// elsewhere, and this adapter only reads last-successful snapshots within
// their configured age.
type cacheCatalogue struct {
	cache *opencode.Cache
	clock ports.Clock
}

// NewCacheCatalogue returns a Catalogue backed by an OpenCode catalogue
// cache. The clock selects snapshot freshness; the cache enforces its own
// staleness bound (MaxAgeMs). A nil clock falls back to the wall clock.
func NewCacheCatalogue(cache *opencode.Cache, clock ports.Clock) Catalogue {
	return &cacheCatalogue{cache: cache, clock: clock}
}

// Models returns every model ID in the fresh catalogue snapshots, paired
// with its metadata when the metadata snapshot has a record. Products
// without a fresh snapshot contribute nothing: a failed refresh never
// invents models, and a model without a metadata record is reported with
// Found=false so the planner demands an operator decision.
func (c *cacheCatalogue) Models() []Model {
	now := c.now()
	var out []Model
	for _, product := range []opencode.Product{opencode.ProductConsole, opencode.ProductGo} {
		snap, ok := c.cache.SnapshotWithinAge(product, now)
		if !ok {
			continue
		}
		metas, _ := c.cache.MetadataFor(product)
		for _, id := range snap.IDs {
			m := Model{Product: string(product), ID: id}
			if meta, found := metas[id]; found {
				m.Found = true
				m.ToolCall = meta.ToolCall
				m.TextInput = meta.TextInput
				m.TextOutput = meta.TextOutput
				m.Context = meta.Context
				m.CostKnown = meta.CostKnown
				m.ZeroCost = meta.ZeroCost
				m.ProviderNPM = meta.ProviderNPM
			}
			out = append(out, m)
		}
	}
	return out
}

func (c *cacheCatalogue) now() int64 {
	if c.clock != nil {
		return c.clock.NowUnixMilli()
	}
	return time.Now().UnixMilli()
}
