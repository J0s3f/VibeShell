package sqlite

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/routing"
)

func mustTurnID(t *testing.T) domain.TurnID {
	t.Helper()
	raw, err := newTestPrefixID(domain.PrefixTurn)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseTurnID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// attemptRef is one attempt's spending reference: a session, a turn, and the
// route/account it was served by.
func attemptRef(t *testing.T) routing.SpendingRef {
	t.Helper()
	raw, err := newTestPrefixID(domain.PrefixSession)
	if err != nil {
		t.Fatal(err)
	}
	session, err := domain.ParseSessionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return routing.SpendingRef{
		Session: session,
		Turn:    mustTurnID(t),
		Route:   mustRouteID(t),
		Account: mustAccountID(t),
	}
}

func usageOf(t *testing.T, ledger *Spending, ref routing.SpendingRef) AccountUsage {
	t.Helper()
	usage, ok, err := ledger.Usage(context.Background(), ref.Account, ref.Route)
	if err != nil || !ok {
		t.Fatalf("Usage: ok=%v err=%v", ok, err)
	}
	return usage
}

func TestSpendingReserveAndReconcilePersist(t *testing.T) {
	ctx := context.Background()
	db, path := durableDB(t)
	const now = 1_700_000_800_000
	ledger1, err := NewSpending(db.SQL(), fixedTestClock{now: now})
	if err != nil {
		t.Fatalf("NewSpending: %v", err)
	}
	ref := attemptRef(t)
	const estimate, actual = 0.25, 0.11
	if err := ledger1.Reserve(ctx, ref, estimate); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	db = restart(t, db, path)
	ledger2, err := NewSpending(db.SQL(), fixedTestClock{now: now + 1_000})
	if err != nil {
		t.Fatalf("NewSpending after restart: %v", err)
	}
	// The drawn-down budget must still be held after a restart, otherwise
	// parallel turns could each spend the same remaining budget.
	held := usageOf(t, ledger2, ref)
	if held.ReservedUSD != estimate || held.EstimatedUSD != estimate || held.ReportedUSD != 0 {
		t.Errorf("usage after restart = %+v, want %v reserved and %v estimated", held, estimate, estimate)
	}
	if held.UpdatedAt != now {
		t.Errorf("usage updated_at = %d, want the reservation time %d", held.UpdatedAt, now)
	}

	if err := ledger2.Reconcile(ctx, ref, actual); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	db = restart(t, db, path)
	ledger3, err := NewSpending(db.SQL(), fixedTestClock{now: now + 2_000})
	if err != nil {
		t.Fatalf("NewSpending after reconcile: %v", err)
	}
	settled := usageOf(t, ledger3, ref)
	if settled.ReportedUSD != actual || settled.ReservedUSD != 0 || settled.EstimatedUSD != estimate {
		t.Errorf("settled usage = %+v, want %v reported, nothing reserved, %v estimated", settled, actual, estimate)
	}
}

func TestSpendingReconcileIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	ledger, err := NewSpending(db.SQL(), fixedTestClock{now: 1})
	if err != nil {
		t.Fatalf("NewSpending: %v", err)
	}
	ref := attemptRef(t)
	if err := ledger.Reserve(ctx, ref, 0.5); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// A repeated reserve must not draw the budget down twice.
	if err := ledger.Reserve(ctx, ref, 0.5); err != nil {
		t.Fatalf("repeat Reserve: %v", err)
	}
	if got := usageOf(t, ledger, ref); got.EstimatedUSD != 0.5 || got.ReservedUSD != 0.5 {
		t.Errorf("usage after a repeated reserve = %+v, want one 0.5 draw", got)
	}
	if err := ledger.Reconcile(ctx, ref, 0.2); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := ledger.Reconcile(ctx, ref, 0.2); err != nil {
		t.Fatalf("repeat Reconcile: %v", err)
	}
	if got := usageOf(t, ledger, ref); got.ReportedUSD != 0.2 || got.ReservedUSD != 0 {
		t.Errorf("usage after a repeated reconcile = %+v, want one 0.2 charge", got)
	}
}

func TestSpendingReleaseRefundsTheDrawnAmount(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	ledger, err := NewSpending(db.SQL(), fixedTestClock{now: 1})
	if err != nil {
		t.Fatalf("NewSpending: %v", err)
	}
	ref := attemptRef(t)
	if err := ledger.Reserve(ctx, ref, 0.4); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := ledger.Release(ctx, ref); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := ledger.Release(ctx, ref); err != nil {
		t.Fatalf("repeat Release: %v", err)
	}
	got := usageOf(t, ledger, ref)
	if got.ReservedUSD != 0 || got.ReportedUSD != 0 || got.EstimatedUSD != 0.4 {
		t.Errorf("usage after release = %+v, want nothing held or charged", got)
	}
	// Usage reported for a refunded attempt is a conflict: the budget was
	// already handed back and cannot be charged twice.
	if err := ledger.Reconcile(ctx, ref, 0.3); !domain.IsConflictError(err) {
		t.Errorf("Reconcile after Release = %v, want a conflict", err)
	}
	if got := usageOf(t, ledger, ref); got.ReportedUSD != 0 {
		t.Errorf("usage after a refused reconcile = %+v, want nothing charged", got)
	}
}

func TestSpendingRefusesUnaccountableSteps(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	ledger, err := NewSpending(db.SQL(), fixedTestClock{now: 1})
	if err != nil {
		t.Fatalf("NewSpending: %v", err)
	}
	ref := attemptRef(t)
	// The router labels an unknown cost estimate by not reserving, so a
	// non-positive reservation is a contract violation, not a zero charge.
	if err := ledger.Reserve(ctx, ref, 0); domain.GetErrorCode(err) != domain.CodeInvalidInput {
		t.Errorf("Reserve(0) = %v, want %s", err, domain.CodeInvalidInput)
	}
	if err := ledger.Reconcile(ctx, ref, 0.1); domain.GetErrorCode(err) != domain.CodeInvalidInput {
		t.Errorf("Reconcile without a reservation = %v, want %s", err, domain.CodeInvalidInput)
	}
	// Releasing an attempt that reserved nothing is a no-op, not an error.
	if err := ledger.Release(ctx, ref); err != nil {
		t.Errorf("Release without a reservation = %v, want nil", err)
	}
	if err := ledger.Reserve(ctx, routing.SpendingRef{Turn: ref.Turn, Route: ref.Route, Account: ref.Account}, 0.1); domain.GetErrorCode(err) != domain.CodeInvalidIdentity {
		t.Errorf("Reserve without a session = %v, want %s", err, domain.CodeInvalidIdentity)
	}
	if _, ok, err := ledger.Usage(ctx, ref.Account, ref.Route); err != nil || ok {
		t.Errorf("Usage with nothing reserved = ok:%v err:%v, want ok=false", ok, err)
	}
}

func TestSpendingParallelReservationsDoNotShareOneBudget(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	ledger, err := NewSpending(db.SQL(), fixedTestClock{now: 1})
	if err != nil {
		t.Fatalf("NewSpending: %v", err)
	}
	base := attemptRef(t)
	const attempts = 8
	const estimate = 0.125
	// Identities are minted up front: each attempt must draw its own budget,
	// and *testing.T must not be used from a helper goroutine.
	turns := make([]domain.TurnID, attempts)
	for i := range turns {
		turns[i] = mustTurnID(t)
	}

	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for _, turn := range turns {
		wg.Add(1)
		go func(turn domain.TurnID) {
			defer wg.Done()
			ref := routing.SpendingRef{Session: base.Session, Turn: turn, Route: base.Route, Account: base.Account}
			if err := ledger.Reserve(ctx, ref, estimate); err != nil {
				errs <- fmt.Errorf("turn %s: %w", turn, err)
			}
		}(turn)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Reserve: %v", err)
	}

	got := usageOf(t, ledger, base)
	if want := attempts * estimate; got.ReservedUSD != want || got.EstimatedUSD != want {
		t.Errorf("usage after %d parallel reservations = %+v, want %v held", attempts, got, want)
	}

	// One attempt settles: only its own share leaves the held amount.
	settled := routing.SpendingRef{Session: base.Session, Turn: turns[0], Route: base.Route, Account: base.Account}
	if err := ledger.Reconcile(ctx, settled, 0.1); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got = usageOf(t, ledger, base)
	if want := (attempts - 1) * estimate; got.ReservedUSD != want {
		t.Errorf("usage after one settlement = %+v, want %v still held", got, want)
	}
	if got.ReportedUSD != 0.1 {
		t.Errorf("usage after one settlement = %+v, want 0.1 reported", got)
	}
}
