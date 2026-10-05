package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// ValidatorVersion identifies the validator that produced a
// ValidationResult. It travels inside the immutable artifact so
// research can tell which validator accepted a version.
const ValidatorVersion = "vibeshell-app-validator/1"

// Artifact bounds from schemas/app-abi.schema.json. The domain
// manifest validation covers structure; these bound the same
// fields at the service boundary so oversized candidates fail
// fast with actionable errors.
const (
	// MaxSourceBytes bounds the JavaScript source (schema
	// maxLength 1048576).
	MaxSourceBytes = 1 << 20
	// MaxCommandNames bounds the command name and alias count
	// (schema maxItems 16).
	MaxCommandNames = 16
	// MaxCommandNameLen bounds one command name or alias
	// (schema maxLength 64).
	MaxCommandNameLen = 64
	// MaxDescriptionLen bounds the human-readable description
	// (schema maxLength 1024).
	MaxDescriptionLen = 1024
)

// Staging sandbox limits for candidate validation. Validation
// runs the candidate once against empty state under tight
// bounds: a short deadline, small memory and output caps, and a
// shallow event queue. The same engine limits apply to real
// executions; staging only narrows the deadline.
var stagingLimits = ports.SandboxLimits{
	DeadlineMs:  250,
	MaxMemoryB:  8 << 20, // 8 MiB
	MaxOutputB:  64 << 10,
	MaxEvents:   16,
	SeededRand:  0x5eed, // fixed seed: staging is reproducible
	SimNowMilli: 0,      // replaced per run from the injected clock
}

// SmokeTestName is the single test every candidate must pass
// before registration: a minimal input event against empty
// state.
const SmokeTestName = "smoke"

// CandidateRequest describes a new application version. A zero
// AppID creates a new application; a non-zero AppID with a
// ParentVersion extends an existing one. Policy is the
// requesting session's sharing policy: registering a
// shared-scoped app requires an enabled sharing policy.
type CandidateRequest struct {
	AppID         domain.AppID
	ParentVersion *domain.AppVersionID
	Manifest      domain.AppManifest
	Source        string
	Owner         domain.UserID
	Scope         domain.Scope
	Provenance    domain.Provenance
	Policy        domain.ScopePolicy
}

// validateCandidateShape checks request-level rules
// before any ID is minted: an extension names its
// parent and app, a new app names neither.
func validateCandidateShape(req CandidateRequest) []string {
	var issues []string
	if req.ParentVersion != nil && req.ParentVersion.IsZero() {
		issues = append(issues, "parent_version must be a valid version when set")
	}
	if req.ParentVersion != nil && req.AppID.IsZero() {
		issues = append(issues, "extending an app requires its app_id")
	}
	if req.ParentVersion == nil && !req.AppID.IsZero() {
		issues = append(issues, "app_id is only set when extending an existing app")
	}
	return issues
}

// validateArtifact checks an artifact against the
// application artifact contract: manifest bounds,
// source bounds, owner, and scope.
func validateArtifact(artifact domain.AppArtifact) []string {
	var issues []string
	if err := domain.ValidateManifest(artifact.Manifest); err != nil {
		issues = append(issues, "manifest: "+err.Error())
	}
	if len(artifact.Manifest.CommandNames) > MaxCommandNames {
		issues = append(issues, fmt.Sprintf("manifest: at most %d command names and aliases", MaxCommandNames))
	}
	for _, name := range artifact.Manifest.CommandNames {
		if len(name) > MaxCommandNameLen {
			issues = append(issues, fmt.Sprintf("manifest: command name %q exceeds %d bytes", name, MaxCommandNameLen))
			break
		}
	}
	if len(artifact.Manifest.Description) > MaxDescriptionLen {
		issues = append(issues, fmt.Sprintf("manifest: description exceeds %d bytes", MaxDescriptionLen))
	}
	if artifact.Manifest.StateSchema == nil {
		issues = append(issues, "manifest: state_schema is required")
	} else if !json.Valid(artifact.Manifest.StateSchema) {
		issues = append(issues, "manifest: state_schema is not valid JSON")
	}
	if artifact.Manifest.Version == "" {
		issues = append(issues, "manifest: version is required")
	}
	if artifact.Source == "" {
		issues = append(issues, "source is required")
	}
	if len(artifact.Source) > MaxSourceBytes {
		issues = append(issues, fmt.Sprintf("source exceeds %d bytes", MaxSourceBytes))
	}
	if artifact.Owner.IsZero() {
		issues = append(issues, "owner is required")
	}
	switch artifact.Scope {
	case domain.ScopeUser, domain.ScopeShared:
		// Generated applications are durable user or
		// shared artifacts. Session state is not an
		// app scope, and baseline seeds come from
		// world initialization, not generation.
	default:
		issues = append(issues, fmt.Sprintf("scope must be user or shared, not %q", artifact.Scope))
	}
	if artifact.Scope == domain.ScopeShared && strings.TrimSpace(artifact.Manifest.Description) == "" {
		issues = append(issues, "shared apps require a description")
	}
	return issues
}

// stageValidation runs the candidate's static checks and its
// isolated staging smoke test, returning the validation record
// that becomes part of the immutable artifact. A candidate never
// reaches the registry unless this function has run: partially
// streamed or syntactically invalid source is rejected here,
// not inside a working activation.
func stageValidation(
	ctx context.Context,
	sandbox ports.AppSandbox,
	clock ports.Clock,
	artifact domain.AppArtifact,
) domain.ValidationResult {
	now := clock.NowUnixMilli()
	result := domain.ValidationResult{
		Passed:           false,
		ValidatedAt:      now,
		ValidatorVersion: ValidatorVersion,
	}

	issues := validateArtifact(artifact)
	if want := HashSource(artifact.Source); want != artifact.SourceHash {
		issues = append(issues, "source_hash does not match the source")
	}

	smoke := domain.TestResult{
		Name:       SmokeTestName,
		DurationMs: 0,
	}
	if len(issues) == 0 {
		// The staging run sees an isolated, empty state: a
		// candidate must work from a clean slate before it can
		// be registered, let alone activated.
		limits := stagingLimits
		limits.SimNowMilli = now
		event := domain.AppEvent{
			EventType: domain.AppEventInput,
			Payload:   json.RawMessage(`{"smoke":true}`),
			Timestamp: now,
		}
		runStart := clock.NowUnixMilli()
		runResult, err := sandbox.Run(ctx, artifact, domain.AppState{}, event, limits)
		smoke.DurationMs = clock.NowUnixMilli() - runStart
		if err != nil {
			issues = append(issues, "staging smoke test failed: "+err.Error())
		} else if err := domain.ValidateResult(runResult); err != nil {
			issues = append(issues, "staging smoke test returned an invalid result: "+err.Error())
		} else {
			smoke.Passed = true
			smoke.Output = runResult.View.StatusLine
		}
	} else {
		smoke.Output = "skipped: static validation failed"
	}
	result.Passed = len(issues) == 0
	result.Issues = issues
	result.TestResults = []domain.TestResult{smoke}
	return result
}

// invalidCandidateError turns collected issues into one
// actionable validation error.
func invalidCandidateError(issues []string) error {
	if len(issues) == 0 {
		return nil
	}
	return domain.NewValidationError(
		domain.CodeInvalidAppManifest,
		"candidate artifact is not registrable",
		map[string]string{"issues": strings.Join(issues, "; ")},
	)
}
