package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// maxSurrounding bounds one Surrounding window on each side so a model
// request for "everything" stays a bounded context slice.
const maxSurrounding = 100

// Query searches events under the given scope/filter with pagination. The
// effective visibility set is the requested scopes intersected with what the
// injected policy allows, and every explicitly requested forbidden scope
// fails the query outright: sharing-off denies cross-user and shared reads at
// this boundary regardless of what the model asked for.
func (e *Events) Query(ctx context.Context, q domain.RetrievalQuery, policy domain.ScopePolicy) (domain.RetrievalResult, error) {
	if err := q.Pagination.Validate(); err != nil {
		return domain.RetrievalResult{}, domain.NewValidationError(domain.CodeInvalidInput, "invalid pagination", map[string]string{"reason": err.Error()})
	}
	if err := checkQueryScopes(q, policy); err != nil {
		return domain.RetrievalResult{}, err
	}
	allowed := allowedScopes(policy)
	requested := map[string]bool{}
	for _, s := range q.Scope.Scopes {
		requested[s.String()] = true
	}
	if q.Scope.IncludeShared {
		requested[domain.ScopeShared.String()] = true
	}
	if len(requested) == 0 {
		for s := range allowed {
			requested[s] = true
		}
	}
	effective := []string{}
	for scope := range requested {
		if allowed[scope] {
			effective = append(effective, scope)
		}
	}

	where, args := []string{"1 = 1"}, []any{}
	placeholders := make([]string, 0, len(effective))
	for _, scope := range effective {
		placeholders = append(placeholders, "?")
		args = append(args, scope)
	}
	where = append(where, "scope IN ("+strings.Join(placeholders, ",")+")")
	if len(q.Scope.SessionIDs) > 0 {
		ps := make([]string, 0, len(q.Scope.SessionIDs))
		for _, id := range q.Scope.SessionIDs {
			ps = append(ps, "?")
			args = append(args, id.String())
		}
		where = append(where, "session_id IN ("+strings.Join(ps, ",")+")")
	}
	if len(q.Scope.UserIDs) > 0 {
		ps := make([]string, 0, len(q.Scope.UserIDs))
		for _, id := range q.Scope.UserIDs {
			ps = append(ps, "?")
			args = append(args, id.String())
		}
		where = append(where, "owner_user IN ("+strings.Join(ps, ",")+")")
	}
	f := q.Filter
	if len(f.Kinds) > 0 {
		ps := make([]string, 0, len(f.Kinds))
		for _, k := range f.Kinds {
			ps = append(ps, "?")
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(ps, ",")+")")
	}
	if f.TurnID != nil {
		where = append(where, "turn_id = ?")
		args = append(args, f.TurnID.String())
	}
	if f.AttemptID != nil {
		where = append(where, "attempt_id = ?")
		args = append(args, f.AttemptID.String())
	}
	if f.AppVersionID != nil {
		where = append(where, "app_version_id = ?")
		args = append(args, f.AppVersionID.String())
	}
	if f.FromTime != 0 {
		where = append(where, "timestamp >= ?")
		args = append(args, f.FromTime)
	}
	if f.ToTime != 0 {
		where = append(where, "timestamp < ?")
		args = append(args, f.ToTime)
	}
	if f.MinSequence != 0 {
		where = append(where, "sequence >= ?")
		args = append(args, f.MinSequence)
	}
	if f.MaxSequence != 0 {
		where = append(where, "sequence <= ?")
		args = append(args, f.MaxSequence)
	}
	if f.ProvenanceSource != "" {
		where = append(where, "provenance_source = ?")
		args = append(args, f.ProvenanceSource)
	}
	cursorID, err := decodeCursor(q.Pagination.Cursor)
	if err != nil {
		return domain.RetrievalResult{}, err
	}
	order := "ASC"
	comparison := ">"
	if q.Pagination.Descending {
		order = "DESC"
		comparison = "<"
	}
	if q.Pagination.Cursor != "" {
		where = append(where, "rowid "+comparison+" ?")
		args = append(args, cursorID)
	}

	limit := q.Pagination.Limit
	args = append(args, limit+1)
	query := fmt.Sprintf(`SELECT e.envelope, e.payload_inline, e.rowid FROM events e WHERE %s ORDER BY e.rowid %s LIMIT ?`,
		strings.Join(where, " AND "), order)
	rows, err := e.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return domain.RetrievalResult{}, fmt.Errorf("sqlite: retrieval query: %w", err)
	}
	defer rows.Close()
	type hit struct {
		rec   domain.EventRecord
		rowID int64
	}
	var hits []hit
	for rows.Next() {
		var envelopeJSON string
		var inline sql.NullString
		var rowID int64
		if err := rows.Scan(&envelopeJSON, &inline, &rowID); err != nil {
			return domain.RetrievalResult{}, err
		}
		rec, err := recordFromRow(envelopeJSON, inline)
		if err != nil {
			return domain.RetrievalResult{}, err
		}
		hits = append(hits, hit{rec: rec, rowID: rowID})
	}
	if err := rows.Err(); err != nil {
		return domain.RetrievalResult{}, err
	}
	res := domain.RetrievalResult{Scope: q.Scope, Filter: f, QueriedAt: time.Now().UnixMilli()}
	if len(hits) > limit {
		res.Truncated = true
		hits = hits[:limit]
		res.NextCursor = encodeCursor(hits[len(hits)-1].rowID)
	}
	for _, h := range hits {
		res.Events = append(res.Events, h.rec)
	}
	return res, nil
}

// Surrounding returns up to before events preceding ref and up to after
// events following it within the same session, all under the same scope
// authorization. Each returned event is provenance-carrying via its envelope.
func (e *Events) Surrounding(ctx context.Context, ref domain.EventReference, before, after int, policy domain.ScopePolicy) ([]domain.EventRecord, error) {
	if before < 0 || after < 0 || before > maxSurrounding || after > maxSurrounding {
		return nil, domain.NewLimitError(domain.CodeContextTooLong, "surrounding window out of range [0,100]", map[string]string{"before": fmt.Sprint(before), "after": fmt.Sprint(after)})
	}
	var sessionID string
	var sequence uint64
	var scope string
	err := e.sql.QueryRowContext(ctx,
		`SELECT session_id, sequence, scope FROM events WHERE event_id = ?`, ref.EventID.String()).Scan(&sessionID, &sequence, &scope)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, domain.NewNotFoundError(domain.CodeEventNotFound, "event not found", map[string]string{"event_id": ref.EventID.String()})
		}
		return nil, fmt.Errorf("sqlite: locate event: %w", err)
	}
	allowed := allowedScopes(policy)
	if !allowed[scope] {
		return nil, fmt.Errorf("%w: surrounding access to scope %q denied", domain.ErrRetrievalDenied, scope)
	}
	scopeList := []string{}
	for s := range allowed {
		scopeList = append(scopeList, s)
	}
	scopePlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(scopeList)), ",")
	scopeArgs := make([]any, 0, len(scopeList))
	for _, s := range scopeList {
		scopeArgs = append(scopeArgs, s)
	}

	beforeRecords, err := e.querySessionNeighbors(ctx, sessionID, sequence, "<", "DESC", before, scopePlaceholders, scopeArgs)
	if err != nil {
		return nil, err
	}
	// restore chronological order within the returned window
	for i, j := 0, len(beforeRecords)-1; i < j; i, j = i+1, j-1 {
		beforeRecords[i], beforeRecords[j] = beforeRecords[j], beforeRecords[i]
	}
	afterRecords, err := e.querySessionNeighbors(ctx, sessionID, sequence, ">", "ASC", after, scopePlaceholders, scopeArgs)
	if err != nil {
		return nil, err
	}
	return append(beforeRecords, afterRecords...), nil
}

func (e *Events) querySessionNeighbors(ctx context.Context, sessionID string, sequence uint64, cmp, order string, limit int, scopePlaceholders string, scopeArgs []any) ([]domain.EventRecord, error) {
	if limit == 0 {
		return nil, nil
	}
	args := append([]any{sessionID, sequence}, scopeArgs...)
	args = append(args, limit)
	rows, err := e.sql.QueryContext(ctx,
		fmt.Sprintf(`SELECT envelope, payload_inline FROM events WHERE session_id = ? AND sequence %s ? AND scope IN (%s) ORDER BY sequence %s LIMIT ?`, cmp, scopePlaceholders, order), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: surrounding query: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}
