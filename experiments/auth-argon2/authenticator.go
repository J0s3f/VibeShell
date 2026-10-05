package auth

import (
	"context"
)

// Authenticator coordinates password-file lookup, dummy verification for
// missing or disabled users, admission control, attempt limits, and the abuse
// rate path. Transport adapters (PLAN 4.3) call it with the connection's
// limiter and source identity; none of these rules belong to the transport.
type Authenticator struct {
	store     *FileStore
	verifier  *Verifier
	admission *Admission
	abuse     *AbuseGuard
}

// NewAuthenticator wires the authentication dependencies.
func NewAuthenticator(store *FileStore, verifier *Verifier, admission *Admission, abuse *AbuseGuard) *Authenticator {
	return &Authenticator{
		store:     store,
		verifier:  verifier,
		admission: admission,
		abuse:     abuse,
	}
}

// Authenticate verifies password for username on behalf of one connection
// identified by source. Every rejection the client can provoke cheaply
// (attempt budget, abuse penalty, queue saturation) happens before any
// Argon2id work. Exactly one verification runs per admitted attempt: a stored
// hash when the account exists and is enabled, the dummy hash otherwise, so
// unknown and disabled users pay the same cost as a wrong password
// (PLAN 4.2). ErrAuthFailed is the single error for unknown users, disabled
// users, and wrong passwords.
func (a *Authenticator) Authenticate(ctx context.Context, source string, limiter *ConnectionLimiter, username string, password []byte) error {
	if err := limiter.Allow(); err != nil {
		return err
	}
	if err := a.abuse.Allow(source); err != nil {
		return err
	}
	release, err := a.admission.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	target := a.targetFor(username)
	if target == nil {
		// Unknown user, disabled user, or corrupt stored hash: the load
		// path rejects corrupt hashes, so this is a defensive dummy run.
		a.verifier.DummyVerify(password)
		return a.failed(source, limiter)
	}
	if !a.verifier.Verify(*target, password) {
		return a.failed(source, limiter)
	}
	limiter.RecordSuccess()
	a.abuse.RecordSuccess(source)
	return nil
}

// targetFor returns the hash to verify against, or nil when the username must
// take the dummy path.
func (a *Authenticator) targetFor(username string) *EncodedHash {
	file, loaded := a.store.Current()
	if !loaded {
		return nil
	}
	entry, found := file.Find(username)
	if !found || !entry.Enabled {
		return nil
	}
	encoded, err := ParseEncoded(entry.Hash)
	if err != nil {
		return nil
	}
	return &encoded
}

func (a *Authenticator) failed(source string, limiter *ConnectionLimiter) error {
	limiter.RecordFailure()
	a.abuse.RecordFailure(source)
	return ErrAuthFailed
}
