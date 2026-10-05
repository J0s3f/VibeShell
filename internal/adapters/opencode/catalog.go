package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/ports"
)

// Meta is the models.dev capability/cost record for one model on one
// product route. Costs from another product must never be inferred onto
// this route.
type Meta struct {
	ID              string
	ToolCall        bool
	TextInput       bool
	TextOutput      bool
	Context         int
	CostKnown       bool
	ZeroCost        bool
	ProviderNPM     string
	HasKnownPricing bool
}

// LoadMetadata parses a models.dev subset
// ({"opencode":{...},"opencode-go":{...}}) into per-product metadata.
func LoadMetadata(body []byte) (map[Product]map[string]Meta, error) {
	var v map[string]struct {
		Models map[string]struct {
			ID         string `json:"id"`
			ToolCall   bool   `json:"tool_call"`
			Modalities *struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Limit *struct {
				Context int `json:"context"`
			} `json:"limit"`
			// Cost objects may carry extra non-price fields (e.g. tier
			// arrays); only scalar price fields decide known/zero cost.
			Cost *struct {
				Input      *float64 `json:"input"`
				Output     *float64 `json:"output"`
				CacheRead  *float64 `json:"cache_read"`
				CacheWrite *float64 `json:"cache_write"`
			} `json:"cost"`
			Provider *struct {
				NPM string `json:"npm"`
			} `json:"provider"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("opencode: malformed metadata: %w", err)
	}
	out := map[Product]map[string]Meta{}
	for productKey, p := range v {
		var product Product
		switch productKey {
		case "opencode":
			product = ProductConsole
		case "opencode-go":
			product = ProductGo
		default:
			continue
		}
		metas := map[string]Meta{}
		for id, m := range p.Models {
			meta := Meta{ID: id, ToolCall: m.ToolCall}
			if m.Modalities != nil {
				meta.TextInput = containsFold(m.Modalities.Input, "text")
				meta.TextOutput = containsFold(m.Modalities.Output, "text")
			}
			if m.Limit != nil {
				meta.Context = m.Limit.Context
			}
			if m.Cost != nil {
				meta.CostKnown = true
				meta.HasKnownPricing = true
				zero := true
				for _, c := range []*float64{m.Cost.Input, m.Cost.Output, m.Cost.CacheRead, m.Cost.CacheWrite} {
					if c != nil && *c != 0 {
						zero = false
					}
				}
				meta.ZeroCost = zero
			}
			if m.Provider != nil {
				meta.ProviderNPM = m.Provider.NPM
			}
			metas[id] = meta
		}
		out[product] = metas
	}
	return out, nil
}

// ProtocolFor classifies the wire protocol from metadata. The models.dev
// provider mapping is the only observed per-model protocol signal:
// @ai-sdk/anthropic speaks Messages, @ai-sdk/google speaks Gemini, and
// the OpenAI SDK family (or no override) defaults to Chat Completions.
// Overrides covers explicitly documented per-model exceptions and is empty
// by default; unknown models resolve to Protocol("") so callers must fail
// closed instead of silently sending to a chat endpoint.
func ProtocolFor(meta Meta, found bool, overrides map[string]Protocol) Protocol {
	if id, ok := overrides[meta.ID]; ok {
		return id
	}
	if !found {
		return ""
	}
	switch meta.ProviderNPM {
	case "@ai-sdk/anthropic":
		return ProtocolMessages
	case "@ai-sdk/google":
		return ProtocolGemini
	default:
		return ProtocolChat
	}
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Suitable-free discovery (PLAN 8.3)
// ---------------------------------------------------------------------------

// FilterOptions configures suitable-free discovery. Unknown cost or
// unknown capability is explicit (NeedsOperator), never silently zero.
type FilterOptions struct {
	// MinContext is the minimum configured context window.
	MinContext int
	// ExplicitAllow names models eligible without a -free suffix (the
	// observed Big Pickle exception: free despite no suffix).
	ExplicitAllow map[string]bool
	// ExplicitDeny names models excluded regardless of suffix/cost.
	ExplicitDeny map[string]bool
	// StructuredOnly names structured-decision models that are never shell
	// text generators (the observed jev-1.13-free exception).
	StructuredOnly map[string]bool
	// ProtocolOverrides pins per-model wire protocols explicitly.
	ProtocolOverrides map[string]Protocol
}

// Decision is the suitability outcome for one catalogue ID on one product.
type Decision struct {
	Product   Product
	ID        string
	Eligible  bool
	Reason    string
	Protocol  Protocol
	CostKnown bool
}

// Reasons are stable decision labels for tests and research records.
const (
	ReasonEligibleAutoFree = "eligible:automatic-free"
	ReasonEligibleExplicit = "eligible:explicit-allow"
	ReasonDeniedExplicit   = "denied:explicit-deny"
	ReasonStructuredOnly   = "denied:structured-decision-only"
	ReasonNotFreeSuffix    = "denied:not-free-suffix"
	ReasonUnknownMetadata  = "needs-operator:unknown-metadata"
	ReasonUnknownCost      = "needs-operator:unknown-cost"
	ReasonKnownPaid        = "denied:known-nonzero-cost"
	ReasonNoTextModality   = "denied:non-text-modality"
	ReasonNoToolSupport    = "denied:no-tool-support"
	ReasonContextTooSmall  = "denied:context-too-small"
	ReasonUnknownProtocol  = "needs-operator:unknown-protocol"
	ReasonNotOnProduct     = "denied:not-on-product"
)

// SuitableFree applies PLAN 8.3 steps to one catalogue ID: -free suffix
// expansion, text/tool/protocol/context intersection, same-product known
// pricing (nonzero excludes; missing requires an operator decision, never
// zero), explicit allow/deny, and protocol attachment so a
// Responses/Messages/Gemini model is never silently sent to chat.
func SuitableFree(product Product, id string, metas map[string]Meta, opts FilterOptions) Decision {
	d := Decision{Product: product, ID: id}
	if opts.ExplicitDeny[id] {
		d.Reason = ReasonDeniedExplicit
		return d
	}
	if opts.StructuredOnly[id] {
		d.Reason = ReasonStructuredOnly
		return d
	}
	meta, found := metas[id]
	if !found {
		d.Reason = ReasonUnknownMetadata
		return d
	}
	freeSuffix := strings.HasSuffix(id, "-free")
	if !freeSuffix && !opts.ExplicitAllow[id] {
		d.Reason = ReasonNotFreeSuffix
		return d
	}
	if !meta.TextInput || !meta.TextOutput {
		d.Reason = ReasonNoTextModality
		return d
	}
	if !meta.ToolCall {
		d.Reason = ReasonNoToolSupport
		return d
	}
	if opts.MinContext > 0 && meta.Context < opts.MinContext {
		d.Reason = ReasonContextTooSmall
		return d
	}
	d.CostKnown = meta.CostKnown
	if !meta.CostKnown {
		d.Reason = ReasonUnknownCost
		return d
	}
	if !meta.ZeroCost {
		d.Reason = ReasonKnownPaid
		return d
	}
	protocol := ProtocolFor(meta, found, opts.ProtocolOverrides)
	if protocol == "" {
		d.Reason = ReasonUnknownProtocol
		return d
	}
	d.Protocol = protocol
	if freeSuffix {
		d.Reason = ReasonEligibleAutoFree
	} else {
		d.Reason = ReasonEligibleExplicit
	}
	d.Eligible = true
	return d
}

// ExpandFree runs discovery over one product catalogue and reports the
// eligible set plus every rejected ID with its reason.
func ExpandFree(product Product, ids []string, metas map[string]Meta, opts FilterOptions) (eligible []Decision, rejected []Decision) {
	for _, id := range ids {
		d := SuitableFree(product, id, metas, opts)
		if d.Eligible {
			eligible = append(eligible, d)
		} else {
			rejected = append(rejected, d)
		}
	}
	return eligible, rejected
}

// EndpointForDecision resolves the inference URL for an eligible
// decision. It refuses unknown protocols: a model whose protocol is not
// classified is never silently sent to a chat endpoint.
func EndpointForDecision(d Decision) (string, error) {
	if !d.Eligible {
		return "", fmt.Errorf("opencode: model %q is not eligible (%s)", d.ID, d.Reason)
	}
	if d.Protocol == "" {
		return "", fmt.Errorf("opencode: model %q has unknown protocol; refusing chat default", d.ID)
	}
	return EndpointFor(d.Product, d.Protocol)
}

// ---------------------------------------------------------------------------
// Catalogue/metadata cache
// ---------------------------------------------------------------------------

// Snapshot is one cached catalogue observation with its provenance:
// where the bytes came from, when, and their hash.
type Snapshot struct {
	// Source is the catalogue URL the bytes were fetched from.
	Source string
	// FetchedAt is the wall-clock fetch time (unix milliseconds).
	FetchedAt int64
	// SHA256 is the hex hash of the exact observed bytes.
	SHA256 string
	// IDs are the sorted model IDs in the catalogue.
	IDs []string
	// Count is the number of records observed.
	Count int
}

// MetadataSnapshot records the provenance of the loaded models.dev subset.
type MetadataSnapshot struct {
	Source    string
	FetchedAt int64
	SHA256    string
}

// Cache holds last-successful catalogue snapshots and models.dev metadata
// with source, fetch time, and hash. Catalogue discovery succeeds without
// an API key, but it proves neither account entitlement nor model health.
// If a refresh fails, callers use the last-known snapshot within its
// configured age via SnapshotWithinAge; the cache never invents models.
type Cache struct {
	mu sync.Mutex

	clock ports.Clock
	http  *http.Client
	// MaxCatalogueBytes caps one catalogue body. Zero selects 4 MiB.
	MaxCatalogueBytes int64
	// MaxAgeMs is the permitted last-known catalogue age. Zero means a
	// failed refresh keeps no usable snapshot (callers must handle it).
	MaxAgeMs int64

	catalogs map[Product]Snapshot
	metas    map[Product]map[string]Meta
	metaProv MetadataSnapshot
}

// parseCatalogue parses one public catalogue body and summarizes it.
func parseCatalogue(body []byte) (Snapshot, error) {
	var v struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return Snapshot{}, fmt.Errorf("opencode: malformed catalogue: %w", err)
	}
	if v.Object == "" {
		return Snapshot{}, fmt.Errorf("opencode: catalogue missing top-level object")
	}
	if v.Data == nil {
		return Snapshot{}, fmt.Errorf("opencode: catalogue missing data array")
	}
	snap := Snapshot{Count: len(v.Data)}
	for _, m := range v.Data {
		if m.ID == "" {
			return Snapshot{}, fmt.Errorf("opencode: catalogue record with empty id")
		}
		snap.IDs = append(snap.IDs, m.ID)
	}
	sort.Strings(snap.IDs)
	return snap, nil
}

// sha256Hex records the exact bytes observed, for receipts.
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (c *Cache) clockNow() int64 {
	if c.clock != nil {
		return c.clock.NowUnixMilli()
	}
	return time.Now().UnixMilli()
}

func (c *Cache) transport() *http.Client {
	if c.http != nil {
		return c.http
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Cache) maxBytes() int64 {
	if c.MaxCatalogueBytes > 0 {
		return c.MaxCatalogueBytes
	}
	return 4 << 20
}

// RefreshCatalog fetches one product catalogue (keyless GET), parses it,
// and stores the snapshot with source, fetch time, and hash. On any
// failure the last-successful snapshot is left intact and the error is
// returned so the caller can fall back to SnapshotWithinAge explicitly.
func (c *Cache) RefreshCatalog(ctx context.Context, product Product, url string) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("opencode: catalogue request: %w", err)
	}
	req.Header.Set("User-Agent", ClientIdentity)
	req.Header.Set("Accept", "application/json")
	resp, err := c.transport().Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("opencode: catalogue fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return Snapshot{}, fmt.Errorf("opencode: catalogue status %d", resp.StatusCode)
	}
	limit := c.maxBytes()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("opencode: catalogue read: %w", err)
	}
	if int64(len(body)) > limit {
		return Snapshot{}, fmt.Errorf("opencode: catalogue body exceeds %d bytes", limit)
	}
	snap, err := parseCatalogue(body)
	if err != nil {
		return Snapshot{}, err
	}
	snap.Source = url
	snap.FetchedAt = c.clockNow()
	snap.SHA256 = sha256Hex(body)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogs == nil {
		c.catalogs = map[Product]Snapshot{}
	}
	c.catalogs[product] = snap
	return snap, nil
}

// SnapshotWithinAge returns the last-successful snapshot for a product
// when it exists and is no older than MaxAgeMs. A failed refresh never
// invents models or paid fallbacks: absence or staleness is reported.
func (c *Cache) SnapshotWithinAge(product Product, now int64) (Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, ok := c.catalogs[product]
	if !ok {
		return Snapshot{}, false
	}
	if c.MaxAgeMs > 0 && now-snap.FetchedAt > c.MaxAgeMs {
		return Snapshot{}, false
	}
	return snap, true
}

// StoreMetadata loads a models.dev subset into the cache, recording its
// source, load time, and hash alongside.
func (c *Cache) StoreMetadata(body []byte, source string) error {
	metas, err := LoadMetadata(body)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metas = metas
	c.metaProv = MetadataSnapshot{Source: source, FetchedAt: c.clockNow(), SHA256: sha256Hex(body)}
	return nil
}

// Metadata returns the cached per-product metadata and its provenance.
func (c *Cache) Metadata() (map[Product]map[string]Meta, MetadataSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.metas, c.metaProv
}

// MetadataFor returns the cached metadata for one product.
func (c *Cache) MetadataFor(product Product) (map[string]Meta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.metas[product]
	return m, ok
}
