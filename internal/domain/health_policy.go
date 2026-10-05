package domain

// Pure health-failure classification and half-open probe admission (PLAN
// 9.1-9.2). These policies take explicit timestamps and records so callers
// never need a real clock, and they are independent of any storage adapter.

// IsHealthNeutral reports whether a failure class carries no provider-health
// penalty. User cancellation aborts the work but says nothing about the
// route, and a concurrent world conflict is resolved by re-evaluating the
// turn, so neither may cool or quarantine a credential, account, or route
// (PLAN 9.1).
func IsHealthNeutral(class FailureClass) bool {
	return class == FailureUserCancelled || class == FailureWorldConflict
}

// IsRequestScopedFailure reports whether a failure describes the current
// request rather than the general availability of the route (PLAN 9.1):
// context repair, malformed tool arguments, and schema/content rejections. A
// single occurrence must not disable the model; repeated incompatibility of
// the same scope may.
func IsRequestScopedFailure(class FailureClass) bool {
	switch class {
	case FailureContextTooLong, FailureInvalidArguments, FailureInvalidResponse, FailureContentRejected:
		return true
	default:
		return false
	}
}

// ProbeDue reports whether a half-open health record may be probed at now
// (PLAN 9.2). A cooling-down record whose cooldown expired is half-open, as
// is a probe-eligible record whose next-probe time has arrived; every other
// state is closed to probing.
func ProbeDue(rec HealthRecord, now int64) bool {
	switch rec.State {
	case HealthProbeEligible:
		return now >= rec.NextProbeAt
	case HealthCoolingDown:
		return now >= rec.CooldownUntil && now >= rec.NextProbeAt
	default:
		return false
	}
}

// AdmitProbe admits at most one probe for a half-open record and returns the
// record to persist. The second call for the same record returns false because
// the first moved it to probing, which is not half-open: only one probe may
// run for a health key at a time (PLAN 9.2). A cooling-down record first
// becomes probe-eligible, matching the domain transition table.
func AdmitProbe(rec HealthRecord, now int64) (HealthRecord, bool) {
	if !ProbeDue(rec, now) {
		return rec, false
	}
	if rec.State == HealthCoolingDown {
		rec.transitionTo(HealthProbeEligible, now)
	}
	rec.RecordProbeStart(now)
	// ProbeDue guarantees the record was half-open, and RecordProbeStart
	// moves a half-open record to probing, so the probe is admitted.
	return rec, rec.State == HealthProbing
}

// SelectableForNewSession reports whether a health record is usable for a new
// session or a failover attempt at now (PLAN 8.4, 9.2). Healthy routes are
// usable, and a half-open route may be tried during a real request; closed and
// quarantined records are not.
func SelectableForNewSession(rec HealthRecord, now int64) bool {
	return rec.IsHealthy(now)
}
