package apps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Source markers the double recognizes to simulate
// sandbox outcomes without executing JavaScript:
//
//   - MarkerCompileError: the candidate fails to
//     compile, so every run fails.
//   - MarkerIncompatibleState: the candidate
//     interprets only its own schema version, so
//     state at any other version fails (a breaking
//     change). Without the marker the candidate
//     understands every schema version up to its
//     own (an additive change).
const (
	MarkerCompileError      = "// @sandbox.compile-error"
	MarkerIncompatibleState = "// @sandbox.incompatible-state"
)

// stateSchemaVersion extracts the declared schema
// version from a manifest state schema.
func stateSchemaVersion(manifest domain.AppManifest) int64 {
	if manifest.StateSchema == nil {
		return 0
	}
	var schema struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(manifest.StateSchema, &schema); err != nil {
		return 0
	}
	return schema.Version
}

// statePortionVersion extracts the schema version
// carried by a state portion.
func statePortionVersion(portion json.RawMessage) (int64, bool) {
	if len(portion) == 0 {
		return 0, false
	}
	var wrapped struct {
		Version int64 `json:"schema"`
	}
	if err := json.Unmarshal(portion, &wrapped); err != nil {
		return 0, false
	}
	return wrapped.Version, true
}

// FakeSandbox is the contract double for the
// ports.AppSandbox port (the B07 sandbox adapter).
// It does not execute JavaScript; it simulates the
// sandbox contract deterministically so registry and
// versioning behavior is testable before the real
// QuickJS adapter lands. RunFunc, when set, replaces
// the default behavior entirely.
type FakeSandbox struct {
	// RunFunc, when non-nil, is the sandbox's
	// behavior for every run.
	RunFunc func(ctx context.Context, artifact domain.AppArtifact, state domain.AppState, event domain.AppEvent, limits ports.SandboxLimits) (domain.AppResult, error)
}

// FakeSandbox satisfies the outbound AppSandbox port.
var _ ports.AppSandbox = (*FakeSandbox)(nil)

// Run evaluates one event against explicit state.
func (f *FakeSandbox) Run(
	ctx context.Context,
	artifact domain.AppArtifact,
	state domain.AppState,
	event domain.AppEvent,
	limits ports.SandboxLimits,
) (domain.AppResult, error) {
	if f.RunFunc != nil {
		return f.RunFunc(ctx, artifact, state, event, limits)
	}
	if strings.Contains(artifact.Source, MarkerCompileError) {
		return domain.AppResult{}, errors.New("simulated compile error")
	}
	declared := stateSchemaVersion(artifact.Manifest)
	strict := strings.Contains(artifact.Source, MarkerIncompatibleState)
	for _, portion := range []json.RawMessage{state.SessionState, state.UserState, state.SharedState} {
		version, present := statePortionVersion(portion)
		if !present {
			continue
		}
		interpretable := version <= declared
		if strict {
			interpretable = version == declared
		}
		if !interpretable {
			return domain.AppResult{}, errors.New("simulated state interpretation failure")
		}
	}
	return domain.AppResult{
		NewState: normalizeState(artifact, state),
		View: domain.AppView{
			Mode:       domain.AppViewModeText,
			StatusLine: "ok",
		},
	}, nil
}

// normalizeState rewrites each state portion's
// schema version to the artifact's declared version,
// modeling how an accepting application rewrites
// state it persists.
func normalizeState(artifact domain.AppArtifact, state domain.AppState) domain.AppState {
	declared := stateSchemaVersion(artifact.Manifest)
	return domain.AppState{
		SessionState: bumpStateVersion(state.SessionState, declared),
		UserState:    bumpStateVersion(state.UserState, declared),
		SharedState:  bumpStateVersion(state.SharedState, declared),
	}
}

func bumpStateVersion(portion json.RawMessage, version int64) json.RawMessage {
	if len(portion) == 0 {
		return portion
	}
	var generic map[string]any
	if err := json.Unmarshal(portion, &generic); err != nil {
		return portion
	}
	generic["schema"] = version
	encoded, err := json.Marshal(generic)
	if err != nil {
		return portion
	}
	return encoded
}
