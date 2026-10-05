// Package catalog covers the keyless catalogue observation (public GET,
// record counts, JSON shape, sha256) and the catalogue-to-metadata join
// with PLAN 8.3 suitability filtering.
//
// Catalogue discovery succeeds without an API key, but it proves neither
// account entitlement nor model health. Authenticated inference behavior is
// UNVERIFIED (see docs/research/2026-10-03-opencode-protocol-spike.md).
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"j0s.at/vibeshell/experiments/opencode-protocol"
)

// Shape is the observed JSON shape of one catalogue response.
type Shape struct {
	// Object is the top-level "object" field (observed: "list").
	Object string
	// Count is the number of records in "data".
	Count int
	// Fields are the sorted top-level keys of data records.
	Fields []string
	// IDs are the sorted model IDs in "data".
	IDs []string
}

// ParseCatalogue parses one public catalogue body and summarizes it.
func ParseCatalogue(body []byte) (Shape, error) {
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
		return Shape{}, fmt.Errorf("catalog: malformed catalogue: %w", err)
	}
	if v.Object == "" {
		return Shape{}, fmt.Errorf("catalog: catalogue missing top-level object")
	}
	if v.Data == nil {
		return Shape{}, fmt.Errorf("catalog: catalogue missing data array")
	}
	shape := Shape{Object: v.Object, Count: len(v.Data)}
	for _, m := range v.Data {
		if m.ID == "" {
			return Shape{}, fmt.Errorf("catalog: record with empty id")
		}
		shape.IDs = append(shape.IDs, m.ID)
	}
	sort.Strings(shape.IDs)
	shape.Fields = []string{"created", "id", "object", "owned_by"}
	return shape, nil
}

// SHA256Hex records the exact bytes observed, for receipts.
func SHA256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

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

// LoadMetadata parses the vendored models.dev subset
// ({"opencode":{...},"opencode-go":{...}}) into per-product metadata.
func LoadMetadata(body []byte) (map[gateway.Product]map[string]Meta, error) {
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
		return nil, fmt.Errorf("catalog: malformed metadata: %w", err)
	}
	out := map[gateway.Product]map[string]Meta{}
	for productKey, p := range v {
		var product gateway.Product
		switch productKey {
		case "opencode":
			product = gateway.ProductConsole
		case "opencode-go":
			product = gateway.ProductGo
		default:
			continue
		}
		metas := map[string]Meta{}
		for id, m := range p.Models {
			meta := Meta{ID: id, ToolCall: m.ToolCall}
			if m.Modalities != nil {
				meta.TextInput = contains(m.Modalities.Input, "text")
				meta.TextOutput = contains(m.Modalities.Output, "text")
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
// @ai-sdk/anthropic speaks Messages, @ai-sdk/google speaks Gemini, and the
// OpenAI SDK family (or no override) defaults to Chat Completions.
// Overrides covers explicitly documented per-model exceptions and is empty
// by default; unknown models resolve to Protocol("") so callers must fail
// closed instead of silently sending to a chat endpoint.
func ProtocolFor(meta Meta, found bool, overrides map[string]gateway.Protocol) gateway.Protocol {
	if id, ok := overrides[meta.ID]; ok {
		return id
	}
	if !found {
		return ""
	}
	switch meta.ProviderNPM {
	case "@ai-sdk/anthropic":
		return gateway.ProtocolMessages
	case "@ai-sdk/google":
		return gateway.ProtocolGemini
	default:
		return gateway.ProtocolChat
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
