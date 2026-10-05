package apps

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// ErrGenerationLost reports that a concurrent generation of the
// same application activated a different version first. Exactly
// one generation wins: the loser's candidate stays registered
// for research but never becomes current.
var ErrGenerationLost = errors.New("concurrent generation lost")

const generationLostCode = "generation_lost"

// codeNotAnAcceptedVersion is the domain error code carried
// when a caller names a version that was never activated, and
// so may not become current or run.
const codeNotAnAcceptedVersion = "not_an_accepted_version"

// IsGenerationLost reports whether err is (or wraps) a lost
// generation race.
func IsGenerationLost(err error) bool {
	var de *domain.DomainError
	if errors.As(err, &de) {
		return de.Code == generationLostCode
	}
	return errors.Is(err, ErrGenerationLost)
}

// Service coordinates the generated-application lifecycle.
// It owns no I/O: artifacts and activation state live in the
// AppRegistry port, JavaScript execution in the AppSandbox
// port, time and randomness in the Clock and Random
// ports, and session pins, app state, and the command index
// in the AppStateStore port. The composition supplies the
// durable store behind that port; without one the service
// defaults to the in-memory store, which behaves the same
// within a process but does not survive a restart.
type Service struct {
	registry ports.AppRegistry
	sandbox  ports.AppSandbox
	clock    ports.Clock
	random   ports.Random
	store    ports.AppStateStore

	// mu serializes the lifecycle state the service itself
	// coordinates: it is held across guarded activations so
	// a check-then-activate pair is atomic with respect to
	// other generations of the same app, and across store
	// operations so one session's pin/state reads and writes
	// stay ordered. The store owns its own concurrency.
	mu sync.Mutex
	// migrations are the registered state migrations,
	// guarded by mu.
	migrations map[domain.AppID]map[domain.AppVersionID]StateMigration
}

// NewService builds an app registry service over the given
// ports. Registry, sandbox, clock, and random are required;
// store may be nil, which selects the in-memory default so
// tests and unconfigured builds keep their behavior.
func NewService(registry ports.AppRegistry, sandbox ports.AppSandbox, clock ports.Clock, random ports.Random, store ports.AppStateStore) *Service {
	if store == nil {
		store = NewInMemoryStateStore(clock)
	}
	return &Service{
		registry:   registry,
		sandbox:    sandbox,
		clock:      clock,
		random:     random,
		store:      store,
		migrations: make(map[domain.AppID]map[domain.AppVersionID]StateMigration),
	}
}

// RegisterCandidate validates a candidate and stores it as an
// immutable, non-activated artifact. Validation runs the
// candidate once in an isolated staging sandbox against empty
// state; the outcome (including failures) becomes part of the
// artifact so rejected attempts stay in the research record.
// Registration never moves the current pointer: a candidate
// becomes current only through Activate.
func (s *Service) RegisterCandidate(ctx context.Context, req CandidateRequest) (domain.AppVersionID, error) {
	if issues := validateCandidateShape(req); len(issues) > 0 {
		return domain.AppVersionID{}, invalidCandidateError(issues)
	}
	if issues := validateArtifact(domain.AppArtifact{
		Manifest: req.Manifest,
		Source:   req.Source,
		Owner:    req.Owner,
		Scope:    req.Scope,
	}); len(issues) > 0 {
		return domain.AppVersionID{}, invalidCandidateError(issues)
	}
	if req.Scope == domain.ScopeShared && !req.Policy.SharedWriteAllowed {
		return domain.AppVersionID{}, domain.NewDeniedError(
			domain.CodeSharingDisabled,
			"registering a shared app requires an enabled sharing policy",
			nil,
		)
	}
	if req.ParentVersion != nil {
		parent, err := s.registry.GetArtifact(ctx, *req.ParentVersion)
		if err != nil {
			return domain.AppVersionID{}, err
		}
		if !req.AppID.IsZero() && parent.AppID != req.AppID {
			return domain.AppVersionID{}, domain.NewValidationError(
				domain.CodeInvalidInput,
				"parent version belongs to a different app",
				map[string]string{
					"parent_app": parent.AppID.String(),
					"app":        req.AppID.String(),
				},
			)
		}
	}

	appID := req.AppID
	if appID.IsZero() {
		minted, err := NewAppID(s.random)
		if err != nil {
			return domain.AppVersionID{}, err
		}
		appID = minted
	}
	versionID, err := NewAppVersionID(s.random)
	if err != nil {
		return domain.AppVersionID{}, err
	}

	artifact := domain.AppArtifact{
		AppID:         appID,
		VersionID:     versionID,
		ParentVersion: req.ParentVersion,
		Manifest:      req.Manifest,
		Source:        req.Source,
		SourceHash:    HashSource(req.Source),
		Owner:         req.Owner,
		Scope:         req.Scope,
		CreatedAt:     s.clock.NowUnixMilli(),
		Provenance:    req.Provenance,
	}
	artifact.Validation = stageValidation(ctx, s.sandbox, s.clock, artifact)

	return s.registry.RegisterCandidate(ctx, artifact)
}

// ActivateRequest moves an application's current pointer to a
// registered, validated candidate. Base, when set, is the
// current version the caller observed when its generation
// started; if the current version has moved on, activation
// fails with a generation-lost conflict so concurrent
// same-app generations have exactly one winner.
type ActivateRequest struct {
	AppID   domain.AppID
	Version domain.AppVersionID
	Actor   domain.UserID
	Policy  domain.ScopePolicy
	Base    *domain.AppVersionID
}

// Activate atomically moves the current pointer after
// validation. Only the owning user may activate a user-scoped
// app; a shared-scoped app requires an enabled sharing
// policy. Activation is idempotent for the current version.
func (s *Service) Activate(ctx context.Context, req ActivateRequest) (domain.AppVersionID, error) {
	if req.AppID.IsZero() || req.Version.IsZero() || req.Actor.IsZero() {
		return domain.AppVersionID{}, domain.NewValidationError(
			domain.CodeInvalidInput, "app, version, and actor are required", nil)
	}
	artifact, err := s.registry.GetArtifact(ctx, req.Version)
	if err != nil {
		return domain.AppVersionID{}, err
	}
	if artifact.AppID != req.AppID {
		return domain.AppVersionID{}, domain.NewValidationError(
			domain.CodeInvalidInput,
			"version belongs to a different app",
			map[string]string{"version_app": artifact.AppID.String(), "app": req.AppID.String()},
		)
	}
	if err := authorizeMutation(artifact, req.Actor, req.Policy); err != nil {
		return domain.AppVersionID{}, err
	}
	if !artifact.Validation.Passed {
		return domain.AppVersionID{}, domain.NewValidationError(
			"candidate_not_validated",
			"only a candidate that passed staging validation can be activated",
			map[string]string{
				"version": req.Version.String(),
				"issues":  joinIssues(artifact.Validation.Issues),
			},
		)
	}

	// The guard and the activation form one atomic section:
	// two generations racing on the same app cannot both
	// observe the base version and activate.
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Base != nil {
		current, err := s.registry.Current(ctx, req.AppID)
		if err != nil {
			return domain.AppVersionID{}, err
		}
		if current != *req.Base {
			return domain.AppVersionID{}, generationLostError(req.AppID, *req.Base, current)
		}
	}
	if err := s.registry.Activate(ctx, req.AppID, req.Version); err != nil {
		return domain.AppVersionID{}, err
	}
	return req.Version, nil
}

// RollbackRequest restores a prior accepted version.
type RollbackRequest struct {
	AppID  domain.AppID
	Target domain.AppVersionID
	Actor  domain.UserID
	Policy domain.ScopePolicy
	Reason string
}

// Rollback moves the current pointer back to a version that
// was activated before. The rolled-away version stays
// registered and retrievable for research and re-activation.
// Existing sessions keep their pinned versions.
func (s *Service) Rollback(ctx context.Context, req RollbackRequest) (domain.AppVersionID, error) {
	if req.AppID.IsZero() || req.Target.IsZero() || req.Actor.IsZero() {
		return domain.AppVersionID{}, domain.NewValidationError(
			domain.CodeInvalidInput, "app, target, and actor are required", nil)
	}
	if req.Reason == "" {
		return domain.AppVersionID{}, domain.NewValidationError(
			domain.CodeInvalidInput, "rollback requires a reason for the research record", nil)
	}
	artifact, err := s.registry.GetArtifact(ctx, req.Target)
	if err != nil {
		return domain.AppVersionID{}, err
	}
	if artifact.AppID != req.AppID {
		return domain.AppVersionID{}, domain.NewValidationError(
			domain.CodeInvalidInput,
			"target version belongs to a different app",
			map[string]string{"target_app": artifact.AppID.String(), "app": req.AppID.String()},
		)
	}
	if artifact.ActivatedAt == 0 {
		return domain.AppVersionID{}, domain.NewValidationError(
			codeNotAnAcceptedVersion,
			"rollback requires a version that was activated before",
			map[string]string{"target": req.Target.String()},
		)
	}
	if err := authorizeMutation(artifact, req.Actor, req.Policy); err != nil {
		return domain.AppVersionID{}, err
	}
	if err := s.registry.Rollback(ctx, req.AppID, req.Target, req.Reason); err != nil {
		return domain.AppVersionID{}, err
	}
	return req.Target, nil
}

// ResolveRequest asks which version of an application a
// session runs and with what state.
type ResolveRequest struct {
	SessionID domain.SessionID
	UserID    domain.UserID
	AppID     domain.AppID
	Policy    domain.ScopePolicy
}

// ResolvePinnedRequest asks for one specific version of an
// application, rather than the version a session would start
// now: a session that already runs an app keeps running the
// version it started with.
type ResolvePinnedRequest struct {
	SessionID domain.SessionID
	UserID    domain.UserID
	AppID     domain.AppID
	Version   domain.AppVersionID
	Policy    domain.ScopePolicy
}

// Resolution is the outcome of resolving an app for a
// session.
type Resolution struct {
	// Version is the artifact version the session runs.
	Version domain.AppVersionID
	// State is the state for that version.
	State domain.AppState
	// Current is the registry's current version at resolve
	// time.
	Current domain.AppVersionID
	// Stale reports that the session retained an older
	// pinned version because the current version could not
	// interpret its state (safe fallback, PLAN 5.6).
	Stale bool
	// Reason explains a stale resolution; empty otherwise.
	Reason string
}

// ResolveSession returns the version and state a session
// runs. An existing session keeps the version it pinned; a
// new session pins the current version and inherits durable
// user/shared state through verified additive migrations.
// When the current version cannot interpret a session's
// state and no migration verifies, the session retains its
// pinned version and state: the old version stays available
// and nothing is discarded.
func (s *Service) ResolveSession(ctx context.Context, req ResolveRequest) (Resolution, error) {
	if req.SessionID.IsZero() || req.UserID.IsZero() || req.AppID.IsZero() {
		return Resolution{}, domain.NewValidationError(
			domain.CodeInvalidInput, "session, user, and app are required", nil)
	}

	s.mu.Lock()
	pin, hasPin, err := s.store.SessionPin(ctx, req.SessionID, req.AppID)
	s.mu.Unlock()
	if err != nil {
		return Resolution{}, err
	}

	current, err := s.registry.Current(ctx, req.AppID)
	if err != nil {
		return Resolution{}, err
	}
	currentArtifact, err := s.registry.GetArtifact(ctx, current)
	if err != nil {
		return Resolution{}, err
	}

	if hasPin {
		if pin.Version == current {
			return Resolution{Version: pin.Version, State: pin.State, Current: current}, nil
		}
		// The current version moved on. Try a verified
		// migration to the current version; on any failure
		// retain the pinned version and its state.
		migrated, err := s.migrateState(ctx, req.AppID, pin.Version, current, pin.State)
		if err == nil {
			err = s.verifyState(ctx, currentArtifact, migrated)
		}
		if err == nil {
			s.mu.Lock()
			err = s.store.PinSession(ctx, req.SessionID, req.AppID, current, migrated)
			s.mu.Unlock()
			if err != nil {
				return Resolution{}, err
			}
			return Resolution{Version: current, State: migrated, Current: current}, nil
		}
		return Resolution{
			Version: pin.Version,
			State:   pin.State,
			Current: current,
			Stale:   true,
			Reason:  err.Error(),
		}, nil
	}

	// New session: visibility first, then inheritance.
	if err := authorizeRead(currentArtifact, req.UserID, req.Policy); err != nil {
		return Resolution{}, err
	}
	state, err := s.inheritDurableState(ctx, req.UserID, req.AppID, current, currentArtifact)
	if err != nil {
		return Resolution{}, err
	}
	s.mu.Lock()
	err = s.store.PinSession(ctx, req.SessionID, req.AppID, current, state)
	s.mu.Unlock()
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Version: current, State: state, Current: current}, nil
}

// ResolvePinned resolves an explicit app version and the
// session's state for it, without re-pinning to the current
// version. It is what a session uses while an app is
// foreground: every user line runs the version the session
// started with, so an activation that happens in between
// cannot change the program the user is talking to.
//
// The read is authorized against the named artifact exactly as
// a first resolution is, because a session may only run a
// version it is permitted to read. A session without a pin
// gets one for the named version, so the state it inherits is
// durable state migrated to that version and a later
// SaveSessionState records the result against it.
//
// The resolution is never stale: keeping a version while the
// current one moved on is the pinned contract, not a fallback
// from an uninterpretable state, so Stale stays false.
func (s *Service) ResolvePinned(ctx context.Context, req ResolvePinnedRequest) (Resolution, error) {
	if req.SessionID.IsZero() || req.UserID.IsZero() || req.AppID.IsZero() || req.Version.IsZero() {
		return Resolution{}, domain.NewValidationError(
			domain.CodeInvalidInput, "session, user, app, and version are required", nil)
	}

	artifact, err := s.registry.GetArtifact(ctx, req.Version)
	if err != nil {
		return Resolution{}, err
	}
	if artifact.AppID != req.AppID {
		return Resolution{}, domain.NewValidationError(
			domain.CodeInvalidInput,
			"version belongs to a different app",
			map[string]string{"version_app": artifact.AppID.String(), "app": req.AppID.String()},
		)
	}
	// Only an accepted version may run: a candidate that was
	// never activated is still being validated, and a session
	// must not be handed an artifact no activation approved.
	if artifact.ActivatedAt == 0 {
		return Resolution{}, domain.NewValidationError(
			codeNotAnAcceptedVersion,
			"running a pinned version requires a version that was activated before",
			map[string]string{"version": req.Version.String()},
		)
	}
	if err := authorizeRead(artifact, req.UserID, req.Policy); err != nil {
		return Resolution{}, err
	}

	current, err := s.registry.Current(ctx, req.AppID)
	if err != nil {
		return Resolution{}, err
	}

	s.mu.Lock()
	pin, hasPin, err := s.store.SessionPin(ctx, req.SessionID, req.AppID)
	s.mu.Unlock()
	if err != nil {
		return Resolution{}, err
	}
	if hasPin {
		if pin.Version != req.Version {
			// The stored session state belongs to the other
			// version. Running this one against it would lose
			// that state or save it under the wrong version, so
			// the disagreement is reported instead.
			return Resolution{}, domain.NewValidationError(
				"session_pinned_another_version",
				"the session already runs another version of this app",
				map[string]string{
					"session":   req.SessionID.String(),
					"app":       req.AppID.String(),
					"pinned":    pin.Version.String(),
					"requested": req.Version.String(),
				},
			)
		}
		return Resolution{Version: req.Version, State: pin.State, Current: current}, nil
	}

	// No pin yet, which is a resumed foreground session: it
	// inherits the durable state for the version it runs.
	state, err := s.inheritDurableState(ctx, req.UserID, req.AppID, req.Version, artifact)
	if err != nil {
		return Resolution{}, err
	}
	s.mu.Lock()
	err = s.store.PinSession(ctx, req.SessionID, req.AppID, req.Version, state)
	s.mu.Unlock()
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Version: req.Version, State: state, Current: current}, nil
}

// SaveStateRequest records a session's current application
// state and rolls the durable user and shared portions
// forward. A nil portion means the application reports no
// state of that scope.
type SaveStateRequest struct {
	SessionID domain.SessionID
	UserID    domain.UserID
	AppID     domain.AppID
	State     domain.AppState
}

// SaveSessionState persists a session's application state
// after an accepted app result. The session must have
// resolved the app first; the state is recorded against the
// session's pinned version.
func (s *Service) SaveSessionState(ctx context.Context, req SaveStateRequest) error {
	if req.SessionID.IsZero() || req.UserID.IsZero() || req.AppID.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "session, user, and app are required", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pin, ok, err := s.store.SessionPin(ctx, req.SessionID, req.AppID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.NewValidationError(
			"session_not_resolved",
			"the session must resolve the app before saving state",
			map[string]string{"session": req.SessionID.String(), "app": req.AppID.String()},
		)
	}
	if err := s.store.SaveSessionState(ctx, req.SessionID, req.AppID, pin.Version, req.State); err != nil {
		return err
	}

	artifact, err := s.registry.GetArtifact(ctx, pin.Version)
	if err != nil {
		return err
	}

	if err := s.store.SaveUserState(ctx, req.UserID, req.AppID, ports.AppStateRecord{
		Version: pin.Version,
		State:   domain.AppState{UserState: req.State.UserState},
	}); err != nil {
		return err
	}
	// Only a shared-scoped app has shared durable state;
	// persisting it for a user-scoped app would leak one
	// user's data into another user's resolution.
	if artifact.Scope == domain.ScopeShared {
		if err := s.store.SaveSharedState(ctx, req.AppID, ports.AppStateRecord{
			Version: pin.Version,
			State:   domain.AppState{SharedState: req.State.SharedState},
		}); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseSession drops a session's pin and session state when
// the session stops using an application. Durable user and
// shared state is retained for later sessions.
func (s *Service) ReleaseSession(ctx context.Context, session domain.SessionID, app domain.AppID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.ReleaseSession(ctx, session, app)
}

// RecordCommand records that the shell's visible command name
// is implemented by the given application, through the same
// durable store as the app state: the shell resolves a command
// to its stored app after a restart instead of generating a
// duplicate. A name recorded again moves to the latest app.
func (s *Service) RecordCommand(ctx context.Context, name string, app domain.AppID) error {
	if name == "" || app.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "command name and app are required", nil)
	}
	return s.store.RecordCommand(ctx, name, app)
}

// CommandIndex returns the command-name to application map
// the shell seeds its routing table from.
func (s *Service) CommandIndex(ctx context.Context) (map[string]domain.AppID, error) {
	return s.store.CommandIndex(ctx)
}

// inheritDurableState assembles the initial state for a new
// session from the durable user and shared records, migrating
// each portion to the current version. A portion that cannot
// be migrated and verified starts fresh: the durable record
// itself is retained, never discarded.
func (s *Service) inheritDurableState(
	ctx context.Context,
	user domain.UserID,
	app domain.AppID,
	current domain.AppVersionID,
	currentArtifact domain.AppArtifact,
) (domain.AppState, error) {
	state := domain.AppState{}

	s.mu.Lock()
	userRecord, hasUser, err := s.store.UserState(ctx, user, app)
	if err != nil {
		s.mu.Unlock()
		return domain.AppState{}, err
	}
	sharedRecord, hasShared, err := s.store.SharedState(ctx, app)
	s.mu.Unlock()
	if err != nil {
		return domain.AppState{}, err
	}
	if currentArtifact.Scope != domain.ScopeShared {
		hasShared = false
	}

	if hasUser && userRecord.Version != current && userRecord.State.UserState != nil {
		migrated, err := s.migrateState(ctx, app, userRecord.Version, current, userRecord.State)
		if err == nil {
			err = s.verifyState(ctx, currentArtifact, migrated)
		}
		if err != nil {
			// Safe fallback: fresh user state; the durable
			// record is retained untouched.
			state.UserState = nil
		} else {
			state.UserState = migrated.UserState
		}
	} else if hasUser {
		state.UserState = userRecord.State.UserState
	}

	if hasShared && sharedRecord.Version != current && sharedRecord.State.SharedState != nil {
		migrated, err := s.migrateState(ctx, app, sharedRecord.Version, current, sharedRecord.State)
		if err == nil {
			err = s.verifyState(ctx, currentArtifact, migrated)
		}
		if err != nil {
			state.SharedState = nil
		} else {
			state.SharedState = migrated.SharedState
		}
	} else if hasShared {
		state.SharedState = sharedRecord.State.SharedState
	}
	return state, nil
}

// authorizeMutation gates activation and rollback: the owning
// user changes a user-scoped app; a shared-scoped app
// follows the sharing policy.
func authorizeMutation(artifact domain.AppArtifact, actor domain.UserID, policy domain.ScopePolicy) error {
	switch artifact.Scope {
	case domain.ScopeUser:
		if actor == artifact.Owner {
			return nil
		}
		return domain.NewDeniedError(
			domain.CodePermissionDenied,
			"only the owning user may activate or roll back this app",
			map[string]string{"app": artifact.AppID.String(), "owner": artifact.Owner.String()},
		)
	case domain.ScopeShared:
		// Shared-scope changes follow the canonical domain
		// policy: sharing must be enabled and shared writes
		// permitted, exactly as for shared world state.
		return policy.AuthorizeWrite(domain.ScopeShared, false)
	default:
		return domain.NewValidationError(
			domain.CodeInvalidScope,
			"app artifacts are user- or shared-scoped",
			map[string]string{"app": artifact.AppID.String()},
		)
	}
}

// authorizeRead gates first resolution of an app for a user
// through the canonical domain scope policy: a user-scoped app
// is readable by its owner and, when sharing allows cross-user
// reads, by others; a shared-scoped app is readable only while
// sharing is enabled.
func authorizeRead(artifact domain.AppArtifact, user domain.UserID, policy domain.ScopePolicy) error {
	switch artifact.Scope {
	case domain.ScopeUser, domain.ScopeShared:
		return policy.AuthorizeRead(artifact.Scope, artifact.Owner == user)
	default:
		return domain.NewValidationError(
			domain.CodeInvalidScope,
			"app artifacts are user- or shared-scoped",
			map[string]string{"app": artifact.AppID.String()},
		)
	}
}

func generationLostError(app domain.AppID, base, current domain.AppVersionID) error {
	return domain.WrapError(
		ErrGenerationLost,
		domain.CategoryConflict,
		generationLostCode,
		fmt.Sprintf("generation lost for app %s: current version moved from %s to %s", app, base, current),
	)
}

func joinIssues(issues []string) string {
	result := ""
	for i, issue := range issues {
		if i > 0 {
			result += "; "
		}
		result += issue
	}
	return result
}
