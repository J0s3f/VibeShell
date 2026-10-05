package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Adapter implements ports.AppSandbox on top of the capability-free
// QuickJS/Wasm engine. One Adapter serves many runs; every run gets its
// own disposable instance, deadline context, heap cap, output cap, and
// deterministic time/randomness.
type Adapter struct {
	engine *Engine
}

// NewAdapter compiles the engine once and returns an adapter. memoryPages
// is the runtime-wide guest linear-memory cap in 64 KiB pages (0 = default,
// 16 MiB).
func NewAdapter(ctx context.Context, memoryPages uint32, cfg Config) (*Adapter, error) {
	e, err := NewEngine(ctx, memoryPages, cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{engine: e}, nil
}

// Close releases the engine. No runs may be in flight.
func (a *Adapter) Close(ctx context.Context) error {
	return a.engine.Close(ctx)
}

// Imports reports the guest module's imports for capability audits.
func (a *Adapter) Imports() []string {
	return a.engine.Imports()
}

// entrypointPattern restricts the manifest entrypoint to a plain JS
// identifier so it can be embedded into the bridge program without
// escaping or code injection.
var entrypointPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// Run evaluates artifact.Source against explicit state and event in a
// disposable instance and validates the bounded JSON result into a
// domain.AppResult. The guest contract is:
//
//	function <manifest.Entrypoint>(state, event) -> resultObject
//
// where state/event are the parsed JSON inputs and resultObject must
// serialize to a domain.AppResult-shaped JSON object. The instance is
// always closed, including after trap, exit, deadline, or host error.
func (a *Adapter) Run(ctx context.Context, artifact domain.AppArtifact, state domain.AppState, event domain.AppEvent, limits ports.SandboxLimits) (domain.AppResult, error) {
	if err := domain.ValidateManifest(artifact.Manifest); err != nil {
		return domain.AppResult{}, domain.NewValidationError(domain.CodeInvalidAppManifest, "invalid app manifest", map[string]string{"reason": err.Error()})
	}
	entrypoint := artifact.Manifest.Entrypoint
	if !entrypointPattern.MatchString(entrypoint) {
		return domain.AppResult{}, domain.NewValidationError(domain.CodeInvalidAppManifest, "entrypoint is not a plain JS identifier", map[string]string{"entrypoint": entrypoint})
	}
	if !domain.IsValidAppEventType(event.EventType) {
		return domain.AppResult{}, domain.NewValidationError("invalid_app_event", "unknown event type", map[string]string{"event_type": event.EventType})
	}
	if len(event.Payload) > 0 && !json.Valid(event.Payload) {
		return domain.AppResult{}, domain.NewValidationError(domain.CodeInvalidInput, "event payload is not valid JSON", nil)
	}
	if limits.DeadlineMs < 0 || limits.MaxMemoryB < 0 || limits.MaxOutputB < 0 || limits.MaxEvents < 0 {
		return domain.AppResult{}, domain.NewValidationError(domain.CodeInvalidInput, "sandbox limits cannot be negative", nil)
	}

	deadline := DefaultDeadline
	if limits.DeadlineMs > 0 {
		deadline = time.Duration(limits.DeadlineMs) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	stateJSON, err := json.Marshal(state)
	if err != nil {
		return domain.AppResult{}, domain.NewInternalError(domain.CodeSerializationFailed, "marshal state", err)
	}
	eventJSON, err := marshalGuestEvent(event)
	if err != nil {
		return domain.AppResult{}, domain.NewInternalError(domain.CodeSerializationFailed, "marshal event", err)
	}
	if len(stateJSON)+len(eventJSON) > maxInputBytes {
		return domain.AppResult{}, domain.NewValidationError(domain.CodeInvalidInput, "state+event JSON exceeds the input bound", map[string]string{"max_input_bytes": fmt.Sprint(maxInputBytes)})
	}

	heapLimit := a.engine.cfg.HeapLimit
	if limits.MaxMemoryB > 0 {
		heapLimit = uint64(limits.MaxMemoryB)
	}
	maxEvents := limits.MaxEvents
	if maxEvents == 0 {
		maxEvents = DefaultMaxEvents
	}
	maxOutput := a.engine.cfg.MaxOutputBytes
	if limits.MaxOutputB > 0 {
		maxOutput = int(limits.MaxOutputB)
	}

	inst, err := a.engine.NewInstance(runCtx, heapLimit, limits.SimNowMilli)
	if err != nil {
		return domain.AppResult{}, classifyContextErr(ctx, runCtx, err, "instantiate")
	}
	defer func() { _ = inst.Close() }()

	program := buildProgram(artifact.Source, entrypoint, stateJSON, eventJSON, limits.SeededRand, limits.SimNowMilli)
	out, err := inst.Eval(runCtx, program)
	if err != nil {
		return domain.AppResult{}, classifyEvalErr(ctx, runCtx, err)
	}
	if len(out) > maxOutput {
		return domain.AppResult{}, domain.NewLimitError(domain.CodeOutputTooLarge, "app result exceeds the output bound", map[string]string{"max_output_bytes": fmt.Sprint(maxOutput)})
	}

	var result domain.AppResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return domain.AppResult{}, domain.NewInternalError(domain.CodeDeserializationFailed, "app result is not AppResult-shaped JSON", err)
	}
	if n := len(result.Effects) + len(result.WorldReads) + aiRequestCount(result); n > maxEvents {
		return domain.AppResult{}, domain.NewLimitError(domain.CodeOutputTooLarge, "app result declares too many effects/requests", map[string]string{"max_events": fmt.Sprint(maxEvents)})
	}
	if err := domain.ValidateResult(result); err != nil {
		return domain.AppResult{}, domain.NewValidationError(domain.CodeInvalidAppResult, "app result failed validation", map[string]string{"reason": err.Error()})
	}
	return result, nil
}

func aiRequestCount(r domain.AppResult) int {
	if r.AIRequest != nil {
		return 1
	}
	return 0
}

// marshalGuestEvent builds the JSON value the guest receives as its event: the
// app-event envelope with the payload's object fields promoted to the top level.
// A generated program can then read event.command, event.args, and event.cwd
// directly, while event.event_type, event.timestamp, and the original
// event.payload stay available. Envelope fields win if a payload reuses one of
// their names.
func marshalGuestEvent(event domain.AppEvent) ([]byte, error) {
	envelopeJSON, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		return nil, err
	}
	merged := make(map[string]json.RawMessage, len(envelope)+4)
	if len(event.Payload) > 0 {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &payload); err == nil {
			for name, value := range payload {
				merged[name] = value
			}
		}
	}
	for name, value := range envelope {
		merged[name] = value
	}
	return json.Marshal(merged)
}

// buildProgram installs the deterministic prelude, evaluates the app
// source, and invokes the manifest entrypoint with the parsed inputs.
// The result is JSON.stringify'd so it crosses the bridge as one bounded
// string. stateJSON/eventJSON are JSON values embedded as JS literals via
// safely quoted string literals (JSON is a subset of JS expression syntax
// except for U+2028/U+2029, which jsString escapes).
func buildProgram(source, entrypoint string, stateJSON, eventJSON []byte, seed, simNowMilli int64) string {
	var b strings.Builder
	b.WriteString(deterministicPrelude(seed, simNowMilli))
	b.WriteString("\n")
	b.WriteString(source)
	b.WriteString("\n")
	fmt.Fprintf(&b, `;(function(){
const __vibe_state = JSON.parse(%s);
const __vibe_event = JSON.parse(%s);
const __vibe_fn = globalThis[%s];
if (typeof __vibe_fn !== "function") {
  throw new TypeError("entrypoint is not defined or not a function");
}
return JSON.stringify(__vibe_fn(__vibe_state, __vibe_event));
})()`, jsString(string(stateJSON)), jsString(string(eventJSON)), jsString(entrypoint))
	return b.String()
}

// deterministicPrelude replaces ambient randomness and wall time inside the
// guest: Math.random follows a seedable PRNG (mulberry32) and Date.now
// returns the run's simulated clock. The WASI clock stub (per-instance
// simNowMilli) covers Date construction paths that consult it.
func deterministicPrelude(seed, simNowMilli int64) string {
	return fmt.Sprintf(`(function(){
  let __s = (%d) >>> 0;
  Math.random = function() {
    __s = (__s + 0x6D2B79F5) | 0;
    let t = Math.imul(__s ^ (__s >>> 15), 1 | __s);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  Date.now = function() { return %d; };
})();`, seed, simNowMilli)
}

// jsString returns s as a safe JS string literal: JSON-valid double-quoted
// with U+2028/U+2029 escaped, since those are valid in JSON but are line
// terminators inside JS string literals.
func jsString(s string) string {
	q, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	out := string(q)
	out = strings.ReplaceAll(out, "\u2028", `\\u2028`)
	out = strings.ReplaceAll(out, "\u2029", `\\u2029`)
	return out
}

// classifyContextErr maps a NewInstance failure to a domain error,
// distinguishing caller cancellation/deadline from engine faults.
func classifyContextErr(ctx, runCtx context.Context, err error, stage string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cancelledError(ctxErr)
	}
	if runCtxErr := runCtx.Err(); runCtxErr != nil {
		return cancelledError(runCtxErr)
	}
	return domain.NewInternalError(domain.CodeSandboxUnavailable, "sandbox "+stage+" failed", err)
}

// classifyEvalErr maps an Eval failure to a domain error, distinguishing
// caller/deadline cancellation from guest exceptions and exits.
func classifyEvalErr(ctx, runCtx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cancelledError(ctxErr)
	}
	if runCtxErr := runCtx.Err(); runCtxErr != nil {
		return cancelledError(runCtxErr)
	}
	if strings.Contains(err.Error(), "guest exited with code") {
		return domain.NewInternalError(domain.CodeSandboxUnavailable, "guest exited during run", err)
	}
	return domain.NewInternalError("sandbox_eval_failed", "sandbox evaluation failed", err)
}

func cancelledError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.NewCancelledError(domain.CodeTimeout, "sandbox run deadline exceeded", nil)
	}
	return domain.NewCancelledError(domain.CodeUserCancelled, "sandbox run cancelled", nil)
}
