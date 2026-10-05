package load

import (
	"fmt"
	"sync/atomic"

	"j0s.at/vibeshell/internal/domain"
)

// idMinter produces domain identities for scenarios that act on the world or
// event store directly (the coordinator mints its own session/turn identities).
//
// The values are deterministic — a counter rendered in the domain's base32
// alphabet — so two runs of the same scenario write the same identifiers and a
// receipt can be compared across runs.
type idMinter struct {
	counter atomic.Int64
}

// next returns the next raw identity string for prefix.
func (m *idMinter) next(prefix string) string {
	return fmt.Sprintf("%s_%026d", prefix, m.counter.Add(1))
}

// Turn returns a fresh turn identity.
func (m *idMinter) Turn() domain.TurnID {
	id, err := domain.ParseTurnID(m.next(domain.PrefixTurn))
	if err != nil {
		panic(fmt.Sprintf("load harness: %v", err))
	}
	return id
}

// Attempt returns a fresh attempt identity.
func (m *idMinter) Attempt() domain.AttemptID {
	id, err := domain.ParseAttemptID(m.next(domain.PrefixAttempt))
	if err != nil {
		panic(fmt.Sprintf("load harness: %v", err))
	}
	return id
}

// fixedIdentity renders a valid, deterministic domain identity for prefix. The
// harness uses it for the constant identities a scenario needs (the double's
// route, the soak artifact) so two runs produce byte-identical records.
func fixedIdentity(prefix string, n int) string {
	return fmt.Sprintf("%s_%026d", prefix, n)
}
