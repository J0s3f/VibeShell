package catalog

import (
	"fmt"
	"strings"

	"j0s.at/vibeshell/experiments/opencode-protocol"
)

// Options configures PLAN 8.3 suitable-free discovery. Unknown cost or
// unknown capability is explicit (NeedsOperator), never silently zero.
type Options struct {
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
	ProtocolOverrides map[string]gateway.Protocol
}

// Decision is the suitability outcome for one catalogue ID on one product.
type Decision struct {
	Product   gateway.Product
	ID        string
	Eligible  bool
	Reason    string
	Protocol  gateway.Protocol
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
func SuitableFree(product gateway.Product, id string, metas map[string]Meta, opts Options) Decision {
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
func ExpandFree(product gateway.Product, ids []string, metas map[string]Meta, opts Options) (eligible []Decision, rejected []Decision) {
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

// EndpointForDecision resolves the inference URL for an eligible decision.
// It refuses unknown protocols: a model whose protocol is not classified
// is never silently sent to a chat endpoint.
func EndpointForDecision(d Decision) (string, error) {
	if !d.Eligible {
		return "", fmt.Errorf("catalog: model %q is not eligible (%s)", d.ID, d.Reason)
	}
	if d.Protocol == "" {
		return "", fmt.Errorf("catalog: model %q has unknown protocol; refusing chat default", d.ID)
	}
	return gateway.EndpointFor(d.Product, d.Protocol)
}
