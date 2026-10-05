package simulation

import (
	"context"
	"encoding/json"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// App world-read round bounds. One app run answers world reads in a bounded
// number of rounds, and one round fulfills a bounded number of requests, so
// an app that never stops asking cannot grow the run without bound.
const (
	// DefaultWorldReadRounds is the round budget when AppRunner.MaxRounds is unset.
	DefaultWorldReadRounds = 4
	// MaxWorldReadsPerRound caps the reads one round resolves; requests
	// beyond the cap receive a found:false outcome instead of a lookup.
	MaxWorldReadsPerRound = 32
)

// AppRunRequest describes one app execution: the artifact and state to run,
// the user's command that started it, the namespace its world reads resolve
// in, and the caller-chosen sandbox bounds.
type AppRunRequest struct {
	Artifact     domain.AppArtifact
	State        domain.AppState
	Command      string
	Args         []string
	CWD          domain.ValidPath
	Namespace    domain.NamespaceID
	NowUnixMilli int64
	Limits       ports.SandboxLimits
}

// AppRunner runs an app and fulfils its world reads in bounded rounds.
type AppRunner struct {
	Sandbox   ports.AppSandbox
	Reader    *WorldReader
	MaxRounds int // default DefaultWorldReadRounds
}

// Run executes the app's input event and, while the result keeps requesting
// world reads, resolves those requests and re-runs the app with a
// world_change event carrying the outcomes. Each run's new state feeds the
// next, so the app keeps its continuity across rounds. A final result that
// still requests reads is returned as-is; the caller decides.
func (r *AppRunner) Run(ctx context.Context, req AppRunRequest) (domain.AppResult, error) {
	input, err := appInputEvent(req)
	if err != nil {
		return domain.AppResult{}, err
	}
	result, err := r.Sandbox.Run(ctx, req.Artifact, req.State, input, req.Limits)
	if err != nil {
		return domain.AppResult{}, err
	}
	maxRounds := r.MaxRounds
	if maxRounds <= 0 {
		maxRounds = DefaultWorldReadRounds
	}
	for round := 0; round < maxRounds && len(result.WorldReads) > 0; round++ {
		outcomes, err := r.resolveReads(ctx, req.Namespace, result.WorldReads)
		if err != nil {
			return domain.AppResult{}, err
		}
		change, err := appWorldChangeEvent(req.NowUnixMilli, outcomes)
		if err != nil {
			return domain.AppResult{}, err
		}
		result, err = r.Sandbox.Run(ctx, req.Artifact, result.NewState, change, req.Limits)
		if err != nil {
			return domain.AppResult{}, err
		}
	}
	return result, nil
}

// resolveReads fulfills one round of read requests within the namespace.
// Requests beyond MaxWorldReadsPerRound are answered found:false so a
// runaway app cannot grow one round without bound.
func (r *AppRunner) resolveReads(ctx context.Context, ns domain.NamespaceID, reads []domain.WorldReadRequest) ([]WorldReadOutcome, error) {
	if r.Reader == nil {
		return nil, domain.NewInternalError(
			domain.CodeInvariantViolation, "app runner has no world reader", nil)
	}
	outcomes := make([]WorldReadOutcome, 0, len(reads))
	for i, read := range reads {
		if i >= MaxWorldReadsPerRound {
			outcomes = append(outcomes, WorldReadOutcome{RequestID: read.RequestID, Path: read.Path})
			continue
		}
		outcome, err := r.Reader.Resolve(ctx, ns, read)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

type appInputPayload struct {
	Command string `json:"command"`
	// Line is the full submitted line, the same value as Command. It is the
	// field a line-based interactive app reads on each input event.
	Line string           `json:"line"`
	Args []string         `json:"args"`
	CWD  domain.ValidPath `json:"cwd"`
}

// appInputEvent builds the input event for the user's command.
func appInputEvent(req AppRunRequest) (domain.AppEvent, error) {
	args := req.Args
	if args == nil {
		args = []string{}
	}
	payload, err := json.Marshal(appInputPayload{Command: req.Command, Line: req.Command, Args: args, CWD: req.CWD})
	if err != nil {
		return domain.AppEvent{}, domain.NewInternalError(
			domain.CodeSerializationFailed, "marshal app input payload", err)
	}
	return domain.AppEvent{
		EventType: domain.AppEventInput,
		Payload:   payload,
		Timestamp: req.NowUnixMilli,
	}, nil
}

type appWorldChangePayload struct {
	Results []WorldReadOutcome `json:"results"`
}

// appWorldChangeEvent builds the world_change event carrying one round's
// read outcomes back to the app.
func appWorldChangeEvent(nowUnixMilli int64, outcomes []WorldReadOutcome) (domain.AppEvent, error) {
	payload, err := json.Marshal(appWorldChangePayload{Results: outcomes})
	if err != nil {
		return domain.AppEvent{}, domain.NewInternalError(
			domain.CodeSerializationFailed, "marshal app world_change payload", err)
	}
	return domain.AppEvent{
		EventType: domain.AppEventWorldChange,
		Payload:   payload,
		Timestamp: nowUnixMilli,
	}, nil
}
