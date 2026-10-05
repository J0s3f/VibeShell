package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Compile-time proof that the store stays behind the application ports.
var _ ports.AppRegistry = (*Apps)(nil)
var _ ports.AppStateStore = (*Apps)(nil)

// Durable state partitions: the durable portion of an application's state is
// either private to one user or shared by every session that may see the app.
const (
	StateScopeUser   = "user"
	StateScopeShared = "shared"
)

// Pointer-move kinds recorded in the activation log.
const (
	ActivationActivate = "activate"
	ActivationRollback = "rollback"
)

// Apps is the SQLite-backed generated-application store. It holds the
// immutable artifact of every accepted version, the per-app current pointer,
// the durable user and shared state partitions, the per-session version pins,
// and the activation/rollback log (PLAN 5.6, 10.2).
//
// It implements ports.AppRegistry and ports.AppStateStore; the state, pin, and
// command-index methods are the durable replacement for the in-memory contract
// double the application service falls back to, so a restart no longer loses
// saved application state, pins, or command names.
type Apps struct {
	sql   *sql.DB
	clock ports.Clock
}

// NewApps wraps db with the generated-application store. Migrations must
// already have run (Open does that); the constructor verifies the tables exist
// so a wiring mistake fails at startup instead of on the first save.
func NewApps(db *sql.DB, clock ports.Clock) (*Apps, error) {
	if db == nil {
		return nil, errors.New("sqlite: app store requires a database handle")
	}
	if clock == nil {
		return nil, errors.New("sqlite: app store requires a clock for activation timestamps")
	}
	if err := requireTable(db, "app_versions"); err != nil {
		return nil, err
	}
	return &Apps{sql: db, clock: clock}, nil
}

// ---------------------------------------------------------------------------
// ports.AppRegistry
// ---------------------------------------------------------------------------

// GetArtifact returns the immutable artifact stored for a version.
func (a *Apps) GetArtifact(ctx context.Context, version domain.AppVersionID) (domain.AppArtifact, error) {
	if version.IsZero() {
		return domain.AppArtifact{}, domain.NewValidationError(domain.CodeInvalidIdentity, "app version is required", nil)
	}
	row, err := scanAppVersion(a.sql.QueryRowContext(ctx,
		`SELECT `+appVersionColumns+` FROM app_versions WHERE version_id = ?`, version.String()))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AppArtifact{}, domain.NewNotFoundError(
				domain.CodeAppVersionNotFound, "unknown app version",
				map[string]string{"version": version.String()},
			)
		}
		return domain.AppArtifact{}, fmt.Errorf("sqlite: read app version %q: %w", version.String(), err)
	}
	return row.artifact()
}

// Current returns the activated version of an app. An app whose candidate has
// never been activated has no current version and reports it as not found.
func (a *Apps) Current(ctx context.Context, app domain.AppID) (domain.AppVersionID, error) {
	var current string
	err := a.sql.QueryRowContext(ctx, `SELECT current_version FROM apps WHERE app_id = ?`, app.String()).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.AppVersionID{}, appNotFound(app)
	case err != nil:
		return domain.AppVersionID{}, fmt.Errorf("sqlite: read current version of %q: %w", app.String(), err)
	}
	if current == "" {
		return domain.AppVersionID{}, appNotFound(app)
	}
	version, err := domain.ParseAppVersionID(current)
	if err != nil {
		return domain.AppVersionID{}, decodeFailure("current app version", err)
	}
	return version, nil
}

// RegisterCandidate stores a candidate artifact without activating it. The
// version identity must be new, an extension's parent must already belong to
// the same app, and the stored source must hash to the recorded content
// reference, so a tampered or partially written artifact is refused here.
// Manifest validation and the staging smoke test belong to the application
// service; the adapter only enforces the invariants storage can guarantee.
func (a *Apps) RegisterCandidate(ctx context.Context, artifact domain.AppArtifact) (domain.AppVersionID, error) {
	if err := checkStorableArtifact(artifact); err != nil {
		return domain.AppVersionID{}, err
	}
	manifest, err := json.Marshal(artifact.Manifest)
	if err != nil {
		return domain.AppVersionID{}, encodeFailure("app manifest", err)
	}
	provenance, err := json.Marshal(artifact.Provenance)
	if err != nil {
		return domain.AppVersionID{}, encodeFailure("app provenance", err)
	}
	validation, err := json.Marshal(artifact.Validation)
	if err != nil {
		return domain.AppVersionID{}, encodeFailure("app validation result", err)
	}
	tx, err := a.sql.BeginTx(ctx, nil)
	if err != nil {
		return domain.AppVersionID{}, fmt.Errorf("sqlite: begin register candidate: %w", err)
	}
	defer tx.Rollback()

	if artifact.ParentVersion != nil {
		if err := checkParent(ctx, tx, artifact); err != nil {
			return domain.AppVersionID{}, err
		}
	}
	var taken int
	switch err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM app_versions WHERE version_id = ?`, artifact.VersionID.String()).Scan(&taken); {
	case err == nil:
		return domain.AppVersionID{}, domain.NewConflictError(
			domain.CodeDuplicateKey, "version ID already registered",
			map[string]string{"version": artifact.VersionID.String()},
		)
	case !errors.Is(err, sql.ErrNoRows):
		return domain.AppVersionID{}, fmt.Errorf("sqlite: look up app version %q: %w", artifact.VersionID.String(), err)
	}

	// The app row comes first: its versions reference it, and an app keeps the
	// owner and scope it was first registered with, so an extension may not
	// silently re-scope existing artifacts.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO apps(app_id, owner, scope, current_version, created_at) VALUES(?,?,?,'',?)
		 ON CONFLICT(app_id) DO NOTHING`,
		artifact.AppID.String(), artifact.Owner.String(), artifact.Scope.String(), artifact.CreatedAt,
	); err != nil {
		return domain.AppVersionID{}, fmt.Errorf("sqlite: insert app %q: %w", artifact.AppID.String(), err)
	}
	var owner, scope string
	if err := tx.QueryRowContext(ctx, `SELECT owner, scope FROM apps WHERE app_id = ?`, artifact.AppID.String()).
		Scan(&owner, &scope); err != nil {
		return domain.AppVersionID{}, fmt.Errorf("sqlite: read app %q: %w", artifact.AppID.String(), err)
	}
	if owner != artifact.Owner.String() {
		return domain.AppVersionID{}, domain.NewConflictError(
			domain.CodeDuplicateKey, "app is already registered to a different owner",
			map[string]string{"app": artifact.AppID.String(), "owner": owner},
		)
	}
	if scope != artifact.Scope.String() {
		return domain.AppVersionID{}, domain.NewConflictError(
			domain.CodeInvalidScope, "app is already registered with a different scope",
			map[string]string{"app": artifact.AppID.String(), "scope": scope},
		)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_versions(
			version_id, app_id, parent_version, manifest, source, source_hash,
			owner, scope, created_at, provenance, validation, activated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,0)`,
		artifact.VersionID.String(), artifact.AppID.String(), parentVersionString(artifact.ParentVersion),
		string(manifest), artifact.Source, artifact.SourceHash.String(),
		artifact.Owner.String(), artifact.Scope.String(), artifact.CreatedAt,
		string(provenance), string(validation),
	); err != nil {
		return domain.AppVersionID{}, fmt.Errorf("sqlite: insert app version %q: %w", artifact.VersionID.String(), err)
	}
	if err := tx.Commit(); err != nil {
		return domain.AppVersionID{}, fmt.Errorf("sqlite: commit register candidate: %w", err)
	}
	return artifact.VersionID, nil
}

// Activate moves the current pointer of an app to a registered candidate that
// passed validation, in one transaction, and records the move. Activating the
// version that is already current is an idempotent no-op: the pointer, the
// artifact's activation stamp, and the log are left untouched.
func (a *Apps) Activate(ctx context.Context, app domain.AppID, version domain.AppVersionID) error {
	tx, err := a.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin activate: %w", err)
	}
	defer tx.Rollback()

	artifact, err := loadOwnedAppVersion(ctx, tx, version, app)
	if err != nil {
		return err
	}
	if !artifact.Validation.Passed {
		return domain.NewValidationError(
			"candidate_not_validated", "only a candidate that passed validation can be activated",
			map[string]string{"version": version.String(), "issues": strings.Join(artifact.Validation.Issues, "; ")},
		)
	}
	current, err := currentPointer(ctx, tx, app)
	if err != nil {
		return err
	}
	if current == version.String() {
		return nil
	}
	if err := movePointer(ctx, tx, app, ActivationActivate, current, version.String(), a.clock.NowUnixMilli(), ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit activate: %w", err)
	}
	return nil
}

// Rollback restores the current pointer to a version that was activated before.
// The rolled-away version stays registered and retrievable, and sessions that
// already pinned it are unaffected.
func (a *Apps) Rollback(ctx context.Context, app domain.AppID, to domain.AppVersionID, reason string) error {
	tx, err := a.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin rollback: %w", err)
	}
	defer tx.Rollback()

	artifact, err := loadOwnedAppVersion(ctx, tx, to, app)
	if err != nil {
		return err
	}
	if artifact.ActivatedAt == 0 {
		return domain.NewValidationError(
			"not_an_accepted_version", "rollback requires a version that was activated before",
			map[string]string{"target": to.String()},
		)
	}
	current, err := currentPointer(ctx, tx, app)
	if err != nil {
		return err
	}
	if current == "" {
		// A rollback restores a pointer; an app that never activated a
		// candidate has none to restore.
		return appNotFound(app)
	}
	if current == to.String() {
		return nil
	}
	if err := movePointer(ctx, tx, app, ActivationRollback, current, to.String(), a.clock.NowUnixMilli(), reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit rollback: %w", err)
	}
	return nil
}

// loadOwnedAppVersion reads a stored artifact and verifies it belongs to app, so
// a pointer move can never cross applications.
func loadOwnedAppVersion(ctx context.Context, tx *sql.Tx, version domain.AppVersionID, app domain.AppID) (domain.AppArtifact, error) {
	row, err := scanAppVersion(tx.QueryRowContext(ctx,
		`SELECT `+appVersionColumns+` FROM app_versions WHERE version_id = ?`, version.String()))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AppArtifact{}, domain.NewNotFoundError(
				domain.CodeAppVersionNotFound, "unknown app version",
				map[string]string{"version": version.String()},
			)
		}
		return domain.AppArtifact{}, fmt.Errorf("sqlite: read app version %q: %w", version.String(), err)
	}
	artifact, err := row.artifact()
	if err != nil {
		return domain.AppArtifact{}, err
	}
	if artifact.AppID != app {
		return domain.AppArtifact{}, domain.NewValidationError(
			domain.CodeInvalidInput, "version belongs to a different app",
			map[string]string{"version_app": artifact.AppID.String(), "app": app.String()},
		)
	}
	return artifact, nil
}

// ---------------------------------------------------------------------------
// Durable state, session pins, activation log
// ---------------------------------------------------------------------------

// AppStateRecord is a stored state snapshot together with the artifact version
// that produced it. State written by an older version needs a verified
// migration before the current version may interpret it (PLAN 5.6). The shape
// is owned by ports.AppStateStore so both sides of the port agree; the alias
// keeps this adapter's documented name.
type AppStateRecord = ports.AppStateRecord

// SessionPin is a session's pinned artifact version together with its
// session-scoped state portion. The shape is owned by ports.AppStateStore;
// the alias keeps this adapter's documented name.
type SessionPin = ports.SessionPin

// AppActivation is one entry of the durable activation log.
type AppActivation struct {
	Kind   string // ActivationActivate or ActivationRollback
	AppID  domain.AppID
	From   *domain.AppVersionID
	To     domain.AppVersionID
	At     int64
	Reason string
}

// UserState returns the durable user-scoped state of an app, or ok=false when
// this user has never saved any.
func (a *Apps) UserState(ctx context.Context, user domain.UserID, app domain.AppID) (AppStateRecord, bool, error) {
	if user.IsZero() || app.IsZero() {
		return AppStateRecord{}, false, domain.NewValidationError(domain.CodeInvalidInput, "user and app are required", nil)
	}
	return a.appState(ctx, StateScopeUser, user.String(), app)
}

// SharedState returns the durable shared state of a shared-scoped app, or
// ok=false when none was ever saved.
func (a *Apps) SharedState(ctx context.Context, app domain.AppID) (AppStateRecord, bool, error) {
	if app.IsZero() {
		return AppStateRecord{}, false, domain.NewValidationError(domain.CodeInvalidInput, "app is required", nil)
	}
	return a.appState(ctx, StateScopeShared, "", app)
}

// SaveUserState records the durable user-scoped portion of an app's state.
func (a *Apps) SaveUserState(ctx context.Context, user domain.UserID, app domain.AppID, record AppStateRecord) error {
	if user.IsZero() || app.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidInput, "user and app are required", nil)
	}
	return a.saveAppState(ctx, StateScopeUser, user.String(), app, record)
}

// SaveSharedState records the durable shared portion of a shared-scoped app's
// state. A user-scoped app has no shared portion: storing one would leak one
// user's data into another user's resolution.
func (a *Apps) SaveSharedState(ctx context.Context, app domain.AppID, record AppStateRecord) error {
	if app.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidInput, "app is required", nil)
	}
	return a.saveAppState(ctx, StateScopeShared, "", app, record)
}

// SessionPin returns the version a session is pinned to for an app, or
// ok=false when the session never resolved that app.
func (a *Apps) SessionPin(ctx context.Context, session domain.SessionID, app domain.AppID) (SessionPin, bool, error) {
	if session.IsZero() || app.IsZero() {
		return SessionPin{}, false, domain.NewValidationError(domain.CodeInvalidInput, "session and app are required", nil)
	}
	var versionID, stateJSON string
	var pinnedAt int64
	err := a.sql.QueryRowContext(ctx,
		`SELECT version_id, session_state, pinned_at FROM app_session_pins WHERE session_id = ? AND app_id = ?`,
		session.String(), app.String()).Scan(&versionID, &stateJSON, &pinnedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionPin{}, false, nil
	case err != nil:
		return SessionPin{}, false, fmt.Errorf("sqlite: read session pin: %w", err)
	}
	pin, err := decodeSessionPin(versionID, stateJSON, pinnedAt)
	if err != nil {
		return SessionPin{}, false, err
	}
	return pin, true, nil
}

// PinSession pins a session to an artifact version and records the state the
// session starts from. Pinning a session that is already pinned moves the pin
// and replaces its session state.
func (a *Apps) PinSession(ctx context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error {
	if session.IsZero() || app.IsZero() || version.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidIdentity, "session, app, and version are required", nil)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return encodeFailure("app session state", err)
	}
	if _, err := a.sql.ExecContext(ctx, `INSERT INTO app_session_pins(
			session_id, app_id, version_id, session_state, pinned_at
		) VALUES(?,?,?,?,?)
		ON CONFLICT(session_id, app_id) DO UPDATE SET
			version_id = excluded.version_id,
			session_state = excluded.session_state,
			pinned_at = excluded.pinned_at`,
		session.String(), app.String(), version.String(), string(encoded), a.clock.NowUnixMilli(),
	); err != nil {
		return fmt.Errorf("sqlite: pin session %q to app %q: %w", session.String(), app.String(), err)
	}
	return nil
}

// SaveSessionState records the session-scoped portion of an app's state for the
// version the session is pinned to. The session must have resolved the app
// first: state without a pin could never be attributed to a version.
func (a *Apps) SaveSessionState(ctx context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error {
	if session.IsZero() || app.IsZero() || version.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidIdentity, "session, app, and version are required", nil)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return encodeFailure("app session state", err)
	}
	result, err := a.sql.ExecContext(ctx, `UPDATE app_session_pins SET session_state = ?
		WHERE session_id = ? AND app_id = ? AND version_id = ?`,
		string(encoded), session.String(), app.String(), version.String())
	if err != nil {
		return fmt.Errorf("sqlite: save session state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: save session state: %w", err)
	}
	if affected == 0 {
		return domain.NewValidationError(
			"session_not_resolved", "the session must resolve the app before saving state",
			map[string]string{"session": session.String(), "app": app.String()},
		)
	}
	return nil
}

// ReleaseSession drops a session's pin and session state. Durable user and
// shared state is retained for later sessions.
func (a *Apps) ReleaseSession(ctx context.Context, session domain.SessionID, app domain.AppID) error {
	if _, err := a.sql.ExecContext(ctx,
		`DELETE FROM app_session_pins WHERE session_id = ? AND app_id = ?`,
		session.String(), app.String(),
	); err != nil {
		return fmt.Errorf("sqlite: release session %q from app %q: %w", session.String(), app.String(), err)
	}
	return nil
}

// Activations returns the app's activation log, oldest first.
func (a *Apps) Activations(ctx context.Context, app domain.AppID) ([]AppActivation, error) {
	rows, err := a.sql.QueryContext(ctx,
		`SELECT kind, from_version, to_version, at, reason FROM app_activations
		 WHERE app_id = ? ORDER BY seq`, app.String())
	if err != nil {
		return nil, fmt.Errorf("sqlite: read app activations: %w", err)
	}
	defer rows.Close()
	var out []AppActivation
	for rows.Next() {
		var (
			kind, from, to, reason string
			at                     int64
		)
		if err := rows.Scan(&kind, &from, &to, &at, &reason); err != nil {
			return nil, fmt.Errorf("sqlite: read app activations: %w", err)
		}
		entry := AppActivation{Kind: kind, AppID: app, At: at, Reason: reason}
		if from != "" {
			version, err := domain.ParseAppVersionID(from)
			if err != nil {
				return nil, decodeFailure("activation source version", err)
			}
			entry.From = &version
		}
		toVersion, err := domain.ParseAppVersionID(to)
		if err != nil {
			return nil, decodeFailure("activation target version", err)
		}
		entry.To = toVersion
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read app activations: %w", err)
	}
	return out, nil
}

// appState reads one durable state partition.
func (a *Apps) appState(ctx context.Context, scope, owner string, app domain.AppID) (AppStateRecord, bool, error) {
	var versionID, stateJSON string
	err := a.sql.QueryRowContext(ctx,
		`SELECT version_id, state FROM app_state WHERE state_scope = ? AND owner = ? AND app_id = ?`,
		scope, owner, app.String()).Scan(&versionID, &stateJSON)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AppStateRecord{}, false, nil
	case err != nil:
		return AppStateRecord{}, false, fmt.Errorf("sqlite: read %s app state: %w", scope, err)
	}
	version, err := domain.ParseAppVersionID(versionID)
	if err != nil {
		return AppStateRecord{}, false, decodeFailure("app state version", err)
	}
	state, err := decodeAppState(stateJSON)
	if err != nil {
		return AppStateRecord{}, false, err
	}
	return AppStateRecord{Version: version, State: state}, true, nil
}

// saveAppState upserts one durable state partition together with the version
// that produced it.
func (a *Apps) saveAppState(ctx context.Context, scope, owner string, app domain.AppID, record AppStateRecord) error {
	if record.Version.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidIdentity, "app state requires the version that produced it", nil)
	}
	encoded, err := json.Marshal(record.State)
	if err != nil {
		return encodeFailure("app state", err)
	}
	if _, err := a.sql.ExecContext(ctx, `INSERT INTO app_state(
			state_scope, owner, app_id, version_id, state, updated_at
		) VALUES(?,?,?,?,?,?)
		ON CONFLICT(state_scope, owner, app_id) DO UPDATE SET
			version_id = excluded.version_id,
			state = excluded.state,
			updated_at = excluded.updated_at`,
		scope, owner, app.String(), record.Version.String(), string(encoded), a.clock.NowUnixMilli(),
	); err != nil {
		return fmt.Errorf("sqlite: save %s app state for %q: %w", scope, app.String(), err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Row codecs and shared helpers
// ---------------------------------------------------------------------------

const appVersionColumns = `version_id, app_id, parent_version, manifest, source, source_hash, ` +
	`owner, scope, created_at, provenance, validation, activated_at`

// appVersionRow is one row of app_versions in stored form.
type appVersionRow struct {
	versionID     string
	appID         string
	parentVersion string
	manifest      string
	source        string
	sourceHash    string
	owner         string
	scope         string
	createdAt     int64
	provenance    string
	validation    string
	activatedAt   int64
}

// rowScanner is satisfied by *sql.Row and *sql.Tx query results.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAppVersion(row rowScanner) (appVersionRow, error) {
	var r appVersionRow
	err := row.Scan(&r.versionID, &r.appID, &r.parentVersion, &r.manifest, &r.source,
		&r.sourceHash, &r.owner, &r.scope, &r.createdAt, &r.provenance, &r.validation, &r.activatedAt)
	return r, err
}

// artifact rebuilds the domain artifact, reporting a decode failure rather than
// silently substituting a partial artifact for corrupted storage.
func (r appVersionRow) artifact() (domain.AppArtifact, error) {
	versionID, err := domain.ParseAppVersionID(r.versionID)
	if err != nil {
		return domain.AppArtifact{}, decodeFailure("app version id", err)
	}
	appID, err := domain.ParseAppID(r.appID)
	if err != nil {
		return domain.AppArtifact{}, decodeFailure("app id", err)
	}
	owner, err := domain.ParseUserID(r.owner)
	if err != nil {
		return domain.AppArtifact{}, decodeFailure("app owner", err)
	}
	scope, err := domain.ParseScope(r.scope)
	if err != nil {
		return domain.AppArtifact{}, decodeFailure("app scope", err)
	}
	sourceHash, err := domain.ParseContentID(r.sourceHash)
	if err != nil {
		return domain.AppArtifact{}, decodeFailure("app source hash", err)
	}
	artifact := domain.AppArtifact{
		AppID:       appID,
		VersionID:   versionID,
		Manifest:    domain.AppManifest{},
		Source:      r.source,
		SourceHash:  sourceHash,
		Owner:       owner,
		Scope:       scope,
		CreatedAt:   r.createdAt,
		Provenance:  domain.Provenance{},
		Validation:  domain.ValidationResult{},
		ActivatedAt: r.activatedAt,
	}
	if r.parentVersion != "" {
		parent, err := domain.ParseAppVersionID(r.parentVersion)
		if err != nil {
			return domain.AppArtifact{}, decodeFailure("parent app version id", err)
		}
		artifact.ParentVersion = &parent
	}
	if err := json.Unmarshal([]byte(r.manifest), &artifact.Manifest); err != nil {
		return domain.AppArtifact{}, decodeFailure("app manifest", err)
	}
	if err := json.Unmarshal([]byte(r.provenance), &artifact.Provenance); err != nil {
		return domain.AppArtifact{}, decodeFailure("app provenance", err)
	}
	if err := json.Unmarshal([]byte(r.validation), &artifact.Validation); err != nil {
		return domain.AppArtifact{}, decodeFailure("app validation result", err)
	}
	return artifact, nil
}

// checkStorableArtifact enforces the invariants the storage layer can guarantee
// on its own: a complete identity, a durable scope, present source, and a
// source hash that proves the stored source is the one that was validated.
func checkStorableArtifact(artifact domain.AppArtifact) error {
	switch {
	case artifact.VersionID.IsZero():
		return domain.NewValidationError(domain.CodeInvalidIdentity, "candidate requires a version ID", nil)
	case artifact.AppID.IsZero():
		return domain.NewValidationError(domain.CodeInvalidIdentity, "candidate requires an app ID", nil)
	case artifact.Owner.IsZero():
		return domain.NewValidationError(domain.CodeInvalidIdentity, "candidate requires an owner", nil)
	case artifact.Scope != domain.ScopeUser && artifact.Scope != domain.ScopeShared:
		return domain.NewValidationError(
			domain.CodeInvalidScope, "app artifacts are user- or shared-scoped",
			map[string]string{"scope": artifact.Scope.String()},
		)
	case artifact.Source == "":
		return domain.NewValidationError(domain.CodeInvalidAppManifest, "candidate requires source", nil)
	}
	// contentIDFor derives the source reference exactly as the content store
	// does, so a stored artifact whose bytes were altered is refused here.
	want, err := contentIDFor([]byte(artifact.Source))
	if err != nil {
		return domain.NewInternalError(domain.CodeSerializationFailed, "sqlite: derive app source hash", err)
	}
	if artifact.SourceHash != want {
		return domain.NewValidationError(
			domain.CodeInvalidAppManifest, "source_hash does not match the source",
			map[string]string{"version": artifact.VersionID.String()},
		)
	}
	return nil
}

// checkParent verifies that an extension names a registered version of the same
// app.
func checkParent(ctx context.Context, tx *sql.Tx, artifact domain.AppArtifact) error {
	var parentApp string
	err := tx.QueryRowContext(ctx, `SELECT app_id FROM app_versions WHERE version_id = ?`,
		artifact.ParentVersion.String()).Scan(&parentApp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.NewValidationError(
			domain.CodeAppVersionNotFound, "parent version is not registered",
			map[string]string{"parent": artifact.ParentVersion.String()},
		)
	case err != nil:
		return fmt.Errorf("sqlite: read parent app version %q: %w", artifact.ParentVersion.String(), err)
	}
	if parentApp != artifact.AppID.String() {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "parent version belongs to a different app",
			map[string]string{"parent_app": parentApp, "app": artifact.AppID.String()},
		)
	}
	return nil
}

// currentPointer returns the activated version of an app inside tx, or an empty
// string when the app exists but has never activated a candidate.
func currentPointer(ctx context.Context, tx *sql.Tx, app domain.AppID) (string, error) {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT current_version FROM apps WHERE app_id = ?`, app.String()).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", domain.NewNotFoundError(
			domain.CodeAppNotFound, "app is not registered",
			map[string]string{"app": app.String()},
		)
	case err != nil:
		return "", fmt.Errorf("sqlite: read current version of %q: %w", app.String(), err)
	}
	return current, nil
}

// movePointer stamps the version's first activation, moves the current pointer,
// and appends the activation log entry, all inside the caller's transaction.
func movePointer(ctx context.Context, tx *sql.Tx, app domain.AppID, kind, from, to string, at int64, reason string) error {
	// The artifact keeps its first acceptance timestamp: later activations of
	// the same version are pointer moves, not a new artifact.
	if _, err := tx.ExecContext(ctx,
		`UPDATE app_versions SET activated_at = ? WHERE version_id = ? AND activated_at = 0`, at, to,
	); err != nil {
		return fmt.Errorf("sqlite: stamp activation of %q: %w", to, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE apps SET current_version = ? WHERE app_id = ?`, to, app.String(),
	); err != nil {
		return fmt.Errorf("sqlite: move current version of %q: %w", app.String(), err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO app_activations(app_id, kind, from_version, to_version, at, reason) VALUES(?,?,?,?,?,?)`,
		app.String(), kind, from, to, at, reason,
	); err != nil {
		return fmt.Errorf("sqlite: record app activation: %w", err)
	}
	return nil
}

func parentVersionString(parent *domain.AppVersionID) string {
	if parent == nil {
		return ""
	}
	return parent.String()
}

func decodeSessionPin(versionID, stateJSON string, pinnedAt int64) (SessionPin, error) {
	version, err := domain.ParseAppVersionID(versionID)
	if err != nil {
		return SessionPin{}, decodeFailure("session pin version", err)
	}
	state, err := decodeAppState(stateJSON)
	if err != nil {
		return SessionPin{}, err
	}
	return SessionPin{Version: version, State: state, PinnedAt: pinnedAt}, nil
}

func decodeAppState(encoded string) (domain.AppState, error) {
	var state domain.AppState
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return domain.AppState{}, decodeFailure("app state", err)
	}
	return state, nil
}

func appNotFound(app domain.AppID) error {
	return domain.NewNotFoundError(
		domain.CodeAppNotFound, "app has no active version",
		map[string]string{"app": app.String()},
	)
}

func encodeFailure(what string, err error) error {
	return domain.NewInternalError(domain.CodeSerializationFailed, "sqlite: encode "+what, err)
}

func decodeFailure(what string, err error) error {
	return domain.NewInternalError(domain.CodeDeserializationFailed, "sqlite: decode "+what, err)
}

// requireTable fails when a table is absent, which means the process skipped
// migrations rather than that the storage is corrupt.
func requireTable(db *sql.DB, name string) error {
	var found string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if err != nil || found != name {
		return fmt.Errorf("sqlite: %s table missing (run Migrate first): %v", name, err)
	}
	return nil
}
