package apps

import (
	"context"
	"sync"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// InMemoryRegistry satisfies the outbound AppRegistry port.
var _ ports.AppRegistry = (*InMemoryRegistry)(nil)

// InMemoryRegistry is the contract double for the
// ports.AppRegistry port (the B01 persistence adapter).
// It preserves the reference semantics the production
// adapter must implement:
//
//   - Artifacts are immutable once registered; a
//     registered version is never modified except to
//     stamp its activation event.
//   - RegisterCandidate stores a candidate without
//     activating it and assigns no activation.
//   - Activate is atomic and idempotent for the current
//     version, and only a candidate that passed
//     validation can be activated.
//   - Rollback restores the current pointer to any
//     previously accepted version and keeps every
//     version registered.
//
// The double also exposes History so tests can assert
// the activation-event record that the production
// adapter appends to the research event store.
type InMemoryRegistry struct {
	clock ports.Clock

	mu        sync.Mutex
	artifacts map[domain.AppVersionID]domain.AppArtifact
	current   map[domain.AppID]domain.AppVersionID
	accepted  map[domain.AppID]map[domain.AppVersionID]bool
	history   []ActivationRecord
}

// ActivationRecord is one activation or rollback event:
// the registry's durable activation-event log.
type ActivationRecord struct {
	Kind   ActivationKind
	AppID  domain.AppID
	From   *domain.AppVersionID
	To     domain.AppVersionID
	At     int64
	Reason string
}

// ActivationKind distinguishes pointer moves.
type ActivationKind string

const (
	// ActivationActivate records a forward activation.
	ActivationActivate ActivationKind = "activate"
	// ActivationRollback records a rollback.
	ActivationRollback ActivationKind = "rollback"
)

// NewInMemoryRegistry builds the double. The clock stamps
// activation events.
func NewInMemoryRegistry(clock ports.Clock) *InMemoryRegistry {
	return &InMemoryRegistry{
		clock:     clock,
		artifacts: make(map[domain.AppVersionID]domain.AppArtifact),
		current:   make(map[domain.AppID]domain.AppVersionID),
		accepted:  make(map[domain.AppID]map[domain.AppVersionID]bool),
	}
}

// GetArtifact returns the immutable artifact for a
// version ID.
func (r *InMemoryRegistry) GetArtifact(_ context.Context, version domain.AppVersionID) (domain.AppArtifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	artifact, ok := r.artifacts[version]
	if !ok {
		return domain.AppArtifact{}, domain.NewNotFoundError(
			domain.CodeAppVersionNotFound,
			"unknown app version",
			map[string]string{"version": version.String()},
		)
	}
	return artifact, nil
}

// Current returns the active version ID for an app.
func (r *InMemoryRegistry) Current(_ context.Context, app domain.AppID) (domain.AppVersionID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	version, ok := r.current[app]
	if !ok {
		return domain.AppVersionID{}, domain.NewNotFoundError(
			domain.CodeAppNotFound,
			"app has no active version",
			map[string]string{"app": app.String()},
		)
	}
	return version, nil
}

// RegisterCandidate stores a candidate version without
// activating it. The candidate's version ID must be
// unique, its manifest valid, and its source hash must
// match the source; an extension's parent must belong
// to the same app.
func (r *InMemoryRegistry) RegisterCandidate(_ context.Context, artifact domain.AppArtifact) (domain.AppVersionID, error) {
	if issues := validateArtifact(artifact); len(issues) > 0 {
		return domain.AppVersionID{}, invalidCandidateError(issues)
	}
	if artifact.SourceHash != HashSource(artifact.Source) {
		return domain.AppVersionID{}, domain.NewValidationError(
			domain.CodeInvalidAppManifest,
			"source_hash does not match the source",
			map[string]string{"version": artifact.VersionID.String()},
		)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if artifact.VersionID.IsZero() {
		return domain.AppVersionID{}, domain.NewValidationError(
			domain.CodeInvalidIdentity,
			"candidate requires a version ID",
			nil,
		)
	}
	if _, exists := r.artifacts[artifact.VersionID]; exists {
		return domain.AppVersionID{}, domain.NewConflictError(
			domain.CodeDuplicateKey,
			"version ID already registered",
			map[string]string{"version": artifact.VersionID.String()},
		)
	}
	if artifact.ParentVersion != nil {
		parent, ok := r.artifacts[*artifact.ParentVersion]
		if !ok {
			return domain.AppVersionID{}, domain.NewValidationError(
				domain.CodeAppVersionNotFound,
				"parent version is not registered",
				map[string]string{"parent": artifact.ParentVersion.String()},
			)
		}
		if parent.AppID != artifact.AppID {
			return domain.AppVersionID{}, domain.NewValidationError(
				domain.CodeInvalidInput,
				"parent version belongs to a different app",
				map[string]string{
					"parent_app": parent.AppID.String(),
					"app":        artifact.AppID.String(),
				},
			)
		}
	}
	r.artifacts[artifact.VersionID] = artifact
	return artifact.VersionID, nil
}

// Activate atomically moves the current pointer to a
// registered candidate that passed validation.
// Activating the current version is an idempotent
// no-op.
func (r *InMemoryRegistry) Activate(_ context.Context, app domain.AppID, version domain.AppVersionID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	artifact, ok := r.artifacts[version]
	if !ok {
		return domain.NewNotFoundError(
			domain.CodeAppVersionNotFound,
			"unknown app version",
			map[string]string{"version": version.String()},
		)
	}
	if artifact.AppID != app {
		return domain.NewValidationError(
			domain.CodeInvalidInput,
			"version belongs to a different app",
			map[string]string{"version_app": artifact.AppID.String(), "app": app.String()},
		)
	}
	if !artifact.Validation.Passed {
		return domain.NewValidationError(
			"candidate_not_validated",
			"only a candidate that passed validation can be activated",
			map[string]string{
				"version": version.String(),
				"issues":  joinIssues(artifact.Validation.Issues),
			},
		)
	}
	if current, ok := r.current[app]; ok && current == version {
		// Idempotent activation: the pointer already
		// points at this version.
		return nil
	}
	var previous *domain.AppVersionID
	if current, ok := r.current[app]; ok {
		previous = &current
	}
	at := r.clock.NowUnixMilli()
	activated := artifact
	activated.ActivatedAt = at
	r.artifacts[version] = activated
	r.current[app] = version
	if r.accepted[app] == nil {
		r.accepted[app] = make(map[domain.AppVersionID]bool)
	}
	r.accepted[app][version] = true
	r.history = append(r.history, ActivationRecord{
		Kind:  ActivationActivate,
		AppID: app,
		From:  previous,
		To:    version,
		At:    at,
	})
	return nil
}

// Rollback restores the current pointer to a prior
// accepted version.
func (r *InMemoryRegistry) Rollback(_ context.Context, app domain.AppID, to domain.AppVersionID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	artifact, ok := r.artifacts[to]
	if !ok {
		return domain.NewNotFoundError(
			domain.CodeAppVersionNotFound,
			"unknown app version",
			map[string]string{"version": to.String()},
		)
	}
	if artifact.AppID != app {
		return domain.NewValidationError(
			domain.CodeInvalidInput,
			"target version belongs to a different app",
			map[string]string{"target_app": artifact.AppID.String(), "app": app.String()},
		)
	}
	if !r.accepted[app][to] {
		return domain.NewValidationError(
			codeNotAnAcceptedVersion,
			"rollback requires a version that was activated before",
			map[string]string{"target": to.String()},
		)
	}
	current, ok := r.current[app]
	if !ok {
		return domain.NewNotFoundError(
			domain.CodeAppNotFound,
			"app has no active version",
			map[string]string{"app": app.String()},
		)
	}
	if current == to {
		// Rolling back to the current version is an
		// idempotent no-op.
		return nil
	}
	previous := current
	r.current[app] = to
	r.history = append(r.history, ActivationRecord{
		Kind:   ActivationRollback,
		AppID:  app,
		From:   &previous,
		To:     to,
		At:     r.clock.NowUnixMilli(),
		Reason: reason,
	})
	return nil
}

// History returns the activation-event log, oldest
// first. It is part of the double's test surface, not
// the port.
func (r *InMemoryRegistry) History() []ActivationRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ActivationRecord(nil), r.history...)
}
