package apps

import (
	"context"
	"errors"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
)

// StateMigration is an additive, forward-only state
// transform from one version of an application to its
// successor along the version chain. Migrations never
// discard data: they add fields or copy values, and the
// previous durable state is retained untouched when no
// verified migration exists (PLAN 5.6).
type StateMigration struct {
	FromVersion domain.AppVersionID
	ToVersion   domain.AppVersionID
	Description string
	Migrate     func(domain.AppState) (domain.AppState, error)
}

// ErrStateIncompatible reports that a target version
// cannot interpret existing state and no verified
// migration exists. Callers keep the previous version for
// the active session (or start fresh state for a new
// session) instead of silently discarding saved data.
var ErrStateIncompatible = errors.New("app state is incompatible with the target version")

// stateIncompatibleCode is the domain error code carried
// by every state-compatibility failure.
const stateIncompatibleCode = "state_incompatible"

// IsStateIncompatible reports whether err is (or wraps) a
// state-compatibility failure.
func IsStateIncompatible(err error) bool {
	var de *domain.DomainError
	if errors.As(err, &de) {
		return de.Code == stateIncompatibleCode
	}
	return errors.Is(err, ErrStateIncompatible)
}

// RegisterMigration records an additive migration for an
// application version chain. The chain link must already
// exist: ToVersion's parent must be FromVersion, so
// migrations and artifacts stay consistent.
func (s *Service) RegisterMigration(ctx context.Context, migration StateMigration) error {
	if migration.FromVersion.IsZero() || migration.ToVersion.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidInput,
			"migration endpoints are required",
			nil,
		)
	}
	if migration.Migrate == nil {
		return domain.NewValidationError(
			domain.CodeInvalidInput,
			"migration function is required",
			map[string]string{"from": migration.FromVersion.String(), "to": migration.ToVersion.String()},
		)
	}
	// Both endpoints must be registered versions; an unknown
	// endpoint is a not-found, not a chain error.
	if _, err := s.registry.GetArtifact(ctx, migration.FromVersion); err != nil {
		return err
	}
	artifact, err := s.registry.GetArtifact(ctx, migration.ToVersion)
	if err != nil {
		return err
	}
	if artifact.ParentVersion == nil || *artifact.ParentVersion != migration.FromVersion {
		return domain.NewValidationError(
			domain.CodeInvalidInput,
			"migration does not follow the version chain",
			map[string]string{
				"from": migration.FromVersion.String(),
				"to":   migration.ToVersion.String(),
			},
		)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.migrations == nil {
		s.migrations = make(map[domain.AppID]map[domain.AppVersionID]StateMigration)
	}
	perApp, ok := s.migrations[artifact.AppID]
	if !ok {
		perApp = make(map[domain.AppVersionID]StateMigration)
		s.migrations[artifact.AppID] = perApp
	}
	perApp[migration.FromVersion] = migration
	return nil
}

// migrateState walks the version chain from `from` to `to`
// and applies every registered additive migration in order.
// The walk follows the artifacts' parent links, so only a
// linear descendant of `from` can be reached. A missing
// migration or a failed transform returns an
// ErrStateIncompatible error; the caller then retains the
// old version for the session.
func (s *Service) migrateState(
	ctx context.Context,
	app domain.AppID,
	from, to domain.AppVersionID,
	state domain.AppState,
) (domain.AppState, error) {
	if from == to {
		return state, nil
	}

	// Collect the chain steps from `to` back to `from`.
	steps := []domain.AppVersionID{}
	visited := map[domain.AppVersionID]bool{to: true}
	current := to
	for current != from {
		artifact, err := s.registry.GetArtifact(ctx, current)
		if err != nil {
			return domain.AppState{}, stateIncompatible(
				"version %s is not registered", current)
		}
		if artifact.AppID != app {
			return domain.AppState{}, stateIncompatible(
				"version %s belongs to a different app", current)
		}
		if artifact.ParentVersion == nil {
			return domain.AppState{}, stateIncompatible(
				"version %s is not a descendant of %s", to, from)
		}
		parent := *artifact.ParentVersion
		if visited[parent] {
			return domain.AppState{}, stateIncompatible(
				"version chain contains a cycle at %s", parent)
		}
		visited[parent] = true
		steps = append(steps, parent)
		current = parent
	}

	// Apply migrations from `from` forward to `to`.
	s.mu.Lock()
	perApp := s.migrations[app]
	s.mu.Unlock()
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		var next domain.AppVersionID
		if i == 0 {
			next = to
		} else {
			next = steps[i-1]
		}
		migration, ok := perApp[step]
		if !ok || migration.ToVersion != next {
			return domain.AppState{}, stateIncompatible(
				"no additive migration from %s to %s", step, next)
		}
		migrated, err := migration.Migrate(state)
		if err != nil {
			return domain.AppState{}, stateIncompatible(
				"migration %s to %s failed: %v", step, next, err)
		}
		state = migrated
	}
	return state, nil
}

// verifyState runs the candidate artifact against a state
// in the sandbox: the version must be able to interpret
// the state it inherits. This is the "cannot interpret
// existing state" gate that triggers safe fallback.
func (s *Service) verifyState(
	ctx context.Context,
	artifact domain.AppArtifact,
	state domain.AppState,
) error {
	limits := stagingLimits
	limits.SimNowMilli = s.clock.NowUnixMilli()
	event := domain.AppEvent{
		EventType: domain.AppEventInput,
		Payload:   []byte(`{"probe":"state_compatibility"}`),
		Timestamp: limits.SimNowMilli,
	}
	result, err := s.sandbox.Run(ctx, artifact, state, event, limits)
	if err != nil {
		return stateIncompatible("version cannot interpret existing state: %v", err)
	}
	if err := domain.ValidateResult(result); err != nil {
		return stateIncompatible("version returned an invalid result for existing state: %v", err)
	}
	return nil
}

func stateIncompatible(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return domain.WrapError(
		ErrStateIncompatible,
		domain.CategoryValidation,
		stateIncompatibleCode,
		message,
	)
}
