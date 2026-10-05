package application

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// The event kinds and payloads the journal appends are owned by the domain
// vocabulary in internal/domain/events.go, which the event store validates every
// append against: a kind or payload declared here would be rejected on write.

// journalRefs are the correlation references one appended event carries.
type journalRefs struct {
	Turn     *domain.TurnID
	Attempt  *domain.AttemptID
	Revision domain.Revision
}

// eventJournal appends one session's research events. It is the only writer of
// the session-local sequence, so ordering is correct without a lock held across
// a store call on behalf of every caller.
type eventJournal struct {
	store   ports.EventStore
	content ports.ContentStore
	clock   ports.Clock

	// budget bounds one append so a stalled store cannot wedge a session.
	budget time.Duration
	// inline is the largest payload kept inline; larger payloads become a
	// content reference.
	inline int

	session domain.SessionID
	prov    domain.Provenance

	mu       sync.Mutex
	sequence uint64
}

// newEventJournal starts a journal for one accepted session.
func newEventJournal(store ports.EventStore, content ports.ContentStore, clock ports.Clock, budget time.Duration, inline int, session domain.SessionID, prov domain.Provenance) *eventJournal {
	return &eventJournal{
		store:   store,
		content: content,
		clock:   clock,
		budget:  budget,
		inline:  inline,
		session: session,
		prov:    prov,
	}
}

// append stores one event and returns its assigned record.
//
// Journal writes deliberately ignore turn cancellation: the research record has
// to keep the turns that were cancelled, interrupted, or discarded. They are
// bounded by the journal timeout instead, and a failure marks the session
// unhealthy so it stops accepting semantic work instead of running unrecorded.
func (j *eventJournal) append(ctx context.Context, kind domain.EventKind, payload domain.EventPayload, refs journalRefs) (domain.EventRecord, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return domain.EventRecord{}, domain.NewInternalError(domain.CodeSerializationFailed, "event payload is not serializable", err)
	}
	payloadRef, err := j.payloadRef(ctx, body)
	if err != nil {
		return domain.EventRecord{}, err
	}

	j.mu.Lock()
	j.sequence++
	sequence := j.sequence
	prov := j.prov
	j.mu.Unlock()

	envelope := domain.NewEventEnvelope(
		j.session,
		sequence,
		kind,
		prov,
		j.clock.NowUnixMilli(),
		j.clock.MonotonicNanos(),
	)
	envelope.Payload = payloadRef
	envelope.TurnID = refs.Turn
	envelope.AttemptID = refs.Attempt
	envelope.NodeRevision = refs.Revision

	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.budget)
	defer cancel()

	return j.store.Append(appendCtx, envelope)
}

// payloadRef inlines a small payload and moves a large one to the content store,
// so one large frame cannot bloat the event stream.
func (j *eventJournal) payloadRef(ctx context.Context, body []byte) (domain.PayloadRef, error) {
	if len(body) <= j.inline {
		return domain.PayloadRef{Inline: body}, nil
	}
	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.budget)
	defer cancel()
	ref, err := j.content.Put(putCtx, body, "application/json")
	if err != nil {
		return domain.PayloadRef{}, domain.NewInternalError(domain.CodeStorageFull, "large event payload could not be stored", err)
	}
	return domain.PayloadRef{ContentID: &ref.Hash}, nil
}
