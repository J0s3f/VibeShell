// Package discovery expands configured auto_free tiers into concrete
// suitable-free route candidates over an injected catalogue port.
//
// The package is pure: it performs no I/O and treats the injected Catalogue
// as the last observed truth. The composition root supplies the catalogue
// (for example the OpenCode catalogue cache via NewCacheCatalogue) and
// receives candidates with explicit decision reasons. The suitability
// policy mirrors internal/adapters/opencode.SuitableFree over
// provider-neutral types and shares that package's reason labels and
// protocol classification, so a model rejected by the provider policy is
// rejected here with the same reason.
package discovery

import (
	"j0s.at/vibeshell/internal/adapters/opencode"
)

// DefaultMaxRoutes bounds the number of routes one auto_free expansion may
// contribute when the planner's MaxRoutes is unset or non-positive.
const DefaultMaxRoutes = 16

// DefaultCapabilities is the capability set required when the configuration
// leaves discovery.capabilities unset: text in/out plus tool support. An
// operator relaxes the set explicitly; the planner never silently drops a
// requirement.
var DefaultCapabilities = []string{CapabilityToolCall, CapabilityTextInput, CapabilityTextOutput}

// Capability names accepted in Requirements.Capabilities. Configuration
// validation rejects anything else before the planner runs.
const (
	CapabilityToolCall   = "tool_call"
	CapabilityTextInput  = "text_input"
	CapabilityTextOutput = "text_output"
)

// Requirements mirrors the discovery config group: the context floor, the
// capability set, and the explicit allow/deny lists a model must satisfy.
// ProtocolOverrides pins per-model wire protocols explicitly. The zero
// Requirements value is valid: no context floor, the default capability
// set, and no allow/deny entries.
type Requirements struct {
	MinContext        int
	Capabilities      []string
	ExplicitAllow     map[string]bool
	ExplicitDeny      map[string]bool
	ProtocolOverrides map[string]string
}

// requiredCapabilities normalizes the configured capability set. An empty
// list selects DefaultCapabilities so an unset configuration keeps the
// fail-closed default.
func (r Requirements) requiredCapabilities() []string {
	if len(r.Capabilities) == 0 {
		return DefaultCapabilities
	}
	return r.Capabilities
}

// protocolOverrides converts the provider-neutral override map to the
// shape opencode.ProtocolFor expects.
func (r Requirements) protocolOverrides() map[string]opencode.Protocol {
	if len(r.ProtocolOverrides) == 0 {
		return nil
	}
	out := make(map[string]opencode.Protocol, len(r.ProtocolOverrides))
	for model, proto := range r.ProtocolOverrides {
		out[model] = opencode.Protocol(proto)
	}
	return out
}

// Model is one catalogue entry: a model ID on one product route with the
// metadata needed for suitability decisions. Found reports whether the
// metadata snapshot has a record for the ID; an absent record is an
// operator decision, never a silent zero.
type Model struct {
	Product     string
	ID          string
	Found       bool
	ToolCall    bool
	TextInput   bool
	TextOutput  bool
	Context     int
	CostKnown   bool
	ZeroCost    bool
	ProviderNPM string
}

// Decision is the suitability outcome for one catalogue ID on one product
// route. Reason is a stable label shared with the provider catalogue
// adapter (opencode.Reason*); eligible decisions carry a Protocol.
type Decision struct {
	Product  string
	ID       string
	Eligible bool
	Reason   string
	Protocol string
}

// Catalogue is the injected view of available models. The composition root
// adapts the provider catalogue cache to this port; this package performs
// no I/O and never invents models the catalogue did not report.
type Catalogue interface {
	// Models returns every known model across product routes, including
	// IDs without a metadata record. The order is unspecified; the
	// planner sorts.
	Models() []Model
}

// Candidate is one concrete route an auto_free tier expansion contributes.
// Route is the deterministic route key (see RouteKey) the composition mints
// a domain route identity from; Product, Model, and Protocol identify the
// upstream route; Reason records why the model is eligible.
type Candidate struct {
	Route    string
	Product  string
	Model    string
	Protocol string
	Reason   string
}

// RouteKey returns the deterministic route key for one product/model/
// protocol triple. The composition uses it both to deduplicate explicit and
// discovered entries and to mint stable route identities.
func RouteKey(product, model, protocol string) string {
	return product + "/" + model + "/" + protocol
}
