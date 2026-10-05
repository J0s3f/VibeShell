package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/routing"
)

// Compile-time proof that the store stays behind the router's health port.
var _ routing.HealthStore = (*RouteHealth)(nil)

const healthColumns = `route_id, account_id, scope, state, failure_class, failure_message, ` +
	`consecutive_failures, last_failure, last_success, next_probe_at, cooldown_until, retry_after, probe_count, updated_at`

// RouteHealth is the SQLite-backed route/account health store. It persists what
// a restart must not forget: cooldowns and their reset time, the failure class
// that caused them, consecutive failures, probe counters and next-probe time,
// and the last known success (PLAN 9.2). A missing record means "no known
// problem", i.e. healthy, exactly as for the in-memory double.
//
// It also holds quota-group exhaustion marks. Several accounts can share one
// quota group (PLAN 8.2), so the block is stored per group and survives a
// restart instead of being cleared with the process.
type RouteHealth struct {
	sql   *sql.DB
	clock ports.Clock
}

// NewRouteHealth wraps db with the health store. Migrations must already have
// run (Open does that).
func NewRouteHealth(db *sql.DB, clock ports.Clock) (*RouteHealth, error) {
	if db == nil {
		return nil, errors.New("sqlite: route health store requires a database handle")
	}
	if clock == nil {
		return nil, errors.New("sqlite: route health store requires a clock for quota-group marks")
	}
	if err := requireTable(db, "route_health"); err != nil {
		return nil, err
	}
	return &RouteHealth{sql: db, clock: clock}, nil
}

// ---------------------------------------------------------------------------
// routing.HealthStore
// ---------------------------------------------------------------------------

// Get returns the record for key, or ok=false when no problem has ever been
// recorded for that scope.
func (h *RouteHealth) Get(ctx context.Context, key domain.HealthKey) (domain.HealthRecord, bool, error) {
	rec, err := scanHealthRecord(h.sql.QueryRowContext(ctx,
		`SELECT `+healthColumns+` FROM route_health WHERE route_id = ? AND account_id = ? AND scope = ?`,
		key.RouteID.String(), accountKey(key.AccountID), key.Scope))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.HealthRecord{}, false, nil
	case err != nil:
		return domain.HealthRecord{}, false, fmt.Errorf("sqlite: read route health: %w", err)
	}
	return rec, true, nil
}

// Save upserts one record. The record carries its own key and its own update
// timestamp, so the router stays the single owner of health policy (PLAN 9.1).
func (h *RouteHealth) Save(ctx context.Context, rec domain.HealthRecord) error {
	if rec.RouteID.IsZero() || rec.Scope == "" {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "health record requires a route and a scope",
			map[string]string{"route": rec.RouteID.String(), "scope": rec.Scope},
		)
	}
	if !domain.IsValidHealthState(rec.State) {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "unknown health state",
			map[string]string{"state": string(rec.State)},
		)
	}
	if _, err := h.sql.ExecContext(ctx, `INSERT INTO route_health(`+healthColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(route_id, account_id, scope) DO UPDATE SET
			state = excluded.state,
			failure_class = excluded.failure_class,
			failure_message = excluded.failure_message,
			consecutive_failures = excluded.consecutive_failures,
			last_failure = excluded.last_failure,
			last_success = excluded.last_success,
			next_probe_at = excluded.next_probe_at,
			cooldown_until = excluded.cooldown_until,
			retry_after = excluded.retry_after,
			probe_count = excluded.probe_count,
			updated_at = excluded.updated_at`,
		rec.RouteID.String(), recordAccountKey(rec.AccountID), rec.Scope, string(rec.State),
		string(rec.FailureClass), rec.FailureMessage, rec.ConsecutiveFailures, rec.LastFailure,
		rec.LastSuccess, rec.NextProbeAt, rec.CooldownUntil, rec.RetryAfter, rec.ProbeCount, rec.UpdatedAt,
	); err != nil {
		return fmt.Errorf("sqlite: save route health: %w", err)
	}
	return nil
}

// List returns every record ordered by key, so due-probe scanning and operator
// status output are deterministic.
func (h *RouteHealth) List(ctx context.Context) ([]domain.HealthRecord, error) {
	rows, err := h.sql.QueryContext(ctx,
		`SELECT `+healthColumns+` FROM route_health ORDER BY route_id, account_id, scope`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list route health: %w", err)
	}
	defer rows.Close()
	var out []domain.HealthRecord
	for rows.Next() {
		rec, err := scanHealthRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list route health: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Quota groups
// ---------------------------------------------------------------------------

// BlockQuotaGroup marks a quota group exhausted until the given time. Accounts
// sharing that group are skipped until then, so one quota error does not turn
// into one identical retry per account in the group (PLAN 8.2). A later mark
// replaces an earlier one.
func (h *RouteHealth) BlockQuotaGroup(ctx context.Context, group string, until int64) error {
	if group == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "quota group is required", nil)
	}
	if _, err := h.sql.ExecContext(ctx, `INSERT INTO quota_groups(quota_group, blocked_until, updated_at)
		VALUES(?,?,?)
		ON CONFLICT(quota_group) DO UPDATE SET
			blocked_until = excluded.blocked_until,
			updated_at = excluded.updated_at`,
		group, until, h.clock.NowUnixMilli(),
	); err != nil {
		return fmt.Errorf("sqlite: block quota group %q: %w", group, err)
	}
	return nil
}

// QuotaGroupBlocked reports whether a quota group is still exhausted at now. An
// unknown group is never blocked.
func (h *RouteHealth) QuotaGroupBlocked(ctx context.Context, group string, now int64) (bool, error) {
	if group == "" {
		return false, nil
	}
	var until int64
	err := h.sql.QueryRowContext(ctx,
		`SELECT blocked_until FROM quota_groups WHERE quota_group = ?`, group).Scan(&until)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("sqlite: read quota group %q: %w", group, err)
	}
	return now < until, nil
}

// QuotaGroupBlocks returns every recorded block, for operator status output.
func (h *RouteHealth) QuotaGroupBlocks(ctx context.Context) (map[string]int64, error) {
	rows, err := h.sql.QueryContext(ctx, `SELECT quota_group, blocked_until FROM quota_groups ORDER BY quota_group`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list quota groups: %w", err)
	}
	defer rows.Close()
	blocks := map[string]int64{}
	for rows.Next() {
		var group string
		var until int64
		if err := rows.Scan(&group, &until); err != nil {
			return nil, fmt.Errorf("sqlite: list quota groups: %w", err)
		}
		blocks[group] = until
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list quota groups: %w", err)
	}
	return blocks, nil
}

// ---------------------------------------------------------------------------
// Row codec
// ---------------------------------------------------------------------------

func scanHealthRecord(row rowScanner) (domain.HealthRecord, error) {
	var (
		routeID, accountID, scope              string
		state, failureClass, failureMessage    string
		consecutiveFailures, probeCount        int
		lastFailure, lastSuccess               int64
		nextProbeAt, cooldownUntil, retryAfter int64
		updatedAt                              int64
	)
	if err := row.Scan(&routeID, &accountID, &scope, &state, &failureClass, &failureMessage,
		&consecutiveFailures, &lastFailure, &lastSuccess, &nextProbeAt, &cooldownUntil,
		&retryAfter, &probeCount, &updatedAt); err != nil {
		return domain.HealthRecord{}, err
	}
	route, err := domain.ParseRouteID(routeID)
	if err != nil {
		return domain.HealthRecord{}, decodeFailure("health route id", err)
	}
	healthState := domain.HealthState(state)
	if !domain.IsValidHealthState(healthState) {
		return domain.HealthRecord{}, decodeFailure("health state", errors.New("unknown health state "+state))
	}
	rec := domain.HealthRecord{
		RouteID:             route,
		Scope:               scope,
		State:               healthState,
		FailureClass:        domain.FailureClass(failureClass),
		FailureMessage:      failureMessage,
		ConsecutiveFailures: consecutiveFailures,
		LastFailure:         lastFailure,
		LastSuccess:         lastSuccess,
		NextProbeAt:         nextProbeAt,
		CooldownUntil:       cooldownUntil,
		RetryAfter:          retryAfter,
		ProbeCount:          probeCount,
		UpdatedAt:           updatedAt,
	}
	if accountID != "" {
		account, err := domain.ParseAccountID(accountID)
		if err != nil {
			return domain.HealthRecord{}, decodeFailure("health account id", err)
		}
		rec.AccountID = &account
	}
	return rec, nil
}

// accountKey is the stored column form of a health key's account: route-scope
// records carry no account, and the zero identity's String() would otherwise
// store a bare separator.
func accountKey(account domain.AccountID) string {
	if account.IsZero() {
		return ""
	}
	return account.String()
}

// recordAccountKey is accountKey for a record's optional account reference.
func recordAccountKey(account *domain.AccountID) string {
	if account == nil || account.IsZero() {
		return ""
	}
	return account.String()
}

// routingHealthV3Schema holds the route/account health and quota-group tables.
const routingHealthV3Schema = `
CREATE TABLE route_health (
	route_id TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL,
	state TEXT NOT NULL,
	failure_class TEXT NOT NULL DEFAULT '',
	failure_message TEXT NOT NULL DEFAULT '',
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_failure INTEGER NOT NULL DEFAULT 0,
	last_success INTEGER NOT NULL DEFAULT 0,
	next_probe_at INTEGER NOT NULL DEFAULT 0,
	cooldown_until INTEGER NOT NULL DEFAULT 0,
	retry_after INTEGER NOT NULL DEFAULT 0,
	probe_count INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(route_id, account_id, scope)
);
CREATE INDEX idx_route_health_probe ON route_health(state, next_probe_at);
CREATE INDEX idx_route_health_cooldown ON route_health(state, cooldown_until);

-- Quota-group exhaustion: several accounts can share one quota group (PLAN
-- 8.2), so the block belongs to the group and not to any single account.
CREATE TABLE quota_groups (
	quota_group TEXT PRIMARY KEY,
	blocked_until INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`
