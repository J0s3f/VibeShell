package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// HealthState represents the health state of a route/account.
type HealthState string

const (
	HealthHealthy       HealthState = "healthy"
	HealthCoolingDown   HealthState = "cooling_down"
	HealthProbeEligible HealthState = "probe_eligible"
	HealthProbing       HealthState = "probing"
	HealthQuarantined   HealthState = "quarantined"
)

// AllHealthStates returns all known health states.
func AllHealthStates() []HealthState {
	return []HealthState{
		HealthHealthy, HealthCoolingDown, HealthProbeEligible, HealthProbing, HealthQuarantined,
	}
}

// IsValidHealthState returns true if the state is known.
func IsValidHealthState(s HealthState) bool {
	for _, known := range AllHealthStates() {
		if s == known {
			return true
		}
	}
	return false
}

// HealthTransitions defines valid state transitions.
// FromState -> []ToState
var HealthTransitions = map[HealthState][]HealthState{
	HealthHealthy:       {HealthCoolingDown, HealthQuarantined},
	HealthCoolingDown:   {HealthProbeEligible, HealthQuarantined},
	HealthProbeEligible: {HealthProbing, HealthQuarantined},
	HealthProbing:       {HealthHealthy, HealthCoolingDown, HealthQuarantined},
	HealthQuarantined:   {HealthProbeEligible}, // manual recovery or probe
}

// CanTransition returns true if the transition is valid.
func CanTransition(from, to HealthState) bool {
	allowed, ok := HealthTransitions[from]
	if !ok {
		return false
	}
	for _, a := range allowed {
		if a == to {
			return true
		}
	}
	return false
}

// HealthRecord tracks the health of a route/account combination.
type HealthRecord struct {
	RouteID             RouteID      `json:"route_id"`
	AccountID           *AccountID   `json:"account_id,omitempty"` // nil for route-level records
	Scope               string       `json:"scope"`                // "credential", "account", "route", "product", "provider"
	State               HealthState  `json:"state"`
	FailureClass        FailureClass `json:"failure_class,omitempty"`
	FailureMessage      string       `json:"failure_message,omitempty"`
	ConsecutiveFailures int          `json:"consecutive_failures"`
	LastFailure         int64        `json:"last_failure"`             // unix milliseconds
	LastSuccess         int64        `json:"last_success,omitempty"`   // unix milliseconds
	NextProbeAt         int64        `json:"next_probe_at,omitempty"`  // unix milliseconds
	CooldownUntil       int64        `json:"cooldown_until,omitempty"` // unix milliseconds
	RetryAfter          int64        `json:"retry_after,omitempty"`    // milliseconds from provider
	ProbeCount          int          `json:"probe_count"`
	UpdatedAt           int64        `json:"updated_at"`
}

// NewHealthRecord creates a healthy record. The caller supplies the timestamp
// from the Clock port so domain construction stays deterministic.
func NewHealthRecord(route RouteID, account AccountID, scope string, now int64) HealthRecord {
	rec := HealthRecord{
		RouteID:   route,
		Scope:     scope,
		State:     HealthHealthy,
		UpdatedAt: now,
	}
	if !account.IsZero() {
		rec.AccountID = &account
	}
	return rec
}

// RecordFailure updates the record with a failure.
func (h *HealthRecord) RecordFailure(class FailureClass, message string, retryAfter *int64, now int64) {
	if IsHealthNeutral(class) {
		// User cancellation and concurrent world conflicts report no provider
		// fault, so they must not count as consecutive failures or cool the
		// scope (PLAN 9.1).
		return
	}
	h.FailureClass = class
	h.FailureMessage = message
	h.ConsecutiveFailures++
	h.LastFailure = now
	if retryAfter != nil {
		h.RetryAfter = *retryAfter
	} else {
		h.RetryAfter = 0
	}
	h.UpdatedAt = now

	// Determine next state based on failure class and current state
	switch class {
	case FailureInvalidCredential:
		h.transitionTo(HealthCoolingDown, now)
		if retryAfter != nil {
			h.CooldownUntil = now + *retryAfter
		} else {
			h.CooldownUntil = now + defaultCredentialCooldown()
		}
	case FailureQuotaExhausted, FailureRateLimited:
		h.transitionTo(HealthCoolingDown, now)
		if retryAfter != nil {
			h.CooldownUntil = now + *retryAfter
		} else {
			h.CooldownUntil = now + defaultQuotaCooldown()
		}
	case FailureModelNotFound:
		h.transitionTo(HealthQuarantined, now)
	case FailureProviderOutage, FailureNetworkTimeout:
		h.transitionTo(HealthCoolingDown, now)
		h.CooldownUntil = now + defaultOutageCooldown()
	case FailureContextTooLong, FailureInvalidArguments, FailureInvalidResponse, FailureContentRejected:
		// Request-scoped; may not affect health
		if h.ConsecutiveFailures >= 3 {
			h.transitionTo(HealthCoolingDown, now)
		}
	default:
		if h.ConsecutiveFailures >= 3 {
			h.transitionTo(HealthCoolingDown, now)
		}
	}
}

// RecordSuccess updates the record with a success.
func (h *HealthRecord) RecordSuccess(now int64) {
	h.ConsecutiveFailures = 0
	h.LastSuccess = now
	h.FailureClass = ""
	h.FailureMessage = ""
	h.UpdatedAt = now

	if h.State == HealthProbing {
		h.transitionTo(HealthHealthy, now)
	} else if h.State == HealthCoolingDown && now >= h.CooldownUntil {
		h.transitionTo(HealthProbeEligible, now)
	}
}

// RecordProbeStart marks a probe as starting.
func (h *HealthRecord) RecordProbeStart(now int64) {
	if h.State == HealthProbeEligible {
		h.transitionTo(HealthProbing, now)
		h.ProbeCount++
		h.UpdatedAt = now
	}
}

// RecordProbeResult records the outcome of a probe.
func (h *HealthRecord) RecordProbeResult(success bool, now int64) {
	h.UpdatedAt = now
	if success {
		h.transitionTo(HealthHealthy, now)
	} else {
		h.transitionTo(HealthCoolingDown, now)
		h.CooldownUntil = now + defaultProbeCooldown()
	}
}

func (h *HealthRecord) transitionTo(newState HealthState, now int64) {
	if CanTransition(h.State, newState) {
		h.State = newState
		h.UpdatedAt = now
	}
}

// IsHealthy returns true if the route is currently healthy for selection.
func (h HealthRecord) IsHealthy(now int64) bool {
	if h.State == HealthHealthy {
		return true
	}
	if h.State == HealthCoolingDown && now >= h.CooldownUntil {
		return true // eligible for probe
	}
	if h.State == HealthProbeEligible {
		return true // eligible for probe
	}
	return false
}

// IsProbeDue returns true if a probe is due.
func (h HealthRecord) IsProbeDue(now int64) bool {
	return h.State == HealthProbeEligible && now >= h.NextProbeAt
}

// HealthKey identifies a health record scope.
type HealthKey struct {
	RouteID   RouteID
	AccountID AccountID
	Scope     string
}

func (k HealthKey) String() string {
	if k.AccountID != (AccountID{}) {
		return fmt.Sprintf("%s:%s:%s", k.Scope, k.RouteID.Value(), k.AccountID.Value())
	}
	return fmt.Sprintf("%s:%s", k.Scope, k.RouteID.Value())
}

// Default cooldown durations (milliseconds).
const (
	DefaultCredentialCooldownMs = 5 * 60 * 1000       // 5 minutes
	DefaultQuotaCooldownMs      = 10 * 60 * 1000      // 10 minutes
	DefaultOutageCooldownMs     = 30 * 1000           // 30 seconds
	DefaultProbeCooldownMs      = 60 * 1000           // 1 minute
	MaxCooldownMs               = 24 * 60 * 60 * 1000 // 24 hours
)

func defaultCredentialCooldown() int64 {
	return DefaultCredentialCooldownMs
}

func defaultQuotaCooldown() int64 {
	return DefaultQuotaCooldownMs
}

func defaultOutageCooldown() int64 {
	return DefaultOutageCooldownMs
}

func defaultProbeCooldown() int64 {
	return DefaultProbeCooldownMs
}

// Request purposes let an operator designate which routes serve which kind of
// work, so a fast model can answer the short MOTD while a more capable model
// generates application artifacts. A route with no purposes serves every
// purpose, which keeps existing configurations working unchanged.
const (
	PurposeMOTD       = "motd"
	PurposeGeneration = "generation"
)

// RoutePolicy defines the routing configuration.
type RoutePolicy struct {
	Tiers          []TierConfig           `json:"tiers"`
	AccountPools   map[string][]AccountID `json:"account_pools"` // pool name -> accounts
	MaxAttempts    int                    `json:"max_attempts"`
	TurnDeadlineMs int64                  `json:"turn_deadline_ms"`
	ProbeBudget    ProbeBudget            `json:"probe_budget"`
}

// TierConfig defines a routing tier.
type TierConfig struct {
	Name        string    `json:"name"`
	RouteIDs    []RouteID `json:"route_ids"`    // explicit routes
	AutoFree    bool      `json:"auto_free"`    // discover -free models
	AccountPool string    `json:"account_pool"` // pool name
	Enabled     bool      `json:"enabled"`
	MinHealthy  int       `json:"min_healthy"` // min healthy routes before tier exhausted
}

// ProbeBudget configures health probe limits.
type ProbeBudget struct {
	MaxConcurrentProbes int   `json:"max_concurrent_probes"`
	ProbeIntervalMs     int64 `json:"probe_interval_ms"`
	MaxProbesPerHour    int   `json:"max_probes_per_hour"`
}

// ValidateRoutePolicy validates a route policy.
func ValidateRoutePolicy(p RoutePolicy) error {
	if len(p.Tiers) == 0 {
		return errors.New("at least one tier required")
	}
	for i, tier := range p.Tiers {
		if tier.Name == "" {
			return fmt.Errorf("tier %d: name required", i)
		}
		if len(tier.RouteIDs) == 0 && !tier.AutoFree {
			return fmt.Errorf("tier %q: at least one route or auto_free required", tier.Name)
		}
		if tier.AccountPool != "" {
			if _, ok := p.AccountPools[tier.AccountPool]; !ok {
				return fmt.Errorf("tier %q: account pool %q not defined", tier.Name, tier.AccountPool)
			}
		}
	}
	if p.MaxAttempts <= 0 {
		return errors.New("max_attempts must be positive")
	}
	if p.TurnDeadlineMs <= 0 {
		return errors.New("turn_deadline_ms must be positive")
	}
	return nil
}

// MarshalJSON implements json.Marshaler for HealthState.
func (h HealthState) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(h))
}

func (h *HealthState) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if !IsValidHealthState(HealthState(s)) {
		return fmt.Errorf("invalid health state: %q", s)
	}
	*h = HealthState(s)
	return nil
}
