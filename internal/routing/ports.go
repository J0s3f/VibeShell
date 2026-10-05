package routing

import (
	"context"

	"j0s.at/vibeshell/internal/domain"
)

// HealthStore is the router's persistence port for health records
// (PLAN 9.2: failure reason, affected scope, last attempt, consecutive
// failures, next probe time, and last known success are persisted). A
// missing record means "no known problem", i.e. healthy. Mutation happens
// only through Get-then-Save by the router; implementations may be a
// SQLite adapter or a test double.
type HealthStore interface {
	// Get returns the record for key, or ok=false when none exists.
	Get(ctx context.Context, key domain.HealthKey) (rec domain.HealthRecord, ok bool, err error)
	// Save upserts one record.
	Save(ctx context.Context, rec domain.HealthRecord) error
	// List returns all records, for due-probe scanning and admin status.
	List(ctx context.Context) ([]domain.HealthRecord, error)
}

// QuotaGroupStore is the router's persistence port for quota-group
// exhaustion marks (PLAN 8.2). Several accounts can share one quota group,
// so a quota or rate-limit failure blocks the whole group until its reset
// time instead of producing one identical retry per account in it. The mark
// must outlive the process: clearing it on restart would turn one exhausted
// group into a fresh retry storm. Without a store the router keeps the marks
// in process memory only.
type QuotaGroupStore interface {
	// BlockQuotaGroup marks a group exhausted until until (unix ms). A later
	// mark replaces an earlier one.
	BlockQuotaGroup(ctx context.Context, group string, until int64) error
	// QuotaGroupBlocked reports whether the group is still exhausted at now.
	// An unknown group is never blocked.
	QuotaGroupBlocked(ctx context.Context, group string, now int64) (bool, error)
}

// SpendingRef identifies the spending reservation of one attempt so a
// reservation, its reconciliation, and its release net against the same
// logical entry (PLAN 9.3).
type SpendingRef struct {
	Session domain.SessionID
	Turn    domain.TurnID
	Route   domain.RouteID
	Account domain.AccountID
}

// SpendingLedger is the spending-limits port (PLAN 9.3). Parallel
// requests must not all spend the same remaining budget: Reserve draws
// the estimated maximum down before the request, Reconcile settles with
// reported usage afterwards, and Release refunds when the attempt never
// produced usage. Amounts are USD; the router labels unknown cost
// estimates explicitly by not reserving.
type SpendingLedger interface {
	Reserve(ctx context.Context, ref SpendingRef, amount float64) error
	Reconcile(ctx context.Context, ref SpendingRef, actual float64) error
	Release(ctx context.Context, ref SpendingRef) error
}
