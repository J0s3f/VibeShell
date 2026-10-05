package simulation

import (
	"context"
	"sort"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// ContextRequest describes one context assembly: the trusted session state,
// the selected model's limits, and the injected scope policy.
type ContextRequest struct {
	SessionID         domain.SessionID
	UserID            domain.UserID
	CWD               domain.ValidPath
	Env               map[string]string
	ActiveInteraction *domain.AppView
	Policy            domain.ScopePolicy
	PromptVersion     string
	ModelRoute        domain.RouteID
	ContextLimit      int // tokens
	ReserveTokens     int // tokens reserved for tool results and the final response
	RecentLimit       int // raw events fetched per scope bucket
	NowUnixMilli      int64
}

// ContextHeader carries the transport-independent identity and environment
// facts every assembled context starts with (PLAN 7.3): prompt identity,
// true session/user identifiers, cwd/environment, active interaction, and
// the latest relevant state revisions.
type ContextHeader struct {
	SessionID         domain.SessionID  `json:"session_id"`
	UserID            domain.UserID     `json:"user_id"`
	PromptVersion     string            `json:"prompt_version"`
	ModelRoute        domain.RouteID    `json:"model_route"`
	CWD               domain.ValidPath  `json:"cwd"`
	Env               map[string]string `json:"env,omitempty"`
	ActiveInteraction *domain.AppView   `json:"active_interaction,omitempty"`
	RelevantRevisions []domain.Revision `json:"relevant_revisions,omitempty"`
	PolicyRevision    int64             `json:"policy_revision"`
	AssembledAt       int64             `json:"assembled_at"`
}

// ContextSummary is a derived summary of older history: the summary record
// with its provenance reference plus the summary text itself.
type ContextSummary struct {
	Ref  domain.SummaryRef `json:"ref"`
	Text string            `json:"text"`
}

// AssembledContext is the bounded model context for one turn: raw recent
// events, derived summaries for older history, and the budget accounting.
type AssembledContext struct {
	Header         ContextHeader        `json:"header"`
	Events         []domain.EventRecord `json:"events"`
	Summaries      []ContextSummary     `json:"summaries"`
	TotalTokens    int                  `json:"total_tokens"`
	Truncated      bool                 `json:"truncated"`
	MaxTokens      int                  `json:"max_tokens"`
	ReservedTokens int                  `json:"reserved_tokens"`
}

// ContextAssembler builds bounded model context from the canonical
// provider-neutral history. Raw events are never deleted or rewritten:
// compaction derives summary records that reference their source ranges.
type ContextAssembler struct {
	Retrieval  ports.RetrievalStore
	Summarizer Summarizer
	Redactor   SecretRedactor
}

// NewContextAssembler builds the assembler over the given ports. History is
// read exclusively through the scoped retrieval port so the injected policy
// governs every bucket, including the caller's own session tail.
func NewContextAssembler(retrieval ports.RetrievalStore, summarizer Summarizer, redactor SecretRedactor) *ContextAssembler {
	return &ContextAssembler{
		Retrieval:  retrieval,
		Summarizer: summarizer,
		Redactor:   redactor,
	}
}

// Assemble builds the context window for one turn. Budget arithmetic:
// available = ContextLimit - ReserveTokens. Scope buckets are filled in
// priority order (session, then user, then shared) newest-first; whatever
// does not fit is summarized per scope with source ranges and provenance.
// A disabled sharing policy keeps the shared bucket empty, so an old shared
// summary can never re-enter through a cache or convenience prompt.
func (a *ContextAssembler) Assemble(ctx context.Context, req ContextRequest) (*AssembledContext, error) {
	if req.ContextLimit <= 0 {
		return nil, domain.NewValidationError(CodeContextInvalid, "context_limit must be positive", nil)
	}
	if req.ReserveTokens < 0 {
		return nil, domain.NewValidationError(CodeContextInvalid, "reserve_tokens cannot be negative", nil)
	}
	if req.ReserveTokens >= req.ContextLimit {
		return nil, domain.NewValidationError(
			CodeContextInvalid, "reserve_tokens must be smaller than context_limit", nil)
	}
	recentLimit := req.RecentLimit
	if recentLimit <= 0 {
		recentLimit = DefaultRecentLimit
	}
	available := req.ContextLimit - req.ReserveTokens

	sessionEvents, err := a.recentSessionEvents(ctx, req, recentLimit)
	if err != nil {
		return nil, err
	}
	userEvents, err := a.recentScopeEvents(ctx, req, domain.ScopeUser, recentLimit)
	if err != nil {
		return nil, err
	}
	var sharedEvents []domain.EventRecord
	if req.Policy.SharingEnabled {
		sharedEvents, err = a.recentScopeEvents(ctx, req, domain.ScopeShared, recentLimit)
		if err != nil {
			return nil, err
		}
	}

	type bucket struct {
		scope  domain.Scope
		events []domain.EventRecord // newest first
	}
	buckets := []bucket{
		{scope: domain.ScopeSession, events: sessionEvents},
		{scope: domain.ScopeUser, events: userEvents},
		{scope: domain.ScopeShared, events: sharedEvents},
	}

	var included []domain.EventRecord
	var summaries []ContextSummary
	total := 0
	truncated := false
	seenRevisions := map[domain.Revision]bool{}
	var revisions []domain.Revision

	for i := range buckets {
		b := &buckets[i]
		if len(b.events) == 0 {
			continue
		}
		used, fit := 0, 0
		for _, ev := range b.events {
			t := estimateEventTokens(ev)
			if total+used+t > available {
				break
			}
			used += t
			fit++
		}
		if fit < len(b.events) {
			truncated = true
			older := reversedCopy(b.events[fit:]) // oldest first for the summary
			summary, err := a.summarize(ctx, req, b.scope, older)
			if err != nil {
				return nil, err
			}
			// Include the derived summary only if it still fits inside the
			// available budget; otherwise the older events stay dropped and
			// the context is marked truncated rather than over budget.
			if total+used+summary.Ref.TokenCount <= available {
				summaries = append(summaries, *summary)
				total += summary.Ref.TokenCount
			}
			b.events = b.events[:fit]
		}
		for _, ev := range b.events {
			redacted, drop := a.Redactor.RedactJSON(ev.Payload)
			if drop {
				truncated = true
				continue
			}
			ev.Payload = redacted
			included = append(included, ev)
			total += estimateEventTokens(ev)
			rev := ev.Envelope.NodeRevision
			if rev != 0 && !seenRevisions[rev] && len(revisions) < MaxRelevantRevisions {
				seenRevisions[rev] = true
				revisions = append(revisions, rev)
			}
		}
	}

	return &AssembledContext{
		Header: ContextHeader{
			SessionID:         req.SessionID,
			UserID:            req.UserID,
			PromptVersion:     req.PromptVersion,
			ModelRoute:        req.ModelRoute,
			CWD:               req.CWD,
			Env:               boundedEnv(req.Env),
			ActiveInteraction: req.ActiveInteraction,
			RelevantRevisions: revisions,
			PolicyRevision:    req.Policy.PolicyRevision,
			AssembledAt:       req.NowUnixMilli,
		},
		Events:         included,
		Summaries:      summaries,
		TotalTokens:    total,
		Truncated:      truncated,
		MaxTokens:      req.ContextLimit,
		ReservedTokens: req.ReserveTokens,
	}, nil
}

// recentSessionEvents fetches the caller's canonical turn history: the
// newest events of the session, under the injected policy. The session scope
// is always caller-local, so the policy authorizes it unconditionally.
func (a *ContextAssembler) recentSessionEvents(ctx context.Context, req ContextRequest, limit int) ([]domain.EventRecord, error) {
	res, err := a.Retrieval.Query(ctx, domain.RetrievalQuery{
		Scope: domain.RetrievalScope{
			Scopes:     []domain.Scope{domain.ScopeSession},
			SessionIDs: []domain.SessionID{req.SessionID},
		},
		Pagination: domain.Pagination{Limit: limit, Descending: true},
	}, req.Policy)
	if err != nil {
		return nil, err
	}
	return res.Events, nil
}

// recentScopeEvents fetches recent events in a non-session scope under the
// injected policy.
func (a *ContextAssembler) recentScopeEvents(ctx context.Context, req ContextRequest, scope domain.Scope, limit int) ([]domain.EventRecord, error) {
	rs := domain.RetrievalScope{Scopes: []domain.Scope{scope}}
	switch scope {
	case domain.ScopeUser:
		rs.UserIDs = []domain.UserID{req.UserID}
	case domain.ScopeShared:
		rs.IncludeShared = true
	}
	res, err := a.Retrieval.Query(ctx, domain.RetrievalQuery{
		Scope:      rs,
		Pagination: domain.Pagination{Limit: limit, Descending: true},
	}, req.Policy)
	if err != nil {
		return nil, err
	}
	return res.Events, nil
}

// summarize derives one summary record for a scope's older history.
func (a *ContextAssembler) summarize(ctx context.Context, req ContextRequest, scope domain.Scope, older []domain.EventRecord) (*ContextSummary, error) {
	record, err := a.Summarizer.Summarize(ctx, SummaryRequest{
		Events:        older,
		Scope:         scope,
		ModelRoute:    req.ModelRoute,
		PromptVersion: req.PromptVersion,
		NowUnixMilli:  req.NowUnixMilli,
	})
	if err != nil {
		return nil, err
	}
	return &ContextSummary{
		Ref: domain.SummaryRef{
			SummaryID:    record.SummaryID,
			Scope:        record.Scope,
			SourceEvents: record.SourceEvents,
			TokenCount:   record.TokenCount,
			ModelRoute:   record.ModelRoute,
			CreatedAt:    record.CreatedAt,
		},
		Text: record.Text,
	}, nil
}

// boundedEnv bounds the environment block: at most MaxEnvEntries entries
// with sorted keys for determinism, each value truncated.
func boundedEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, MaxEnvEntries)
	for _, k := range keys {
		if len(out) >= MaxEnvEntries {
			break
		}
		v := env[k]
		if len(v) > MaxEnvValueBytes {
			v = v[:MaxEnvValueBytes]
		}
		out[k] = v
	}
	return out
}
