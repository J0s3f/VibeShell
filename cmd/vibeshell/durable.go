package main

import (
	"fmt"

	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/routing"
)

// The durable adapters serve more than one consumer: the router, the admin
// surface, and the generated-application engine each declare their own port,
// and one SQLite-backed store satisfies them all. These assertions keep that
// true at compile time, so a store that drifts from a port fails the build
// rather than the first request.
var (
	_ routing.HealthStore     = (*sqlite.RouteHealth)(nil)
	_ routing.QuotaGroupStore = (*sqlite.RouteHealth)(nil)
	_ admin.HealthStore       = (*sqlite.RouteHealth)(nil)
	_ routing.SpendingLedger  = (*sqlite.Spending)(nil)
	_ ports.AppRegistry       = (*sqlite.Apps)(nil)
)

// durableState is the set of SQLite-backed adapters built over one open,
// migrated database. They share the database handle, so the component closes
// them together by closing the store; no adapter owns a connection of its own.
type durableState struct {
	apps     *sqlite.Apps
	health   *sqlite.RouteHealth
	spending *sqlite.Spending
}

// openDurableState constructs the durable adapters. It must run after
// sqlite.Open, which applies pending migrations: each constructor checks for
// the tables it owns so a missing migration fails startup instead of the first
// read. A construction failure is a wiring or schema fault, not an operator
// condition, so it aborts startup.
func openDurableState(db *sqlite.DB, clock ports.Clock) (*durableState, error) {
	appStore, err := sqlite.NewApps(db.SQL(), clock)
	if err != nil {
		return nil, fmt.Errorf("open app store: %w", err)
	}
	routeHealth, err := sqlite.NewRouteHealth(db.SQL(), clock)
	if err != nil {
		return nil, fmt.Errorf("open route health store: %w", err)
	}
	spending, err := sqlite.NewSpending(db.SQL(), clock)
	if err != nil {
		return nil, fmt.Errorf("open spending ledger: %w", err)
	}
	return &durableState{apps: appStore, health: routeHealth, spending: spending}, nil
}

// buildRouter wires the model router over the durable state. Route and account
// health, quota-group exhaustion, and spending accounting are therefore the
// same rows the admin surface reads and a restart re-reads, rather than
// per-process state that would clear every cooldown and reset a spent budget.
func buildRouter(gateway ports.ModelGateway, state *durableState, clock ports.Clock, random ports.Random, cfg routing.Config) *routing.Router {
	return routing.NewRouter(
		gateway, state.health, state.spending, clock, random, cfg,
		routing.WithQuotaGroups(state.health),
	)
}
