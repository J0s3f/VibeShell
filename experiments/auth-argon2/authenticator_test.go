package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// newTestAuthenticator wires an Authenticator over obviously-fake fixture
// accounts hashed with fastTestParams, on an injected clock.
func newTestAuthenticator(t *testing.T, admissionCfg AdmissionConfig, abuseCfg AbuseConfig, users ...UserEntry) (*Authenticator, *fakeClock) {
	t.Helper()
	clock := newFakeClock()

	verifier, err := NewVerifier(fastTestParams, testSalt)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	admission, err := NewAdmission(admissionCfg)
	if err != nil {
		t.Fatalf("NewAdmission: %v", err)
	}
	abuse, err := NewAbuseGuard(abuseCfg, clock.now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}
	store := &FileStore{}
	if len(users) > 0 {
		if err := store.Load(mustEncodeFile(t, fixtureFile(t, users...))); err != nil {
			t.Fatalf("Load: %v", err)
		}
	}
	return NewAuthenticator(store, verifier, admission, abuse), clock
}

func testLimiter(t *testing.T, clock *fakeClock, maxAttempts int) *ConnectionLimiter {
	t.Helper()
	limiter, err := NewConnectionLimiter(ConnectionConfig{MaxAttempts: maxAttempts}, clock.now)
	if err != nil {
		t.Fatalf("NewConnectionLimiter: %v", err)
	}
	return limiter
}

func defaultAbuseConfig() AbuseConfig {
	cfg := DefaultAbuseConfig()
	cfg.MaxTrackedSources = 16
	return cfg
}

func TestAuthenticateAcceptsCorrectPassword(t *testing.T) {
	auth, clock := newTestAuthenticator(t,
		DefaultAdmissionConfig(), defaultAbuseConfig(),
		fixtureEntry(t, "alice", true), fixtureEntry(t, "bob", false))
	limiter := testLimiter(t, clock, 3)

	err := auth.Authenticate(context.Background(), "203.0.113.1:1000", limiter, "alice", []byte(fakePassword))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if limiter.failures != 0 {
		t.Errorf("failures after success = %d, want 0", limiter.failures)
	}
}

func TestAuthenticateRejectsWrongUnknownAndDisabledUsers(t *testing.T) {
	auth, clock := newTestAuthenticator(t,
		DefaultAdmissionConfig(), defaultAbuseConfig(),
		fixtureEntry(t, "alice", true), fixtureEntry(t, "bob", false))
	limiter := testLimiter(t, clock, 10)

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"wrong password", "alice", "wrong-password-fake"},
		{"unknown user", "mallory", fakePassword},
		{"disabled user with correct password", "bob", fakePassword},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := auth.Authenticate(context.Background(), "203.0.113.1:1001", limiter, tc.username, []byte(tc.password))
			if !errors.Is(err, ErrAuthFailed) {
				t.Fatalf("Authenticate error = %v, want ErrAuthFailed", err)
			}
		})
	}
	if limiter.failures != 3 {
		t.Errorf("recorded failures = %d, want 3", limiter.failures)
	}
}

func TestAuthenticateFailsClosedWithoutLoadedFile(t *testing.T) {
	auth, clock := newTestAuthenticator(t, DefaultAdmissionConfig(), defaultAbuseConfig())
	limiter := testLimiter(t, clock, 3)

	err := auth.Authenticate(context.Background(), "203.0.113.1:1002", limiter, "alice", []byte(fakePassword))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Authenticate error = %v, want ErrAuthFailed", err)
	}
	if limiter.failures != 1 {
		t.Errorf("recorded failures = %d, want 1", limiter.failures)
	}
}

func TestAuthenticateEnforcesConnectionAttemptLimit(t *testing.T) {
	auth, clock := newTestAuthenticator(t,
		DefaultAdmissionConfig(), defaultAbuseConfig(),
		fixtureEntry(t, "alice", true))
	limiter := testLimiter(t, clock, 2)

	for i := 0; i < 2; i++ {
		err := auth.Authenticate(context.Background(), "203.0.113.1:1003", limiter, "alice", []byte("wrong-password-fake"))
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("attempt %d error = %v, want ErrAuthFailed", i+1, err)
		}
	}
	err := auth.Authenticate(context.Background(), "203.0.113.1:1003", limiter, "alice", []byte("wrong-password-fake"))
	if !errors.Is(err, ErrAttemptsExceeded) {
		t.Fatalf("attempt 3 error = %v, want ErrAttemptsExceeded", err)
	}
}

func TestAuthenticateEnforcesAbuseRateAcrossConnections(t *testing.T) {
	abuseCfg := defaultAbuseConfig()
	abuseCfg.Threshold = 2
	auth, clock := newTestAuthenticator(t,
		DefaultAdmissionConfig(), abuseCfg,
		fixtureEntry(t, "alice", true))

	const source = "198.51.100.42:2000"
	for i := 0; i < 2; i++ {
		// A fresh limiter per connection: the abuse path must catch a
		// source that opens a new connection after every failure.
		limiter := testLimiter(t, clock, 6)
		err := auth.Authenticate(context.Background(), source, limiter, "alice", []byte("wrong-password-fake"))
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("failure %d error = %v, want ErrAuthFailed", i+1, err)
		}
	}

	limiter := testLimiter(t, clock, 6)
	err := auth.Authenticate(context.Background(), source, limiter, "alice", []byte(fakePassword))
	if !errors.Is(err, ErrAbusive) {
		t.Fatalf("third connection error = %v, want ErrAbusive", err)
	}

	// A different source still authenticates with the correct password.
	other := testLimiter(t, clock, 6)
	err = auth.Authenticate(context.Background(), "203.0.113.7:3000", other, "alice", []byte(fakePassword))
	if err != nil {
		t.Fatalf("unrelated source authenticate: %v", err)
	}
}

func TestAuthenticateRejectsWhenAdmissionBusy(t *testing.T) {
	auth, clock := newTestAuthenticator(t,
		AdmissionConfig{MaxConcurrent: 1, MaxWaiting: 0}, defaultAbuseConfig(),
		fixtureEntry(t, "alice", true))
	limiter := testLimiter(t, clock, 3)

	release, err := auth.admission.acquire(context.Background())
	if err != nil {
		t.Fatalf("hold admission slot: %v", err)
	}
	defer release()

	err = auth.Authenticate(context.Background(), "203.0.113.1:1004", limiter, "alice", []byte(fakePassword))
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Authenticate error = %v, want ErrBusy", err)
	}
	if limiter.failures != 0 {
		t.Errorf("busy rejection recorded a failure: %d", limiter.failures)
	}
}

// TestAuthenticateBoundedBurst exercises the whole path concurrently so the
// race detector covers the file snapshot, admission slots, limiter, and abuse
// guard together.
func TestAuthenticateBoundedBurst(t *testing.T) {
	const (
		workers       = 24
		maxConcurrent = 4
	)
	auth, clock := newTestAuthenticator(t,
		AdmissionConfig{MaxConcurrent: maxConcurrent, MaxWaiting: workers},
		defaultAbuseConfig(),
		fixtureEntry(t, "alice", true))

	limiters := make([]*ConnectionLimiter, workers)
	for i := range limiters {
		limiters[i] = testLimiter(t, clock, 6)
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			source := fmt.Sprintf("198.51.100.%d:2200", i)
			password := []byte(fakePassword)
			if i%2 == 1 {
				password = []byte("wrong-password-fake")
			}
			err := auth.Authenticate(context.Background(), source, limiters[i], "alice", password)
			if i%2 == 0 {
				if err != nil {
					t.Errorf("worker %d: %v", i, err)
				}
			} else if !errors.Is(err, ErrAuthFailed) {
				t.Errorf("worker %d error = %v, want ErrAuthFailed", i, err)
			}
		}(i)
	}
	wg.Wait()
}
