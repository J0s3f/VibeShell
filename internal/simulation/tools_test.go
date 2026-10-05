package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func mustParseContentID(t *testing.T, s string) domain.ContentID {
	t.Helper()
	id, err := domain.ParseContentID(s)
	if err != nil {
		t.Fatalf("parse content id %q: %v", s, err)
	}
	return id
}

// seedApp registers an accepted artifact owned by owner with the given scope
// and makes it the current version of its app.
func seedApp(t *testing.T, apps *fakeApps, owner domain.UserID, scope domain.Scope) domain.AppArtifact {
	t.Helper()
	artifact := domain.AppArtifact{
		AppID:      testApp,
		VersionID:  testAppVer,
		Owner:      owner,
		Scope:      scope,
		Source:     "function main() { return { mode: 'text' }; }",
		SourceHash: mustParseContentID(t, "cnt_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		Manifest: domain.AppManifest{
			ABIVersion:   domain.AppABIVersion,
			CommandNames: []string{"demo", "dm"},
			Entrypoint:   "main",
			Description:  "an invented demo program",
		},
		CreatedAt: 1_700_000_000_000,
	}
	apps.seed(artifact)
	return artifact
}

// TestAppLookupSource verifies app lookup returns the immutable source and
// command names for the caller's own artifact.
func TestAppLookupSource(t *testing.T) {
	registry, _, _, _, _, apps, _, _, _, _ := newTestRegistry()
	seedApp(t, apps, testUser, domain.ScopeUser)
	call := testCallContext()

	result := mustExec(t, registry, call, "app.lookup", map[string]any{
		"app_id": testApp.String(),
	})
	var lookup appLookupResult
	if err := json.Unmarshal(result, &lookup); err != nil {
		t.Fatalf("unmarshal app lookup: %v", err)
	}
	if lookup.VersionID != testAppVer {
		t.Errorf("version = %v, want %v", lookup.VersionID, testAppVer)
	}
	if lookup.Source == "" {
		t.Error("source is empty")
	}
	if len(lookup.CommandNames) != 2 || lookup.CommandNames[0] != "demo" {
		t.Errorf("command names = %v, want [demo dm]", lookup.CommandNames)
	}
	if lookup.Scope != domain.ScopeUser {
		t.Errorf("scope = %v, want user", lookup.Scope)
	}
}

// TestAppLookupScopeDenied verifies another user's artifact is denied while
// sharing is disabled.
func TestAppLookupScopeDenied(t *testing.T) {
	registry, _, _, _, _, apps, _, _, _, _ := newTestRegistry()
	seedApp(t, apps, testUser2, domain.ScopeUser)

	err := mustExecErr(t, registry, sharingOffCall(), "app.lookup", map[string]any{
		"app_id": testApp.String(),
	})
	if !domain.IsDeniedError(err) {
		t.Fatalf("cross-user app lookup: error = %v, want denied", err)
	}
}

// TestAppCandidateRegistersWithoutActivating verifies a candidate version is
// validated, trial-run in the sandbox, and registered without activation.
func TestAppCandidateRegistersWithoutActivating(t *testing.T) {
	registry, _, _, _, _, apps, sandbox, _, _, _ := newTestRegistry()
	seedApp(t, apps, testUser, domain.ScopeUser)
	sandbox.result = domain.AppResult{View: domain.AppView{Mode: domain.AppViewModeText}}
	call := testCallContext()

	result := mustExec(t, registry, call, "app.candidate", map[string]any{
		"app_id": testApp.String(),
		"source": "function main() { return { mode: 'text' }; }",
		"manifest": domain.AppManifest{
			ABIVersion:   domain.AppABIVersion,
			CommandNames: []string{"demo"},
			Entrypoint:   "main",
		},
		"test_event": domain.AppEvent{EventType: domain.AppEventInput},
	})
	var candidate appCandidateResult
	if err := json.Unmarshal(result, &candidate); err != nil {
		t.Fatalf("unmarshal candidate: %v", err)
	}
	if candidate.Activated {
		t.Error("candidate must not be activated by the tool")
	}
	if candidate.ParentVersion != testAppVer {
		t.Errorf("parent version = %v, want %v", candidate.ParentVersion, testAppVer)
	}
	if candidate.VersionID.IsZero() {
		t.Error("candidate version ID is zero")
	}
	if candidate.Trial == nil || !candidate.Trial.Passed {
		t.Fatalf("trial = %+v, want a passing trial", candidate.Trial)
	}
	if len(sandbox.runs) != 1 {
		t.Errorf("sandbox runs = %d, want 1", len(sandbox.runs))
	}
	// The active version is unchanged.
	current, err := apps.Current(context.Background(), testApp)
	if err != nil {
		t.Fatalf("current version: %v", err)
	}
	if current != testAppVer {
		t.Errorf("current version = %v, want %v (unchanged)", current, testAppVer)
	}
}

// TestAppCandidateRejectsInvalidManifest verifies manifest validation happens
// before any candidate is stored.
func TestAppCandidateRejectsInvalidManifest(t *testing.T) {
	registry, _, _, _, _, apps, _, _, _, _ := newTestRegistry()
	seedApp(t, apps, testUser, domain.ScopeUser)

	err := mustExecErr(t, registry, testCallContext(), "app.candidate", map[string]any{
		"app_id": testApp.String(),
		"source": "x",
		"manifest": domain.AppManifest{
			ABIVersion:   0, // unsupported
			CommandNames: []string{"demo"},
			Entrypoint:   "main",
		},
	})
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != CodeAppCandidateInvalid {
		t.Fatalf("invalid manifest: error = %v, want code %q", err, CodeAppCandidateInvalid)
	}
}

// TestInteractionPropose verifies a valid declarative interaction is
// accepted and an unapproved view is rejected.
func TestInteractionPropose(t *testing.T) {
	registry, _, _, _, _, _, _, _, _, _ := newTestRegistry()
	call := testCallContext()

	result := mustExec(t, registry, call, "interaction.propose", map[string]any{
		"view":   domain.AppView{Mode: domain.AppViewModePager},
		"reason": "show a long file",
	})
	var proposal interactionProposeResult
	if err := json.Unmarshal(result, &proposal); err != nil {
		t.Fatalf("unmarshal proposal: %v", err)
	}
	if proposal.ProposalID == "" || proposal.Status != "proposed" {
		t.Errorf("proposal = %+v, want an id and proposed status", proposal)
	}

	err := mustExecErr(t, registry, call, "interaction.propose", map[string]any{
		"view": domain.AppView{Mode: "raw-terminal"},
	})
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != CodeInteractionInvalid {
		t.Fatalf("invalid view: error = %v, want code %q", err, CodeInteractionInvalid)
	}
}
