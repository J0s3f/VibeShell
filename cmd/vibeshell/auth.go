package main

import (
	"fmt"
	"log/slog"
	"time"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/observability"
	"j0s.at/vibeshell/internal/ports"

	ssh "j0s.at/vibeshell/internal/adapters/ssh"
)

// passwordLookup resolves secure-mode usernames to their durable identity via
// the config adapter's password store. It is used by the SSH handler after a
// successful password authentication; the store verifies the credential in
// the transport callback (see passwordAuthenticator).
type passwordLookup struct {
	store *config.PasswordStore
	clock ports.Clock
}

// identityFor returns the durable identity recorded for a secure username.
func (l *passwordLookup) identityFor(username string) (domain.UserID, error) {
	return secureIdentity(l.store, username)
}

// passwordAuthenticator adapts the config adapter's password store to the
// SSH transport's PasswordAuthenticator port (PLAN 4.2). It maps every
// failure to the transport's uniform ErrUnauthenticated so a client cannot
// distinguish an unknown user, a wrong password, and a disabled account.
type passwordAuthenticator struct {
	store *config.PasswordStore
}

var _ ssh.PasswordAuthenticator = (*passwordAuthenticator)(nil)

// AuthenticatePassword verifies one attempt. The password store performs
// dummy verification for unknown/disabled users, so the work profile is
// indistinguishable.
func (a *passwordAuthenticator) AuthenticatePassword(username, password string) (ssh.Principal, error) {
	if a.store == nil {
		return ssh.Principal{}, fmt.Errorf("%w: password authentication is not configured", ssh.ErrUnauthenticated)
	}
	user, ok, err := a.store.Verify(username, []byte(password))
	if err != nil {
		return ssh.Principal{}, fmt.Errorf("%w: verification failed", ssh.ErrUnauthenticated)
	}
	if !ok {
		return ssh.Principal{}, ssh.ErrUnauthenticated
	}
	return ssh.Principal{Username: user.Username}, nil
}

// authModeOf maps the handler's mode to the application's research-only auth
// mode value.
func authModeOf(mode configAuthMode) application.AuthMode {
	if mode == authModeSecure {
		return application.AuthModeSecure
	}
	return application.AuthModePublic
}

// nowUnixMilli is the composition root's wall-clock read for presentation
// inputs and startup records.
func nowUnixMilli() int64 { return time.Now().UnixMilli() }

// attr builds one allowlisted structured-log attribute. It exists so the
// composition root names fields through the observability constants rather
// than string literals.
func attr(field observability.AllowedField, value string) slog.Attr {
	return slog.String(string(field), value)
}
