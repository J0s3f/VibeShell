package simulation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// ---------------------------------------------------------------------------
// history.search
// ---------------------------------------------------------------------------

type historySearchArgs struct {
	Query    string `json:"query,omitempty"`
	Match    string `json:"match,omitempty"` // exact_command | path | text
	Kind     string `json:"kind,omitempty"`
	Scope    string `json:"scope,omitempty"`
	FromTime int64  `json:"from_time,omitempty"`
	ToTime   int64  `json:"to_time,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

// historyMatch is one search hit with the provenance the agent needs to
// distinguish an old example from current truth (PLAN 7.3): scope, time,
// session, source command, cwd, and state version.
type historyMatch struct {
	EventID      domain.EventID   `json:"event_id"`
	SessionID    domain.SessionID `json:"session_id"`
	Sequence     uint64           `json:"sequence"`
	Kind         domain.EventKind `json:"kind"`
	Timestamp    int64            `json:"timestamp"`
	Scope        domain.Scope     `json:"scope"`
	Command      string           `json:"command,omitempty"`
	CWD          string           `json:"cwd,omitempty"`
	StateVersion domain.Revision  `json:"state_version,omitempty"`
	MatchType    string           `json:"match_type"`
	Excerpt      string           `json:"excerpt,omitempty"`
}

type historySearchResult struct {
	Matches    []historyMatch `json:"matches"`
	Truncated  bool           `json:"truncated"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// searchScope builds the trusted retrieval scope for a history search. The
// caller's session and user scopes are always eligible; the shared scope
// joins only when the injected policy enables sharing. A model request can
// narrow the scope but never widen it, and naming the shared scope while
// sharing is disabled is a denial.
func (r *Registry) searchScope(call CallContext, name string) (domain.RetrievalScope, error) {
	switch name {
	case "", "session", "user", "shared":
	default:
		return domain.RetrievalScope{}, domain.NewValidationError(
			domain.CodeInvalidInput, fmt.Sprintf("unknown scope %q", name), nil)
	}
	rs := domain.RetrievalScope{}
	includeSession := name == "" || name == "session"
	includeUser := name == "" || name == "user"
	includeShared := name == "" || name == "shared"
	if name == "shared" && !call.Policy.SharingEnabled {
		return domain.RetrievalScope{}, domain.NewDeniedError(
			domain.CodeSharingDisabled, "shared history search denied while sharing is disabled", nil)
	}
	if !call.Policy.SharingEnabled {
		includeShared = false
	}
	if includeSession {
		rs.Scopes = append(rs.Scopes, domain.ScopeSession)
		rs.SessionIDs = append(rs.SessionIDs, call.SessionID)
	}
	if includeUser {
		rs.Scopes = append(rs.Scopes, domain.ScopeUser)
		rs.UserIDs = append(rs.UserIDs, call.UserID)
	}
	if includeShared {
		rs.Scopes = append(rs.Scopes, domain.ScopeShared)
		rs.IncludeShared = true
	}
	return rs, nil
}

func parseEventKind(name string) (domain.EventKind, error) {
	if name == "" {
		return "", nil
	}
	kind := domain.EventKind(name)
	if !domain.IsValidEventKind(kind) {
		return "", domain.NewValidationError(
			domain.CodeInvalidInput, fmt.Sprintf("unknown event kind %q", name), nil)
	}
	return kind, nil
}

func (r *Registry) historySearch(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args historySearchArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	switch args.Match {
	case "", "exact_command", "path", "text":
	default:
		return nil, domain.NewValidationError(
			domain.CodeInvalidInput, fmt.Sprintf("unknown match type %q", args.Match), nil)
	}
	kind, err := parseEventKind(args.Kind)
	if err != nil {
		return nil, err
	}
	rs, err := r.searchScope(call, args.Scope)
	if err != nil {
		return nil, err
	}
	filter := domain.RetrievalFilter{
		FromTime: args.FromTime,
		ToTime:   args.ToTime,
	}
	if kind != "" {
		filter.Kinds = []domain.EventKind{kind}
	}
	res, err := r.deps.Retrieval.Query(ctx, domain.RetrievalQuery{
		Scope:      rs,
		Filter:     filter,
		Pagination: domain.Pagination{Limit: MaxResultLimit, Cursor: args.Cursor, Descending: true},
	}, call.Policy)
	if err != nil {
		return nil, err
	}
	matches := make([]historyMatch, 0, len(res.Events))
	for _, ev := range res.Events {
		match, ok, err := r.matchEvent(ev, args)
		if err != nil {
			return nil, err
		}
		if ok {
			matches = append(matches, match)
		}
	}
	// Fine-grained matching happens after the bounded scoped scan, so the
	// model-requested limit is applied to matches, not to the raw page.
	limit := clampLimit(args.Limit, DefaultResultLimit, MaxResultLimit)
	truncated := res.Truncated
	if len(matches) > limit {
		matches = matches[:limit]
		truncated = true
	}
	return marshalResult(historySearchResult{
		Matches:    matches,
		Truncated:  truncated,
		NextCursor: res.NextCursor,
	})
}

// matchEvent applies the fine-grained match filter the retrieval query
// cannot express: exact command equality, normalized path equality, or
// full-text containment. An invalid query path matches nothing.
func (r *Registry) matchEvent(ev domain.EventRecord, args historySearchArgs) (historyMatch, bool, error) {
	envelope := ev.Envelope
	match := historyMatch{
		EventID:      envelope.EventID,
		SessionID:    envelope.SessionID,
		Sequence:     envelope.Sequence,
		Kind:         envelope.Kind,
		Timestamp:    envelope.Timestamp,
		StateVersion: envelope.NodeRevision,
	}
	payload := ev.Payload
	switch args.Match {
	case "exact_command":
		if args.Query == "" || envelope.Kind != domain.EventKindInputAccepted {
			return match, false, nil
		}
		var decoded domain.InputAcceptedPayload
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return match, false, nil
		}
		if decoded.Command != args.Query {
			return match, false, nil
		}
		match.Command = decoded.Command
		match.MatchType = "exact_command"
		match.Excerpt = decoded.Command
	case "path":
		if args.Query == "" {
			return match, false, nil
		}
		want, err := parseQueryPath(args.Query)
		if err != nil {
			return match, false, nil
		}
		var decoded domain.WorldReadPayload
		if envelope.Kind != domain.EventKindWorldRead {
			return match, false, nil
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return match, false, nil
		}
		for _, wp := range decoded.Paths {
			if wp == want {
				match.MatchType = "path"
				match.Excerpt = wp.String()
				break
			}
		}
		if match.MatchType == "" {
			return match, false, nil
		}
	default: // text
		if args.Query != "" && !strings.Contains(string(payload), args.Query) {
			return match, false, nil
		}
		match.MatchType = "text"
		match.Excerpt = excerpt(payload, args.Query, 200)
	}
	match.Excerpt = r.deps.Redactor.RedactText(match.Excerpt)
	return match, true, nil
}

// parseQueryPath normalizes a model-supplied path for matching: a trailing
// slash is insignificant (except for root). Dot segments and other invalid
// forms are rejected rather than cleaned, so a query can never resolve
// outside the simulated namespace.
func parseQueryPath(s string) (domain.ValidPath, error) {
	if s == "" {
		return "", domain.ErrEmptyPath
	}
	trimmed := strings.TrimRight(s, "/")
	if trimmed == "" {
		trimmed = "/"
	}
	return domain.ParsePath(trimmed)
}

// excerpt returns a bounded snippet of a payload, centered on the query
// when present.
func excerpt(payload []byte, query string, maxLen int) string {
	s := strings.TrimSpace(string(payload))
	if len(s) <= maxLen {
		return s
	}
	if query != "" {
		if idx := strings.Index(s, query); idx >= 0 {
			start := idx - maxLen/2
			if start < 0 {
				start = 0
			}
			end := start + maxLen
			if end > len(s) {
				end = len(s)
			}
			return s[start:end]
		}
	}
	return s[:maxLen]
}

// ---------------------------------------------------------------------------
// history.context
// ---------------------------------------------------------------------------

type historyContextArgs struct {
	EventID domain.EventID `json:"event_id,omitempty"`
	TurnID  domain.TurnID  `json:"turn_id,omitempty"`
	Before  int            `json:"before,omitempty"`
	After   int            `json:"after,omitempty"`
}

type historyContextResult struct {
	Target     *domain.EventRecord  `json:"target,omitempty"`
	Before     []domain.EventRecord `json:"before,omitempty"`
	After      []domain.EventRecord `json:"after,omitempty"`
	TurnEvents []domain.EventRecord `json:"turn_events,omitempty"`
	Truncated  bool                 `json:"truncated"`
}

// history.context fetches the events surrounding a referenced event, or the
// events of a referenced turn. Every record passes the secret redactor: a
// record consisting solely of secret material is dropped, never disclosed.
func (r *Registry) historyContext(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args historyContextArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.EventID.IsZero() && args.TurnID.IsZero() {
		return nil, domain.NewValidationError(
			domain.CodeInvalidInput, "event_id or turn_id is required", nil)
	}
	before := clampLimit(args.Before, DefaultSurrounding, MaxSurrounding)
	after := clampLimit(args.After, DefaultSurrounding, MaxSurrounding)
	res := historyContextResult{}
	if !args.EventID.IsZero() {
		target, err := r.deps.Events.GetByID(ctx, args.EventID)
		if err != nil {
			return nil, err
		}
		// A directly referenced event is caller-local: session-scoped history
		// belongs to its own session. While sharing is disabled, an event
		// recorded in another session must not be readable by ID regardless of
		// how a store reports its scope, so the policy is enforced here at the
		// tool boundary rather than relying on the store (PLAN 10.4).
		if !call.Policy.SharingEnabled && target.Envelope.SessionID != call.SessionID {
			return nil, domain.NewDeniedError(
				domain.CodeSharingDisabled,
				"cross-session event reference denied while sharing is disabled", nil)
		}
		_, drop := r.deps.Redactor.RedactJSON(target.Payload)
		if drop {
			return nil, domain.NewDeniedError(
				CodeRecordUndisclosable, "referenced event is not disclosable", nil)
		}
		res.Target = &target
		ref := domain.EventReference{
			EventID:   target.Envelope.EventID,
			SessionID: target.Envelope.SessionID,
			Sequence:  target.Envelope.Sequence,
			Kind:      target.Envelope.Kind,
			Timestamp: target.Envelope.Timestamp,
		}
		surrounding, err := r.deps.Retrieval.Surrounding(ctx, ref, before, after, call.Policy)
		if err != nil {
			return nil, err
		}
		// The store returns preceding events then following events, both in
		// ascending sequence order; partition by sequence around the target.
		targetSeq := target.Envelope.Sequence
		for _, ev := range surrounding {
			_, drop := r.deps.Redactor.RedactJSON(ev.Payload)
			if drop {
				res.Truncated = true
				continue
			}
			if ev.Envelope.Sequence < targetSeq {
				res.Before = append(res.Before, ev)
			} else {
				res.After = append(res.After, ev)
			}
		}
		if len(res.Before) > before {
			res.Before = res.Before[len(res.Before)-before:]
			res.Truncated = true
		}
		if len(res.After) > after {
			res.After = res.After[:after]
			res.Truncated = true
		}
		return marshalResult(res)
	}
	// Turn reference: return the turn's events under the trusted scope.
	rs, err := r.searchScope(call, "")
	if err != nil {
		return nil, err
	}
	turnID := args.TurnID
	retrieved, err := r.deps.Retrieval.Query(ctx, domain.RetrievalQuery{
		Scope:      rs,
		Filter:     domain.RetrievalFilter{TurnID: &turnID},
		Pagination: domain.Pagination{Limit: MaxResultLimit, Descending: false},
	}, call.Policy)
	if err != nil {
		return nil, err
	}
	for _, ev := range retrieved.Events {
		_, drop := r.deps.Redactor.RedactJSON(ev.Payload)
		if drop {
			res.Truncated = true
			continue
		}
		res.TurnEvents = append(res.TurnEvents, ev)
	}
	res.Truncated = res.Truncated || retrieved.Truncated
	return marshalResult(res)
}
