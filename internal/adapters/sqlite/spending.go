package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/routing"
)

// Compile-time proof that the store stays behind the router's ledger port.
var _ routing.SpendingLedger = (*Spending)(nil)

// Reservation states. A reservation is drawn before the request, then settled
// with the reported usage or refunded when the attempt never produced usage
// (PLAN 9.3).
const (
	reservationReserved = "reserved"
	reservationSettled  = "settled"
	reservationReleased = "released"
)

// AccountUsage is the durable cost accounting for one account on one route, in
// USD: what reservations drew down, what is still held open, and what the
// provider actually reported.
type AccountUsage struct {
	EstimatedUSD float64
	ReservedUSD  float64
	ReportedUSD  float64
	UpdatedAt    int64
}

// Spending is the SQLite-backed spending ledger. Parallel turns must not all
// spend the same remaining budget, so each attempt draws its estimated maximum
// down (Reserve) before the request and settles afterwards (Reconcile or
// Release). Every step is a durable row: after a restart, an attempt that was
// reserved but never reconciled is still visibly held, and the released or
// settled amount is not counted twice.
type Spending struct {
	sql   *sql.DB
	clock ports.Clock
}

// NewSpending wraps db with the spending ledger. Migrations must already have
// run (Open does that).
func NewSpending(db *sql.DB, clock ports.Clock) (*Spending, error) {
	if db == nil {
		return nil, errors.New("sqlite: spending ledger requires a database handle")
	}
	if clock == nil {
		return nil, errors.New("sqlite: spending ledger requires a clock for usage timestamps")
	}
	if err := requireTable(db, "spending_reservations"); err != nil {
		return nil, err
	}
	return &Spending{sql: db, clock: clock}, nil
}

// ---------------------------------------------------------------------------
// routing.SpendingLedger
// ---------------------------------------------------------------------------

// Reserve draws the estimated maximum cost down before a paid request.
// Reserving the same attempt twice is idempotent: the budget was already drawn,
// and drawing it again would charge the same turn for the same work.
func (s *Spending) Reserve(ctx context.Context, ref routing.SpendingRef, amount float64) error {
	if err := checkSpendingRef(ref); err != nil {
		return err
	}
	if amount <= 0 {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "a reservation requires a positive estimated amount",
			map[string]string{"amount": strconv.FormatFloat(amount, 'g', -1, 64)},
		)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin reserve: %w", err)
	}
	defer tx.Rollback()

	state, _, err := reservation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if state != "" {
		return nil
	}
	now := s.clock.NowUnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO spending_reservations(
			session_id, turn_id, route_id, account_id, reserved_usd, settled_usd, state, updated_at
		) VALUES(?,?,?,?,?,NULL,?,?)`,
		ref.Session.String(), ref.Turn.String(), ref.Route.String(), ref.Account.String(),
		amount, reservationReserved, now,
	); err != nil {
		return fmt.Errorf("sqlite: insert reservation: %w", err)
	}
	if err := applyUsage(ctx, tx, ref, amount, amount, 0, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit reserve: %w", err)
	}
	return nil
}

// Reconcile settles a reservation with the reported usage: the drawn amount is
// released and the actual cost is recorded. Reconciling an already-settled
// attempt is a no-op; reporting usage for a released reservation is a conflict,
// because the budget was already handed back.
func (s *Spending) Reconcile(ctx context.Context, ref routing.SpendingRef, actual float64) error {
	if err := checkSpendingRef(ref); err != nil {
		return err
	}
	if actual < 0 {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "reported cost cannot be negative",
			map[string]string{"actual": strconv.FormatFloat(actual, 'g', -1, 64)},
		)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin reconcile: %w", err)
	}
	defer tx.Rollback()

	state, reserved, err := reservation(ctx, tx, ref)
	if err != nil {
		return err
	}
	switch {
	case state == "":
		return domain.NewValidationError(
			domain.CodeInvalidInput, "usage was reported for an attempt that reserved nothing",
			spendingDetails(ref),
		)
	case state == reservationSettled:
		return nil
	case state == reservationReleased:
		return domain.NewConflictError(
			domain.CodeConcurrentModification, "usage was reported for a released reservation",
			spendingDetails(ref),
		)
	}
	now := s.clock.NowUnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE spending_reservations
		SET state = ?, settled_usd = ?, updated_at = ?
		WHERE session_id = ? AND turn_id = ? AND route_id = ? AND account_id = ? AND state = ?`,
		reservationSettled, actual, now,
		ref.Session.String(), ref.Turn.String(), ref.Route.String(), ref.Account.String(),
		reservationReserved,
	); err != nil {
		return fmt.Errorf("sqlite: settle reservation: %w", err)
	}
	if err := applyUsage(ctx, tx, ref, 0, -reserved, actual, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit reconcile: %w", err)
	}
	return nil
}

// Release refunds a reservation whose attempt never produced usage. Releasing
// an attempt that reserved nothing, or that was already released, is a no-op;
// releasing a settled reservation is a conflict, because the cost was already
// recorded.
func (s *Spending) Release(ctx context.Context, ref routing.SpendingRef) error {
	if err := checkSpendingRef(ref); err != nil {
		return err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin release: %w", err)
	}
	defer tx.Rollback()

	state, reserved, err := reservation(ctx, tx, ref)
	if err != nil {
		return err
	}
	switch {
	case state == "", state == reservationReleased:
		return nil
	case state == reservationSettled:
		return domain.NewConflictError(
			domain.CodeConcurrentModification, "a settled reservation cannot be refunded",
			spendingDetails(ref),
		)
	}
	now := s.clock.NowUnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE spending_reservations
		SET state = ?, updated_at = ?
		WHERE session_id = ? AND turn_id = ? AND route_id = ? AND account_id = ? AND state = ?`,
		reservationReleased, now,
		ref.Session.String(), ref.Turn.String(), ref.Route.String(), ref.Account.String(),
		reservationReserved,
	); err != nil {
		return fmt.Errorf("sqlite: release reservation: %w", err)
	}
	if err := applyUsage(ctx, tx, ref, 0, -reserved, 0, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit release: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Usage reporting
// ---------------------------------------------------------------------------

// Usage returns the cost accounting for one account on one route, or ok=false
// when that pair never reserved anything.
func (s *Spending) Usage(ctx context.Context, account domain.AccountID, route domain.RouteID) (AccountUsage, bool, error) {
	var usage AccountUsage
	err := s.sql.QueryRowContext(ctx, `SELECT estimated_usd, reserved_usd, reported_usd, updated_at
		FROM account_usage WHERE account_id = ? AND route_id = ?`,
		account.String(), route.String(),
	).Scan(&usage.EstimatedUSD, &usage.ReservedUSD, &usage.ReportedUSD, &usage.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AccountUsage{}, false, nil
	case err != nil:
		return AccountUsage{}, false, fmt.Errorf("sqlite: read account usage: %w", err)
	}
	return usage, true, nil
}

// ---------------------------------------------------------------------------
// Row helpers
// ---------------------------------------------------------------------------

// reservation reads one reservation row, returning an empty state when the
// attempt never reserved anything.
func reservation(ctx context.Context, tx *sql.Tx, ref routing.SpendingRef) (state string, reserved float64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT state, reserved_usd FROM spending_reservations
		WHERE session_id = ? AND turn_id = ? AND route_id = ? AND account_id = ?`,
		ref.Session.String(), ref.Turn.String(), ref.Route.String(), ref.Account.String(),
	).Scan(&state, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: read reservation: %w", err)
	}
	return state, reserved, nil
}

// applyUsage adds the deltas of one ledger step to the account's totals.
func applyUsage(ctx context.Context, tx *sql.Tx, ref routing.SpendingRef, estimated, reserved, reported float64, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_usage(
			account_id, route_id, estimated_usd, reserved_usd, reported_usd, updated_at
		) VALUES(?,?,?,?,?,?)
		ON CONFLICT(account_id, route_id) DO UPDATE SET
			estimated_usd = estimated_usd + excluded.estimated_usd,
			reserved_usd = reserved_usd + excluded.reserved_usd,
			reported_usd = reported_usd + excluded.reported_usd,
			updated_at = excluded.updated_at`,
		ref.Account.String(), ref.Route.String(), estimated, reserved, reported, now,
	); err != nil {
		return fmt.Errorf("sqlite: update account usage: %w", err)
	}
	return nil
}

func checkSpendingRef(ref routing.SpendingRef) error {
	if ref.Session.IsZero() || ref.Turn.IsZero() || ref.Route.IsZero() || ref.Account.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidIdentity, "a spending reference requires session, turn, route, and account",
			spendingDetails(ref),
		)
	}
	return nil
}

func spendingDetails(ref routing.SpendingRef) map[string]string {
	return map[string]string{
		"session": ref.Session.String(),
		"turn":    ref.Turn.String(),
		"route":   ref.Route.String(),
		"account": ref.Account.String(),
	}
}

// spendingV3Schema holds the cost accounting tables.
const spendingV3Schema = `
CREATE TABLE account_usage (
	account_id TEXT NOT NULL,
	route_id TEXT NOT NULL DEFAULT '',
	estimated_usd REAL NOT NULL DEFAULT 0,
	reserved_usd REAL NOT NULL DEFAULT 0,
	reported_usd REAL NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(account_id, route_id)
);

-- One row per attempt that drew budget down, so a reservation, its
-- reconciliation, and its release net against the same logical entry even when
-- the process restarts between them.
CREATE TABLE spending_reservations (
	session_id TEXT NOT NULL,
	turn_id TEXT NOT NULL,
	route_id TEXT NOT NULL,
	account_id TEXT NOT NULL,
	reserved_usd REAL NOT NULL DEFAULT 0,
	settled_usd REAL,
	state TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(session_id, turn_id, route_id, account_id)
);
CREATE INDEX idx_spending_reservations_state ON spending_reservations(state);
`
