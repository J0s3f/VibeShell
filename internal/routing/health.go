package routing

import (
	"context"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// routeHealthy reports whether a route record is usable for selection now.
// Absent records are treated as healthy; quarantined records never are;
// cooling records become eligible again once their cooldown expired.
func (r *Router) routeHealthy(ctx context.Context, route domain.RouteID, now int64) bool {
	rec, ok, err := r.health.Get(ctx, domain.HealthKey{RouteID: route, Scope: ScopeRoute})
	if err != nil || !ok {
		return true
	}
	return domain.SelectableForNewSession(rec, now)
}

// accountHealthy reports whether the (route, account) pair is free of
// credential/account-scope problems, and whether the account has at least
// one credential not currently cooling (a single revoked key does not
// strand an account whose remaining keys are usable, and never touches
// unrelated accounts or the route record).
func (r *Router) accountHealthy(ctx context.Context, route domain.RouteID, acc domain.Account, now int64) bool {
	rec, ok, err := r.health.Get(ctx, domain.HealthKey{RouteID: route, AccountID: acc.ID, Scope: ScopeAccount})
	if err == nil && ok && !domain.SelectableForNewSession(rec, now) {
		return false
	}
	cred, ok, err := r.health.Get(ctx, domain.HealthKey{RouteID: route, AccountID: acc.ID, Scope: ScopeCredential})
	if err == nil && ok && !domain.SelectableForNewSession(cred, now) {
		return false
	}
	return true
}

// RunDueProbes admits one bounded probe per due, half-open health key
// (PLAN 9.2): a record in cooling-down whose cooldown expired, or in
// probe-eligible state, with NextProbeAt due. Only one probe runs at a
// time for a key, the probe budget (ProbeBudget) is enforced, and a probe
// success returns the key to healthy while a failure re-cools it. Probes
// are small fixed requests with their own deadline, never unbounded SDK
// retry loops.
func (r *Router) RunDueProbes(ctx context.Context) (int, error) {
	now := r.clock.NowUnixMilli()
	recs, err := r.health.List(ctx)
	if err != nil {
		return 0, err
	}
	ran := 0
	for _, rec := range recs {
		if !domain.ProbeDue(rec, now) {
			continue
		}
		if !r.tryAcquireProbeBudget(now) {
			break
		}
		if err := r.runProbe(ctx, rec, now); err != nil {
			return ran, err
		}
		ran++
	}
	return ran, nil
}

// tryAcquireProbeBudget enforces MaxProbesPerHour from the route policy
// and records the probe start. It returns false when the hourly budget is
// spent.
func (r *Router) tryAcquireProbeBudget(now int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now - 60*60*1000
	kept := r.probeTimestamps[:0]
	for _, t := range r.probeTimestamps {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	r.probeTimestamps = kept
	if r.cfg.Policy.ProbeBudget.MaxProbesPerHour > 0 && len(r.probeTimestamps) >= r.cfg.Policy.ProbeBudget.MaxProbesPerHour {
		return false
	}
	r.probeTimestamps = append(r.probeTimestamps, now)
	return true
}

func (r *Router) runProbe(ctx context.Context, rec domain.HealthRecord, now int64) error {
	// AdmitProbe makes a cooling record whose cooldown expired probe-eligible
	// (cooling_down -> probe_eligible is the one transition out of cooling
	// that does not need an operator) and then moves it to probing, which
	// closes the half-open state so a second probe for the same key is
	// refused. A record that is not half-open is never mutated.
	rec, admitted := domain.AdmitProbe(rec, now)
	if !admitted {
		return nil
	}
	if err := r.health.Save(ctx, rec); err != nil {
		return err
	}

	probeCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(now+ProbeDeadlineMs))
	defer cancel()
	account := domain.AccountID{}
	if rec.AccountID != nil {
		account = *rec.AccountID
	}
	_, err := r.gateway.Request(probeCtx, domain.ModelRequest{
		RouteID:    rec.RouteID,
		AccountID:  account,
		Messages:   []domain.Message{{Role: domain.RoleUser, Content: "probe"}},
		DeadlineMs: now + ProbeDeadlineMs,
		RequestID:  "probe-" + attemptID(rec.ProbeCount).String(),
	})

	fresh, ok, gerr := r.health.Get(ctx, domain.HealthKey{RouteID: rec.RouteID, AccountID: account, Scope: rec.Scope})
	if gerr != nil || !ok {
		fresh = rec
	}
	fresh.RecordProbeResult(err == nil, r.clock.NowUnixMilli())
	return r.health.Save(ctx, fresh)
}
