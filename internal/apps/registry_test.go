package apps

import (
	"context"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func TestNewIDsAreValidAndUnique(t *testing.T) {
	random := &SequenceRandom{}
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		appID, err := NewAppID(random)
		if err != nil {
			t.Fatalf("NewAppID: %v", err)
		}
		if appID.Prefix() != domain.PrefixApp {
			t.Errorf("app ID prefix = %q, want %q", appID.Prefix(), domain.PrefixApp)
		}
		if seen[appID.String()] {
			t.Errorf("duplicate app ID %s", appID)
		}
		seen[appID.String()] = true

		versionID, err := NewAppVersionID(random)
		if err != nil {
			t.Fatalf("NewAppVersionID: %v", err)
		}
		if versionID.Prefix() != domain.PrefixAppVer {
			t.Errorf("version ID prefix = %q, want %q", versionID.Prefix(), domain.PrefixAppVer)
		}
		if seen[versionID.String()] {
			t.Errorf("duplicate version ID %s", versionID)
		}
		seen[versionID.String()] = true
	}
}

func TestCrockfordEncodeProperties(t *testing.T) {
	encoded := crockfordEncode([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	if encoded != strings.Repeat("0", crockfordSymbols) {
		t.Errorf("zero bytes encode to %q, want %q", encoded, strings.Repeat("0", crockfordSymbols))
	}
	for _, symbol := range encoded {
		if !strings.ContainsRune(crockfordAlphabet, symbol) {
			t.Errorf("encoded symbol %q is outside the Crockford alphabet", symbol)
		}
	}
	// Equal bytes encode identically (deterministic hashing).
	first := crockfordEncode([]byte("identical input"))
	second := crockfordEncode([]byte("identical input"))
	if first != second {
		t.Errorf("crockfordEncode is not deterministic: %q != %q", first, second)
	}
}

func TestHashSourceIsDeterministicAndDistinct(t *testing.T) {
	first := HashSource("const a = 1;")
	second := HashSource("const a = 1;")
	if first != second {
		t.Errorf("equal sources hash differently: %s != %s", first, second)
	}
	if HashSource("const a = 2;") == first {
		t.Errorf("different sources share a hash")
	}
	// The hash must be a valid content identity so it can
	// travel inside the artifact contract.
	if _, err := domain.ParseContentID(first.String()); err != nil {
		t.Errorf("source hash %s is not a valid content identity: %v", first, err)
	}
	if first.Prefix() != domain.PrefixContent {
		t.Errorf("source hash prefix = %q, want %q", first.Prefix(), domain.PrefixContent)
	}
}

func TestInMemoryRegistryLifecycle(t *testing.T) {
	ctx := context.Background()
	clock := NewFixedClock(1000)
	registry := NewInMemoryRegistry(clock)
	sandbox := &FakeSandbox{}
	service := NewService(registry, sandbox, clock, &SequenceRandom{}, nil)
	owner := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	// Registration stores a candidate without activating it.
	version, err := service.RegisterCandidate(ctx, CandidateRequest{
		Manifest: testManifest("app", 1),
		Source:   testSource(),
		Owner:    owner,
		Scope:    domain.ScopeUser,
		Policy:   domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	artifact, err := registry.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if _, err := registry.Current(ctx, artifact.AppID); !domain.IsNotFoundError(err) {
		t.Fatalf("Current on a registered-but-inactive app = %v, want not-found", err)
	}
	if artifact.ActivatedAt != 0 {
		t.Errorf("candidate ActivatedAt = %d, want 0 before activation", artifact.ActivatedAt)
	}

	// Activation is atomic and stamps the activation event.
	clock.Advance(500)
	if _, err := service.Activate(ctx, ActivateRequest{AppID: artifact.AppID, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy()}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	current, err := registry.Current(ctx, artifact.AppID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != version {
		t.Errorf("current = %s, want %s", current, version)
	}
	activated, err := registry.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact after activation: %v", err)
	}
	if activated.ActivatedAt != 1500 {
		t.Errorf("activated ActivatedAt = %d, want 1500", activated.ActivatedAt)
	}
	history := registry.History()
	if len(history) != 1 || history[0].Kind != ActivationActivate || history[0].To != version || history[0].At != 1500 {
		t.Errorf("activation history = %+v, want one activation record at 1500", history)
	}

	// Idempotent activation does not append a record.
	if _, err := service.Activate(ctx, ActivateRequest{AppID: artifact.AppID, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy()}); err != nil {
		t.Fatalf("idempotent Activate: %v", err)
	}
	if len(registry.History()) != 1 {
		t.Errorf("idempotent activation appended %d records, want 1", len(registry.History()))
	}
}

func TestInMemoryRegistryRejectsInvalidOperations(t *testing.T) {
	ctx := context.Background()
	clock := NewFixedClock(0)
	registry := NewInMemoryRegistry(clock)
	owner := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	valid := domain.AppArtifact{
		AppID:      mustAppID(t, "app_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		VersionID:  mustVersionID(t, "av_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		Manifest:   testManifest("app", 1),
		Source:     testSource(),
		SourceHash: HashSource(testSource()),
		Owner:      owner,
		Scope:      domain.ScopeUser,
		CreatedAt:  0,
		Validation: domain.ValidationResult{Passed: true, ValidatedAt: 0, ValidatorVersion: ValidatorVersion},
	}

	if _, err := registry.RegisterCandidate(ctx, valid); err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}

	// Duplicate version IDs conflict.
	if _, err := registry.RegisterCandidate(ctx, valid); !domain.IsConflictError(err) {
		t.Errorf("duplicate registration = %v, want conflict", err)
	}

	// A hash mismatch is rejected.
	tampered := valid
	tampered.VersionID = mustVersionID(t, "av_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	tampered.Source = "changed"
	if _, err := registry.RegisterCandidate(ctx, tampered); !domain.IsValidationError(err) {
		t.Errorf("tampered source hash = %v, want validation error", err)
	}

	// Only validated candidates activate.
	unvalidated := valid
	unvalidated.VersionID = mustVersionID(t, "av_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	unvalidated.Source = testSource() + MarkerCompileError
	unvalidated.SourceHash = HashSource(unvalidated.Source)
	unvalidated.Validation = domain.ValidationResult{Passed: false, Issues: []string{"staging smoke test failed"}, ValidatedAt: 0, ValidatorVersion: ValidatorVersion}
	if _, err := registry.RegisterCandidate(ctx, unvalidated); err != nil {
		t.Fatalf("RegisterCandidate (unvalidated): %v", err)
	}
	if err := registry.Activate(ctx, valid.AppID, unvalidated.VersionID); !domain.IsValidationError(err) {
		t.Errorf("activating an unvalidated candidate = %v, want validation error", err)
	}

	// Unknown versions and cross-app versions are rejected.
	if err := registry.Activate(ctx, valid.AppID, mustVersionID(t, "av_01ARZ3NDEKTSV4RRFFQ69G5FCH")); !domain.IsNotFoundError(err) {
		t.Errorf("activating an unknown version = %v, want not-found", err)
	}
	if err := registry.Activate(ctx, mustAppID(t, "app_01ARZ3NDEKTSV4RRFFQ69G5FBW"), valid.VersionID); !domain.IsValidationError(err) {
		t.Errorf("activating a cross-app version = %v, want validation error", err)
	}

	// Rollback requires a previously accepted version.
	if err := registry.Rollback(ctx, valid.AppID, unvalidated.VersionID, "because"); !domain.IsValidationError(err) {
		t.Errorf("rollback to an unaccepted version = %v, want validation error", err)
	}
	if err := registry.Activate(ctx, valid.AppID, valid.VersionID); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Rolling back to the current version is an idempotent no-op.
	if err := registry.Rollback(ctx, valid.AppID, valid.VersionID, "because"); err != nil {
		t.Errorf("rollback to the current version = %v, want nil", err)
	}
}

func mustUserID(t *testing.T, value string) domain.UserID {
	t.Helper()
	id, err := domain.ParseUserID(value)
	if err != nil {
		t.Fatalf("ParseUserID(%q): %v", value, err)
	}
	return id
}

func mustAppID(t *testing.T, value string) domain.AppID {
	t.Helper()
	id, err := domain.ParseAppID(value)
	if err != nil {
		t.Fatalf("ParseAppID(%q): %v", value, err)
	}
	return id
}

func mustVersionID(t *testing.T, value string) domain.AppVersionID {
	t.Helper()
	id, err := domain.ParseAppVersionID(value)
	if err != nil {
		t.Fatalf("ParseAppVersionID(%q): %v", value, err)
	}
	return id
}

func mustSessionID(t *testing.T, value string) domain.SessionID {
	t.Helper()
	id, err := domain.ParseSessionID(value)
	if err != nil {
		t.Fatalf("ParseSessionID(%q): %v", value, err)
	}
	return id
}
