package simulation

import (
	"context"
	"encoding/json"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Candidate trial sandbox bounds. A trial runs the candidate artifact once
// against disposable staging state under the same bounded sandbox as
// production app runs — no external I/O, hard deadline, bounded output.
const (
	CandidateTrialDeadlineMs int64 = 5000
	CandidateTrialMaxMemoryB int64 = 64 << 20
	CandidateTrialMaxOutputB int64 = 1 << 20
	CandidateTrialMaxEvents  int   = 100
)

// ---------------------------------------------------------------------------
// app.lookup
// ---------------------------------------------------------------------------

type appLookupArgs struct {
	AppID     domain.AppID        `json:"app_id,omitempty"`
	VersionID domain.AppVersionID `json:"version_id,omitempty"`
}

type appLookupResult struct {
	AppID           domain.AppID        `json:"app_id"`
	VersionID       domain.AppVersionID `json:"version_id"`
	CommandNames    []string            `json:"command_names"`
	Scope           domain.Scope        `json:"scope"`
	Owner           domain.UserID       `json:"owner"`
	Description     string              `json:"description"`
	Source          string              `json:"source"`
	SourceTruncated bool                `json:"source_truncated"`
	SourceHash      domain.ContentID    `json:"source_hash"`
	ActivatedAt     int64               `json:"activated_at"`
}

// app.lookup finds an existing accepted application and its immutable
// source. Scope enforcement follows the injected policy: another user's
// artifact requires cross-user read; a shared artifact requires sharing.
func (r *Registry) appLookup(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args appLookupArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	var artifact domain.AppArtifact
	switch {
	case !args.VersionID.IsZero():
		var err error
		artifact, err = r.deps.Apps.GetArtifact(ctx, args.VersionID)
		if err != nil {
			return nil, err
		}
	case !args.AppID.IsZero():
		current, err := r.deps.Apps.Current(ctx, args.AppID)
		if err != nil {
			return nil, err
		}
		artifact, err = r.deps.Apps.GetArtifact(ctx, current)
		if err != nil {
			return nil, err
		}
	default:
		return nil, domain.NewValidationError(
			domain.CodeInvalidInput, "app_id or version_id is required", nil)
	}
	if err := call.Policy.AuthorizeRead(artifact.Scope, artifact.Owner == call.UserID); err != nil {
		return nil, err
	}
	res := appLookupResult{
		AppID:        artifact.AppID,
		VersionID:    artifact.VersionID,
		CommandNames: artifact.Manifest.CommandNames,
		Scope:        artifact.Scope,
		Owner:        artifact.Owner,
		Description:  artifact.Manifest.Description,
		SourceHash:   artifact.SourceHash,
		ActivatedAt:  artifact.ActivatedAt,
	}
	if len(artifact.Source) > MaxContentReadBytes {
		res.Source = artifact.Source[:MaxContentReadBytes]
		res.SourceTruncated = true
	} else {
		res.Source = artifact.Source
	}
	return marshalResult(res)
}

// ---------------------------------------------------------------------------
// app.candidate
// ---------------------------------------------------------------------------

type appCandidateArgs struct {
	AppID     domain.AppID       `json:"app_id"`
	Source    string             `json:"source"`
	Manifest  domain.AppManifest `json:"manifest"`
	TestEvent *domain.AppEvent   `json:"test_event,omitempty"`
}

type appTrialResult struct {
	Passed     bool     `json:"passed"`
	Issues     []string `json:"issues,omitempty"`
	Output     string   `json:"output,omitempty"`
	DurationMs int64    `json:"duration_ms"`
}

type appCandidateResult struct {
	AppID         domain.AppID        `json:"app_id"`
	VersionID     domain.AppVersionID `json:"version_id"`
	ParentVersion domain.AppVersionID `json:"parent_version"`
	Activated     bool                `json:"activated"`
	Trial         *appTrialResult     `json:"trial,omitempty"`
}

// app.candidate creates a candidate version of an existing application: the
// manifest is validated, the source is content-addressed, an optional trial
// event is run through the bounded sandbox against disposable state, and
// the candidate is registered without activation. Activation remains a
// separate validated step.
func (r *Registry) appCandidate(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args appCandidateArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.AppID.IsZero() {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "app_id is required", nil)
	}
	if err := domain.ValidateManifest(args.Manifest); err != nil {
		return nil, domain.NewValidationError(
			CodeAppCandidateInvalid, "candidate manifest rejected: "+err.Error(), nil)
	}
	parent, err := r.deps.Apps.Current(ctx, args.AppID)
	if err != nil {
		return nil, err
	}
	versionIDStr, err := newID(domain.PrefixAppVer, r.deps.Random)
	if err != nil {
		return nil, err
	}
	versionID, err := domain.ParseAppVersionID(versionIDStr)
	if err != nil {
		return nil, err
	}
	ref, err := r.deps.Content.Put(ctx, []byte(args.Source), "text/javascript")
	if err != nil {
		return nil, err
	}
	artifact := domain.AppArtifact{
		AppID:         args.AppID,
		VersionID:     versionID,
		ParentVersion: &parent,
		Manifest:      args.Manifest,
		Source:        args.Source,
		SourceHash:    ref.Hash,
		Owner:         call.UserID,
		Scope:         domain.ScopeUser,
		CreatedAt:     call.NowUnixMilli,
		Provenance: domain.Provenance{
			Source:        "tool",
			Actor:         "app.candidate",
			PromptVersion: call.PromptVersion,
		},
	}
	res := appCandidateResult{
		AppID:         args.AppID,
		VersionID:     versionID,
		ParentVersion: parent,
		Activated:     false,
	}
	if args.TestEvent != nil {
		trial := r.trialRun(ctx, call, artifact, *args.TestEvent)
		res.Trial = trial
		artifact.Validation = domain.ValidationResult{
			Passed:      trial.Passed,
			Issues:      trial.Issues,
			TestResults: []domain.TestResult{{Name: "candidate-trial", Passed: trial.Passed, Output: trial.Output}},
			ValidatedAt: call.NowUnixMilli,
		}
	}
	if _, err := r.deps.Apps.RegisterCandidate(ctx, artifact); err != nil {
		return nil, err
	}
	return marshalResult(res)
}

// trialRun executes one bounded sandbox trial of a candidate artifact. A
// sandbox or validation failure is a failed trial, not a tool error: the
// candidate is still registered with the failed trial recorded.
func (r *Registry) trialRun(ctx context.Context, call CallContext, artifact domain.AppArtifact, event domain.AppEvent) *appTrialResult {
	trial := &appTrialResult{}
	limits := ports.SandboxLimits{
		DeadlineMs:  CandidateTrialDeadlineMs,
		MaxMemoryB:  CandidateTrialMaxMemoryB,
		MaxOutputB:  CandidateTrialMaxOutputB,
		MaxEvents:   CandidateTrialMaxEvents,
		SimNowMilli: call.NowUnixMilli,
	}
	outcome, err := r.deps.Sandbox.Run(ctx, artifact, domain.AppState{}, event, limits)
	if err != nil {
		trial.Passed = false
		trial.Issues = []string{"sandbox error: " + err.Error()}
		return trial
	}
	if err := domain.ValidateResult(outcome); err != nil {
		trial.Passed = false
		trial.Issues = []string{"invalid result: " + err.Error()}
		return trial
	}
	trial.Passed = true
	trial.Output = fmt.Sprintf("view mode=%s effects=%d world_reads=%d exited=%t",
		outcome.View.Mode, len(outcome.Effects), len(outcome.WorldReads), outcome.Exited)
	return trial
}
