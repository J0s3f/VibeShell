package discovery

import (
	"fmt"
	"sort"
	"strings"

	"j0s.at/vibeshell/internal/adapters/opencode"
)

// Planner expands auto_free tiers into suitable-free candidates. It is
// pure: the catalogue is injected, requirements are supplied at
// construction, and expansion performs no I/O.
type Planner struct {
	Catalogue    Catalogue
	Requirements Requirements
	MaxRoutes    int
}

// Decide evaluates every catalogue model against req and returns one
// decision per model, eligible and rejected alike, sorted by product then
// model ID. Rejected decisions carry the reason for the exclusion, so
// operators can see why a model was not discovered.
func (p *Planner) Decide(req Requirements) []Decision {
	if p.Catalogue == nil {
		return nil
	}
	models := p.Catalogue.Models()
	out := make([]Decision, 0, len(models))
	for _, m := range models {
		out = append(out, p.evaluate(m, req))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Product != out[j].Product {
			return out[i].Product < out[j].Product
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Expand returns the concrete routes a tier's auto_free flag should add.
//
// explicit holds the route keys already present in the tier (see RouteKey),
// resolved by the composition from its route specs; candidates naming a
// route key in that set are skipped so an explicit route and a discovered
// route never duplicate the same product/model/protocol triple. The
// remaining candidates are ordered deterministically by route key and
// capped at MaxRoutes (DefaultMaxRoutes when unset or non-positive). An
// empty or disabled catalogue yields no expansion and no error.
func (p *Planner) Expand(tierName string, explicit []string) ([]Candidate, error) {
	if p.Catalogue == nil {
		return nil, fmt.Errorf("discovery: tier %q: nil catalogue", tierName)
	}
	skip := make(map[string]bool, len(explicit))
	for _, key := range explicit {
		skip[key] = true
	}
	var out []Candidate
	seen := make(map[string]bool)
	for _, d := range p.Decide(p.Requirements) {
		if !d.Eligible {
			continue
		}
		key := RouteKey(d.Product, d.ID, d.Protocol)
		if skip[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Candidate{
			Route:    key,
			Product:  d.Product,
			Model:    d.ID,
			Protocol: d.Protocol,
			Reason:   d.Reason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Route < out[j].Route })
	max := p.MaxRoutes
	if max <= 0 {
		max = DefaultMaxRoutes
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// evaluate applies the suitable-free policy to one catalogue model. The
// gate sequence mirrors opencode.SuitableFree over provider-neutral types;
// reason labels and protocol classification are shared with that package.
func (p *Planner) evaluate(m Model, req Requirements) Decision {
	d := Decision{Product: m.Product, ID: m.ID}
	if req.ExplicitDeny[m.ID] {
		d.Reason = opencode.ReasonDeniedExplicit
		return d
	}
	if !m.Found {
		d.Reason = opencode.ReasonUnknownMetadata
		return d
	}
	freeSuffix := strings.HasSuffix(m.ID, "-free")
	if !freeSuffix && !req.ExplicitAllow[m.ID] {
		d.Reason = opencode.ReasonNotFreeSuffix
		return d
	}
	if reason, ok := missingCapability(m, req.requiredCapabilities()); ok {
		d.Reason = reason
		return d
	}
	if req.MinContext > 0 && m.Context < req.MinContext {
		d.Reason = opencode.ReasonContextTooSmall
		return d
	}
	if !m.CostKnown {
		d.Reason = opencode.ReasonUnknownCost
		return d
	}
	if !m.ZeroCost {
		d.Reason = opencode.ReasonKnownPaid
		return d
	}
	protocol := opencode.ProtocolFor(opencode.Meta{ID: m.ID, ProviderNPM: m.ProviderNPM}, m.Found, req.protocolOverrides())
	if protocol == "" {
		d.Reason = opencode.ReasonUnknownProtocol
		return d
	}
	d.Protocol = string(protocol)
	if freeSuffix {
		d.Reason = opencode.ReasonEligibleAutoFree
	} else {
		d.Reason = opencode.ReasonEligibleExplicit
	}
	d.Eligible = true
	return d
}

// missingCapability reports the reason label for the first required
// capability the model lacks, or ok=false when the model satisfies every
// requirement. Unknown capability names are ignored: configuration
// validation rejects them before the planner runs.
func missingCapability(m Model, required []string) (string, bool) {
	for _, cap := range required {
		var has bool
		switch cap {
		case CapabilityToolCall:
			has = m.ToolCall
		case CapabilityTextInput:
			has = m.TextInput
		case CapabilityTextOutput:
			has = m.TextOutput
		default:
			continue
		}
		if !has {
			if cap == CapabilityToolCall {
				return opencode.ReasonNoToolSupport, true
			}
			return opencode.ReasonNoTextModality, true
		}
	}
	return "", false
}
