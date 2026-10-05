package simulation

import (
	"crypto/sha256"
	"encoding/base32"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// envelopeTokenOverhead approximates the per-event envelope cost (kind, IDs,
// sequence) that accompanies the payload in model context. It keeps budget
// arithmetic monotonic without marshaling the whole record.
const envelopeTokenOverhead = 8

// crockford is the base32 variant used by every domain identity: no padding,
// no I/L/O/U, matching the identity regex in internal/domain.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// encodeID128 renders 16 random bytes as the 26-character Crockford suffix
// of a domain identity.
func encodeID128(b []byte) string {
	return crockford.EncodeToString(b)
}

// newID generates a fresh identity with the given prefix from the injected
// randomness source, so tests stay deterministic.
func newID(prefix string, r ports.Random) (string, error) {
	b, err := r.Bytes(16)
	if err != nil {
		return "", err
	}
	return prefix + "_" + encodeID128(b), nil
}

// estimateTokens approximates a token count from text length using the
// standard four-characters-per-token heuristic. Budget arithmetic only needs
// a stable, monotonic estimate — exact tokenizer counts belong to the
// provider adapters.
func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// estimateEventTokens estimates the token cost of one event record from its
// payload size. The canonical payload is the model-facing content; the
// envelope contributes a small fixed overhead.
func estimateEventTokens(ev domain.EventRecord) int {
	payload := ev.Payload
	if len(payload) == 0 {
		payload = ev.Envelope.Payload.Inline
	}
	return estimateTokens(string(payload)) + envelopeTokenOverhead
}

// deriveSummaryID deterministically names a summary after its source events,
// so re-summarizing the same range yields the same ID without randomness.
func deriveSummaryID(events []domain.EventRecord) string {
	h := sha256.New()
	for _, ev := range events {
		h.Write([]byte(ev.Envelope.EventID.String()))
	}
	return "sum_" + encodeID128(h.Sum(nil)[:16])
}

func eventIDs(events []domain.EventRecord) []domain.EventID {
	ids := make([]domain.EventID, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.Envelope.EventID)
	}
	return ids
}

// eventRange describes the per-session sequence span covered by a slice of
// events ordered oldest first.
func eventRange(events []domain.EventRecord) *EventRange {
	if len(events) == 0 {
		return nil
	}
	first := events[0].Envelope
	last := events[len(events)-1].Envelope
	return &EventRange{SessionID: first.SessionID, FromSeq: first.Sequence, ToSeq: last.Sequence}
}

func reversedCopy(events []domain.EventRecord) []domain.EventRecord {
	out := make([]domain.EventRecord, len(events))
	for i, ev := range events {
		out[len(events)-1-i] = ev
	}
	return out
}
