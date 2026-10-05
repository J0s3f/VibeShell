// Package provider multiplexes inference across independent provider
// implementations. Each configured route is owned by exactly one provider: the
// HTTP adapter for provider APIs, the OpenCode CLI adapter for models that are
// reachable only through the CLI, or any future implementation of
// ports.ModelGateway.
//
// The Registry is itself a ports.ModelGateway, so routing, the admission gate,
// and request logging keep depending on a single gateway no matter how many
// providers exist. Adding a provider is therefore a composition change: the
// shell, the SSH adapter, and the routing rules are untouched.
package provider

import (
	"context"
	"fmt"
	"sync"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Registry dispatches each model request to the provider that owns its route.
// Build one with NewRegistry; the zero Registry has no route table to write to.
//
// A Registry is safe for concurrent use: composition may bind routes while turns
// are being dispatched.
type Registry struct {
	clock ports.Clock

	mu      sync.RWMutex
	byRoute map[domain.RouteID]ports.ModelGateway
}

// NewRegistry builds an empty registry. clock stamps the failure envelope a
// dispatch miss returns; a nil clock leaves that timestamp zero, which keeps a
// miss deterministic where no wall clock is wanted.
func NewRegistry(clock ports.Clock) *Registry {
	return &Registry{clock: clock, byRoute: make(map[domain.RouteID]ports.ModelGateway)}
}

// Register binds a route to its provider. A later registration for the same
// route replaces the earlier one, so the composition root decides the final
// binding by ordering its own registrations. gateway must not be nil.
func (r *Registry) Register(route domain.RouteID, gateway ports.ModelGateway) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byRoute[route] = gateway
}

// Provider returns the gateway registered for a route, and false when the route
// has no provider.
func (r *Registry) Provider(route domain.RouteID) (ports.ModelGateway, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	gateway, ok := r.byRoute[route]
	return gateway, ok
}

// Request dispatches to the gateway registered for req.RouteID and returns its
// response unchanged. A route without a provider fails closed as
// model_not_found rather than being sent anywhere, so a misconfigured route
// surfaces as a route-scoped failure instead of a silent answer.
func (r *Registry) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	// The lookup lock is released before the dispatch: a provider call can be
	// slow, and holding the read lock across it would block rebinding.
	gateway, ok := r.Provider(req.RouteID)
	if !ok {
		return domain.ModelResponse{}, r.noProvider(req)
	}
	return gateway.Request(ctx, req)
}

// Len reports how many routes have a provider, for startup diagnostics.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byRoute)
}

// noProvider builds the fail-closed envelope for a route with no provider.
func (r *Registry) noProvider(req domain.ModelRequest) domain.ErrorEnvelope {
	var nowUnixMilli int64
	if r.clock != nil {
		nowUnixMilli = r.clock.NowUnixMilli()
	}
	return domain.NewErrorEnvelope(
		domain.FailureModelNotFound,
		fmt.Sprintf("no provider registered for route %s", req.RouteID),
		req.RouteID, req.AccountID, nowUnixMilli)
}

var _ ports.ModelGateway = (*Registry)(nil)
