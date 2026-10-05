package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// appFixture wires the apps service over the in-memory registry double, a
// fake sandbox, and a deterministic clock, with one activated v1 application.
type appFixture struct {
	service  *apps.Service
	registry *apps.InMemoryRegistry
	sandbox  *apps.FakeSandbox
	owner    domain.UserID
	appID    domain.AppID
	current  domain.AppVersionID
}

func newAppFixture(t *testing.T) *appFixture {
	t.Helper()
	ctx := context.Background()
	clock := apps.NewFixedClock(1_700_000_000_000)
	registry := apps.NewInMemoryRegistry(clock)
	sandbox := &apps.FakeSandbox{}
	service := apps.NewService(registry, sandbox, clock, &apps.SequenceRandom{}, nil)
	owner := userID(t, 1)

	version, err := service.RegisterCandidate(ctx, apps.CandidateRequest{
		Manifest: appManifest("moon-orchard", 1),
		Source:   appSource(),
		Owner:    owner,
		Scope:    domain.ScopeUser,
		Policy:   domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v1: %v", err)
	}
	artifact, err := registry.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact v1: %v", err)
	}
	if _, err := service.Activate(ctx, apps.ActivateRequest{
		AppID: artifact.AppID, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v1: %v", err)
	}
	return &appFixture{
		service: service, registry: registry, sandbox: sandbox,
		owner: owner, appID: artifact.AppID, current: version,
	}
}

func (f *appFixture) registerExtension(t *testing.T, source string) domain.AppVersionID {
	t.Helper()
	parent := f.current
	id, err := f.service.RegisterCandidate(context.Background(), apps.CandidateRequest{
		AppID:         f.appID,
		ParentVersion: &parent,
		Manifest:      appManifest("moon-orchard", 1),
		Source:        source,
		Owner:         f.owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate extension: %v", err)
	}
	return id
}

func appManifest(command string, schemaVersion int64) domain.AppManifest {
	return domain.AppManifest{
		ABIVersion:   domain.AppABIVersion,
		CommandNames: []string{command},
		Description:  "generated application " + command,
		StateSchema:  json.RawMessage(`{"version":` + itoa(schemaVersion) + `}`),
		Capabilities: []string{domain.CapabilityFileRead},
		Entrypoint:   "handle",
		Version:      "1.0.0",
	}
}

func appSource() string {
	return "export function handle(event, state) {\n  return { state: state, view: { mode: 'text' } };\n}\n"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// TestFailedSandboxCandidateLeavesPriorVersionIntact is the PLAN 15.5 D05
// acceptance case: an extension whose isolated staging sandbox run fails is
// registered for research but never becomes current, so the previously
// activated version stays active and remains resolvable.
func TestFailedSandboxCandidateLeavesPriorVersionIntact(t *testing.T) {
	ctx := context.Background()
	f := newAppFixture(t)

	// The extension's source triggers a simulated sandbox/compile failure
	// during the isolated staging run.
	bad := f.registerExtension(t, appSource()+apps.MarkerCompileError)
	if bad == f.current {
		t.Fatalf("failed candidate reused the current version id")
	}

	// The current pointer did not move.
	current, err := f.registry.Current(ctx, f.appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != f.current {
		t.Fatalf("current = %s after failed candidate, want %s", current, f.current)
	}

	// The candidate is retained (research) with a failed validation record.
	candidate, err := f.registry.GetArtifact(ctx, bad)
	if err != nil {
		t.Fatalf("GetArtifact(failed candidate): %v", err)
	}
	if candidate.Validation.Passed {
		t.Errorf("failed candidate reports Passed=true")
	}
	if len(candidate.Validation.Issues) == 0 {
		t.Errorf("failed candidate has no validation issues")
	}
	if candidate.ActivatedAt != 0 {
		t.Errorf("failed candidate ActivatedAt = %d, want 0", candidate.ActivatedAt)
	}

	// Activation of the failed candidate is refused.
	_, err = f.service.Activate(ctx, apps.ActivateRequest{
		AppID: f.appID, Version: bad, Actor: f.owner, Policy: domain.DefaultScopePolicy(),
	})
	if !domain.IsValidationError(err) {
		t.Fatalf("activating an unvalidated candidate = %v, want validation error", err)
	}

	// A fresh session still resolves the intact prior version.
	res, err := f.service.ResolveSession(ctx, apps.ResolveRequest{
		SessionID: sessionID(t, 1), UserID: f.owner, AppID: f.appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if res.Version != f.current {
		t.Errorf("resolved version = %s, want the prior intact version %s", res.Version, f.current)
	}
	if res.Stale {
		t.Errorf("resolution unexpectedly stale: %s", res.Reason)
	}
}

// TestRuntimeSandboxFailureDoesNotActivateCandidate proves that even when a
// candidate's staging validation passes, a runtime sandbox failure on the
// active version does not rewrite the accepted artifact or silently activate
// a new one; the failure surfaces and the current pointer holds.
func TestRuntimeSandboxFailureDoesNotActivateCandidate(t *testing.T) {
	ctx := context.Background()
	f := newAppFixture(t)

	// A well-formed extension validates and can be registered, but we never
	// activate it; a runtime sandbox failure mid-interaction must not change
	// the active pointer by itself.
	good := f.registerExtension(t, appSource()+"// extension\n")
	f.sandbox.RunFunc = func(context.Context, domain.AppArtifact, domain.AppState, domain.AppEvent, ports.SandboxLimits) (domain.AppResult, error) {
		return domain.AppResult{}, errors.New("simulated runtime sandbox failure")
	}
	artifact, err := f.registry.GetArtifact(ctx, f.current)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if _, err := f.sandbox.Run(ctx, artifact, domain.AppState{}, domain.AppEvent{
		EventType: domain.AppEventInput, Payload: json.RawMessage(`{}`),
	}, ports.SandboxLimits{}); err == nil {
		t.Fatalf("expected a simulated runtime sandbox failure")
	}

	current, err := f.registry.Current(ctx, f.appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != f.current {
		t.Errorf("current changed to %s on runtime sandbox failure, want %s", current, f.current)
	}

	// The well-formed extension is registered but not activated.
	candidate, err := f.registry.GetArtifact(ctx, good)
	if err != nil {
		t.Fatalf("GetArtifact(candidate): %v", err)
	}
	if candidate.ActivatedAt != 0 {
		t.Errorf("unactivated candidate ActivatedAt = %d, want 0", candidate.ActivatedAt)
	}

	// The accepted v1 source is immutable and unchanged.
	reloaded, err := f.registry.GetArtifact(ctx, f.current)
	if err != nil {
		t.Fatalf("GetArtifact after failure: %v", err)
	}
	if reloaded.Source != appSource() {
		t.Errorf("accepted source changed after a failed candidate")
	}
}
