package admin

import (
	"context"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// The bindings in this file are the thin adapters between the admin service's
// small interfaces and the concrete adapters the composition root wires. They
// keep the service itself free of adapter imports so it can be tested with
// doubles; only this file knows the config and apps packages.

// PasswordStoreBinding adapts the config adapter's password store to
// PasswordMaintenance. It owns the cost parameters and randomness the store's
// AddUser needs, so an operator command supplies only username and password.
type PasswordStoreBinding struct {
	store  *config.PasswordStore
	random ports.Random
	params config.Params
}

// Compile-time proof of the delegation.
var _ PasswordMaintenance = (*PasswordStoreBinding)(nil)

// NewPasswordStoreBinding builds the binding. A nil random source is
// rejected by AddUser at call time, not silently replaced.
func NewPasswordStoreBinding(store *config.PasswordStore, random ports.Random, params config.Params) *PasswordStoreBinding {
	return &PasswordStoreBinding{store: store, random: random, params: params}
}

// AddUser creates a user with a fresh stable identity.
func (b *PasswordStoreBinding) AddUser(username string, password []byte) (domain.UserID, error) {
	return b.store.AddUser(b.random, username, password, b.params)
}

// SetPassword replaces an existing user's hash.
func (b *PasswordStoreBinding) SetPassword(username string, password []byte) error {
	return b.store.SetPassword(username, password, b.params)
}

// SetEnabled enables or disables login.
func (b *PasswordStoreBinding) SetEnabled(username string, enabled bool) error {
	return b.store.SetEnabled(username, enabled)
}

// RemoveUser deletes a user.
func (b *PasswordStoreBinding) RemoveUser(username string) error {
	return b.store.RemoveUser(username)
}

// Users lists the entries without hash material. An entry whose identity does
// not parse is skipped: the file is validated on load, so this only guards a
// concurrently replaced file.
func (b *PasswordStoreBinding) Users() []PasswordUser {
	file := b.store.Pin()
	out := make([]PasswordUser, 0, len(file.Users))
	for _, user := range file.Users {
		identity, err := domain.ParseUserID(user.Identity)
		if err != nil {
			continue
		}
		out = append(out, PasswordUser{Username: user.Username, Enabled: user.Enabled, Identity: identity})
	}
	return out
}

// AppServiceBinding adapts the application registry service to AppRollbacker.
// An operator rollback runs with the app's owning user as the actor, because
// the registry authorizes user-scoped mutations by owner; the operator's
// authority is the container-exec access that invoked the command.
type AppServiceBinding struct {
	registry ports.AppRegistry
	service  *apps.Service
	policy   domain.ScopePolicy
}

// Compile-time proof of the delegation.
var _ AppRollbacker = (*AppServiceBinding)(nil)

// NewAppServiceBinding builds the binding over the registry and its service.
func NewAppServiceBinding(registry ports.AppRegistry, service *apps.Service) *AppServiceBinding {
	return &AppServiceBinding{
		registry: registry,
		service:  service,
		policy:   domain.DefaultScopePolicy(),
	}
}

// RollbackApp restores the active pointer to a prior accepted version.
func (b *AppServiceBinding) RollbackApp(ctx context.Context, app domain.AppID, target domain.AppVersionID, reason string) error {
	if app.IsZero() || target.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidInput, "app and target version are required", nil)
	}
	artifact, err := b.registry.GetArtifact(ctx, target)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "administrative rollback"
	}
	_, err = b.service.Rollback(ctx, apps.RollbackRequest{
		AppID:  app,
		Target: target,
		Actor:  artifact.Owner,
		Policy: b.policy,
		Reason: reason,
	})
	return err
}
