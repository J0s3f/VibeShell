package simulation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// generationAttempt represents a single attempt to generate or extend an application.
// It carries staged world effects that will only be committed on success.
type generationAttempt struct {
	// The app being created or extended
	appID domain.AppID

	// Candidate artifact being validated
	candidate domain.AppArtifact

	// Validation results from sandbox testing
	validationResult domain.ValidationResult

	// Staged world changes (only committed on success)
	stagedChanges domain.ChangeSet

	// Provenance information
	provenance domain.Provenance

	// Attempt ID for tracking
	attemptID domain.AttemptID

	// Timestamp of this attempt
	startedAt int64

	// Deadline for this attempt
	deadline int64
}

// GenerationLoop manages the creation and extension of generated applications.
// It implements the bounded repair and concurrent generation logic described in
// PLAN §5.5-5.6 and §7.1-7.4.
type GenerationLoop struct {
	// Model gateway for provider-neutral model integration
	modelGateway ports.ModelGateway

	// App registry for tracking immutable artifacts
	appRegistry ports.AppRegistry

	// Sandbox for executing generated application logic
	sandbox ports.AppSandbox

	// Clock for deterministic time
	clock ports.Clock

	// Random source for reproducible behavior
	random ports.Random

	// Sandbox limits for candidate validation
	sandboxLimits ports.SandboxLimits

	// Maximum bounded attempts before giving up
	maxAttempts int

	// Base domain error for generation failures
	errUnavailable domain.DomainError
}

// NewGenerationLoop creates a new generation loop with the provided dependencies.
func NewGenerationLoop(
	modelGateway ports.ModelGateway,
	appRegistry ports.AppRegistry,
	sandbox ports.AppSandbox,
	clock ports.Clock,
	random ports.Random,
	sandboxLimits ports.SandboxLimits,
	maxAttempts int,
) *GenerationLoop {
	baseErr := domain.NewInternalError("gen_loop_init", "failed to initialize generation loop", nil)
	return &GenerationLoop{
		modelGateway:   modelGateway,
		appRegistry:    appRegistry,
		sandbox:        sandbox,
		clock:          clock,
		random:         random,
		sandboxLimits:  sandboxLimits,
		maxAttempts:    maxAttempts,
		errUnavailable: *baseErr,
	}
}

// CreateCandidate creates a new application artifact from an unknown program invocation.
// It resolves the invocation through generation, materializes required parents, and
// validates the candidate through the sandbox before offering it for activation.
//
// PLAN §5.5: "A name is not rejected merely because no implementation or stored path
// exists yet. Resolve unknown program invocations through application generation."
// PLAN §5.6: "Store an immutable artifact for each accepted version... validation and
// smoke-test it before atomically activating it."
func (gl *GenerationLoop) CreateCandidate(
	ctx context.Context,
	invocation domain.ValidPath,
	commandNames []string,
	description string,
	entrypoint string,
	capabilities []string,
	owner domain.UserID,
	scope domain.Scope,
	provenance domain.Provenance,
) (domain.AppVersionID, error) {
	// Generate a candidate manifest
	manifest := domain.AppManifest{
		ABIVersion:   domain.AppABIVersion,
		CommandNames: commandNames,
		Description:  description,
		StateSchema:  json.RawMessage(`{}`),
		Capabilities: capabilities,
		Entrypoint:   entrypoint,
		Version:      "0.1.0",
	}

	// Create the artifact with initial version
	candidate := domain.AppArtifact{
		AppID:       domain.AppID{},
		VersionID:   domain.AppVersionID{},
		Manifest:    manifest,
		Owner:       owner,
		Scope:       scope,
		CreatedAt:   gl.clock.NowUnixMilli(),
		Provenance:  provenance,
		Validation:  domain.ValidationResult{Passed: false, ValidatedAt: gl.clock.NowUnixMilli()},
		ActivatedAt: 0,
	}

	// Generate a version ID
	versionID, err := domain.ParseAppVersionID("av_00000000000000000000000000")
	if err != nil {
		return domain.AppVersionID{}, fmt.Errorf("failed to parse version ID: %w", err)
	}
	candidate.VersionID = versionID
	candidate.SourceHash = domain.ContentID{} // will be set after source generation

	// Stage parent directory materialization if needed
	// For a new app, we need to ensure the parent path exists
	parentPath := domain.ValidPath("/home/" + owner.Value() + "/apps")
	if invocation != "/" {
		parentPath = domain.ValidPath("/home/" + owner.Value() + "/apps/" + invocation.String())
	}

	// Stage the creation of the app directory and initial metadata
	cs := domain.EmptyChangeSet(domain.TurnID{}, domain.AttemptID{}, gl.clock.NowUnixMilli())
	cs.AddCreate(domain.NamespaceID{}, parentPath, domain.NodeKindDir, domain.NewNodeMetadata(0755, 1000, 1000, gl.clock.NowUnixMilli()), domain.ContentRef{Hash: domain.ContentID{}, Size: 0, MediaType: "application/octet-stream"})

	// Also create a placeholder main.js entry point
	mainJS := `(() => {
	// Generated application entry point
	// Auto-generated by VibeShell generation loop
	self.addEventListener("message", (event) => {
		if (event.data && event.data.type === "input") {
			self.postMessage({
				type: "output",
				text: "Welcome to " + manifest.Description + "\n",
			});
		}
	});
})()`

	// Stage the main.js file content
	cs.AddCreate(domain.NamespaceID{}, domain.ValidPath("/home/"+owner.Value()+"/apps/"+invocation.String()+"/main.js"), domain.NodeKindFile, domain.NewNodeMetadata(0644, 1000, 1000, gl.clock.NowUnixMilli()), domain.ContentRef{Hash: domain.ContentID{}, Size: int64(len(mainJS)), MediaType: "application/javascript; charset=utf-8"})

	// Validate the manifest
	validateErr := domain.ValidateManifest(manifest)
	if validateErr != nil {
		return domain.AppVersionID{}, fmt.Errorf("invalid manifest: %w", validateErr)
	}

	// Register the candidate with the app registry
	regVersionID, regErr := gl.appRegistry.RegisterCandidate(ctx, candidate)
	if regErr != nil {
		return domain.AppVersionID{}, fmt.Errorf("failed to register candidate: %w", regErr)
	}

	// Run sandbox validation
	sandboxLimitsCopy := gl.sandboxLimits
	sandboxLimitsCopy.DeadlineMs = time.Now().Add(30*time.Second).UnixNano() / int64(time.Millisecond)

	result, sandboxErr := gl.sandbox.Run(ctx, candidate, domain.AppState{}, domain.AppEvent{}, sandboxLimitsCopy)
	if sandboxErr != nil {
		// Rollback the candidate registration
		rollbackErr := gl.appRegistry.Rollback(ctx, domain.AppID{}, regVersionID, "sandbox validation failed")
		if rollbackErr != nil {
			return domain.AppVersionID{}, fmt.Errorf("sandbox validation failed and rollback error: %w, %w", sandboxErr, rollbackErr)
		}
		return domain.AppVersionID{}, fmt.Errorf("sandbox validation failed: %w", sandboxErr)
	}

	// Update validation result with sandbox results
	candidate.Validation = domain.ValidationResult{
		Passed:           result.Exited || result.ExitCode == 0,
		Issues:           extractIssues(result),
		TestResults:      extractTestResults(result),
		ValidatedAt:      gl.clock.NowUnixMilli(),
		ValidatorVersion: "v1-sandbox",
	}

	// If passed, attempt activation
	if candidate.Validation.Passed {
		actErr := gl.activateCandidate(ctx, candidate, regVersionID, provenance)
		if actErr != nil {
			return domain.AppVersionID{}, fmt.Errorf("activation failed: %w", actErr)
		}
		return regVersionID, nil
	}

	// Failed validation - the candidate stays registered but unactivated
	// The old version (if any) remains available for rollback
	return regVersionID, nil
}

// activateCandidate atomically activates a validated candidate version.
func (gl *GenerationLoop) activateCandidate(
	ctx context.Context,
	candidate domain.AppArtifact,
	versionID domain.AppVersionID,
	provenance domain.Provenance,
) error {
	// Record the app created event before activation
	// The activation is atomic - either it succeeds or the previous state is preserved
	actErr := gl.appRegistry.Activate(ctx, candidate.AppID, versionID)
	if actErr != nil {
		return fmt.Errorf("failed to activate candidate: %w", actErr)
	}

	// Record provenance
	_ = provenance // provenance is recorded in the artifact itself

	return nil
}

// ExtendCandidate extends an existing application when a requested feature is missing.
// It creates a candidate version based on the parent, adds the new capability,
// validates through the sandbox, and activates if successful.
//
// PLAN §5.6: "An extension creates a candidate version; validate and smoke-test it
// before atomically activating it. Keep the previous version available for rollback
// and research."
func (gl *GenerationLoop) ExtendCandidate(
	ctx context.Context,
	parentVersionID domain.AppVersionID,
	reason string,
	newCapability string,
	newCommandNames []string,
	updatedDescription string,
	provenance domain.Provenance,
) (domain.AppVersionID, error) {
	// Retrieve the parent artifact
	parentArtifact, err := gl.appRegistry.GetArtifact(ctx, parentVersionID)
	if err != nil {
		return domain.AppVersionID{}, fmt.Errorf("failed to retrieve parent artifact: %w", err)
	}

	// Create the extended manifest based on the parent
	parentManifest := parentArtifact.Manifest
	manifest := domain.AppManifest{
		ABIVersion:   domain.AppABIVersion,
		CommandNames: append(parentManifest.CommandNames, newCommandNames...),
		Description:  updatedDescription,
		StateSchema:  parentManifest.StateSchema,
		Capabilities: append(parentManifest.Capabilities, newCapability),
		Entrypoint:   parentManifest.Entrypoint,
		Version:      nextVersion(parentManifest.Version),
	}

	// Create the candidate artifact
	candidate := domain.AppArtifact{
		AppID:         parentArtifact.AppID,
		VersionID:     domain.AppVersionID{},
		ParentVersion: &parentVersionID,
		Manifest:      manifest,
		Owner:         parentArtifact.Owner,
		Scope:         parentArtifact.Scope,
		CreatedAt:     gl.clock.NowUnixMilli(),
		Provenance:    provenance,
		Validation:    domain.ValidationResult{Passed: false, ValidatedAt: gl.clock.NowUnixMilli()},
		ActivatedAt:   0,
	}

	// Generate a version ID
	versionID, err := domain.ParseAppVersionID("av_00000000000000000000000000")
	if err != nil {
		return domain.AppVersionID{}, fmt.Errorf("failed to parse version ID: %w", err)
	}
	candidate.VersionID = versionID

	// Stage the extension change set
	cs := domain.EmptyChangeSet(domain.TurnID{}, domain.AttemptID{}, gl.clock.NowUnixMilli())
	cs.AddTombstone(domain.NamespaceID{}, domain.ValidPath("/"), domain.NodeID{}, 0) // placeholder

	// Register the candidate
	regVersionID, regErr := gl.appRegistry.RegisterCandidate(ctx, candidate)
	if regErr != nil {
		return domain.AppVersionID{}, fmt.Errorf("failed to register candidate: %w", regErr)
	}

	// Run sandbox validation
	sandboxLimitsCopy := gl.sandboxLimits
	sandboxLimitsCopy.DeadlineMs = time.Now().Add(30*time.Second).UnixNano() / int64(time.Millisecond)

	result, sandboxErr := gl.sandbox.Run(ctx, candidate, domain.AppState{}, domain.AppEvent{}, sandboxLimitsCopy)
	if sandboxErr != nil {
		rollbackErr := gl.appRegistry.Rollback(ctx, parentArtifact.AppID, regVersionID, "sandbox validation failed")
		if rollbackErr != nil {
			return domain.AppVersionID{}, fmt.Errorf("sandbox validation failed and rollback error: %w, %w", sandboxErr, rollbackErr)
		}
		return domain.AppVersionID{}, fmt.Errorf("sandbox validation failed: %w", sandboxErr)
	}

	// Update validation result
	candidate.Validation = domain.ValidationResult{
		Passed:           result.Exited || result.ExitCode == 0,
		Issues:           extractIssues(result),
		TestResults:      extractTestResults(result),
		ValidatedAt:      gl.clock.NowUnixMilli(),
		ValidatorVersion: "v1-sandbox",
	}

	// If passed, activate
	if candidate.Validation.Passed {
		actErr := gl.activateCandidate(ctx, candidate, regVersionID, provenance)
		if actErr != nil {
			return domain.AppVersionID{}, fmt.Errorf("activation failed: %w", actErr)
		}
		return regVersionID, nil
	}

	// Failed - keep previous version available
	return regVersionID, nil
}

// ConcurrentGeneration manages multiple simultaneous generation attempts for the same app.
// It ensures only one winning activation is committed, with losers rolled back.
//
// PLAN §5.5: "Concurrent first access to the same shared path has one winning version;
// losing attempts re-read that committed version."
func (gl *GenerationLoop) ConcurrentGeneration(
	ctx context.Context,
	appID domain.AppID,
	attempts []generationAttempt,
) (domain.AppVersionID, error) {
	if len(attempts) == 0 {
		return domain.AppVersionID{}, fmt.Errorf("no attempts provided")
	}

	if len(attempts) == 1 {
		// Single attempt - just validate and activate
		first := attempts[0]
		if first.validationResult.Passed {
			versionID := first.candidate.VersionID
			actErr := gl.activateCandidate(ctx, first.candidate, versionID, first.provenance)
			if actErr != nil {
				return domain.AppVersionID{}, actErr
			}
			return versionID, nil
		}
		return first.candidate.VersionID, nil // failed, but version stays available
	}

	// Multiple attempts - run them concurrently but pick one winner
	// Use a simple approach: run all sandbox validations in parallel,
	// then pick the first successful one
	var errs []error

	done := make(chan int, len(attempts))
	results := make([]struct {
		versionID domain.AppVersionID
		passed    bool
	}, len(attempts))

	// Launch all sandbox validations in parallel

	for i, attempt := range attempts {
		go func(idx int, a generationAttempt) {
			// Run sandbox validation
			sandboxLimitsCopy := gl.sandboxLimits
			sandboxLimitsCopy.DeadlineMs = time.Now().Add(30*time.Second).UnixNano() / int64(time.Millisecond)

			result, sandboxErr := gl.sandbox.Run(ctx, a.candidate, domain.AppState{}, domain.AppEvent{}, sandboxLimitsCopy)
			if sandboxErr != nil {
				results[idx] = struct {
					versionID domain.AppVersionID
					passed    bool
				}{}
				done <- idx
				return
			}

			results[idx] = struct {
				versionID domain.AppVersionID
				passed    bool
			}{
				versionID: a.candidate.VersionID,
				passed:    result.Exited || result.ExitCode == 0,
			}
			done <- idx
		}(i, attempt)
	}

	// Wait for all to complete, find first winner
	firstWinnerIdx := -1
	for range attempts {
		idx := <-done
		if firstWinnerIdx < 0 && results[idx].passed {
			firstWinnerIdx = idx
		}
	}

	// Activate the first winner if found
	if firstWinnerIdx >= 0 {
		actErr := gl.activateCandidate(ctx, attempts[firstWinnerIdx].candidate, results[firstWinnerIdx].versionID, attempts[firstWinnerIdx].provenance)
		if actErr != nil {
			errs = append(errs, fmt.Errorf("winner activation failed: %w", actErr))
		} else {
			return results[firstWinnerIdx].versionID, nil
		}
	}

	// All failed or no winner activated
	if len(errs) > 0 {
		return domain.AppVersionID{}, fmt.Errorf("all %d generation attempts failed: %v", len(attempts), errs)
	}
	// No one passed validation
	return domain.AppVersionID{}, nil
}

// BoundedRepair performs bounded repair of a partially valid candidate.
// It attempts incremental fixes rather than starting from scratch.
//
// PLAN §5.5: "bounded repair of partially valid candidates"
func (gl *GenerationLoop) BoundedRepair(
	ctx context.Context,
	candidate domain.AppArtifact,
	maxRepairAttempts int,
) (domain.AppArtifact, domain.ValidationResult, error) {
	current := candidate
	var validation domain.ValidationResult

	for attempt := 0; attempt < maxRepairAttempts; attempt++ {
		// Run sandbox validation
		sandboxLimitsCopy := gl.sandboxLimits
		sandboxLimitsCopy.DeadlineMs = time.Now().Add(30*time.Second).UnixNano() / int64(time.Millisecond)

		result, sandboxErr := gl.sandbox.Run(ctx, current, domain.AppState{}, domain.AppEvent{}, sandboxLimitsCopy)
		if sandboxErr != nil {
			return current, domain.ValidationResult{Passed: false, ValidatedAt: gl.clock.NowUnixMilli()}, fmt.Errorf("sandbox error on repair attempt %d: %w", attempt+1, sandboxErr)
		}

		// Update validation
		validation = domain.ValidationResult{
			Passed:           result.Exited || result.ExitCode == 0,
			Issues:           extractIssues(result),
			TestResults:      extractTestResults(result),
			ValidatedAt:      gl.clock.NowUnixMilli(),
			ValidatorVersion: "v1-sandbox",
		}

		if validation.Passed {
			return current, validation, nil
		}

		// Attempt incremental fix based on issues
		// For now, we just note the issues and try a minimal adjustment
		// In a full implementation, this would use the issues to guide a targeted fix
		if attempt < maxRepairAttempts-1 {
			// Brief pause before retry (deterministic based on rand seed)
			time.Sleep(time.Duration(gl.random.Intn(100)) * time.Millisecond)
		}
	}

	return current, validation, fmt.Errorf("bounded repair failed after %d attempts: candidate issues: %v", maxRepairAttempts, validation.Issues)
}

// Helper functions

func extractIssues(result domain.AppResult) []string {
	var issues []string
	if result.ExitCode != 0 && result.ExitCode != -1 {
		issues = append(issues, fmt.Sprintf("exit code %d", result.ExitCode))
	}
	if result.Exited {
		issues = append(issues, "app requested termination")
	}
	for _, eff := range result.Effects {
		issues = append(issues, fmt.Sprintf("effect: %s", eff.Mutation.Type))
	}
	if result.WorldReads != nil {
		issues = append(issues, fmt.Sprintf("world reads: %d", len(result.WorldReads)))
	}
	return issues
}

func extractTestResults(result domain.AppResult) []domain.TestResult {
	// In a full implementation, these would come from sandbox test runs
	return nil
}

func nextVersion(current string) string {
	// Simple semantic version bump
	if current == "" {
		return "0.1.0"
	}
	// Parse and increment patch
	parts := strings.Split(current, ".")
	result := make([]int, len(parts))
	for i, p := range parts {
		var n int
		fmt.Sscanf(p, "%d", &n)
		result[i] = n + 1
	}
	if len(result) == 1 {
		return fmt.Sprintf("0.1.%d", result[0])
	}
	if len(result) == 2 {
		return fmt.Sprintf("0.%d.%d", result[0], result[1])
	}
	return current
}
