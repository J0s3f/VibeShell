package load

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/routing"
)

// LocalEngine is the model-free turn engine used by the session and SSH
// scenarios. It answers a small, documented set of harness commands without
// contacting any provider, so a measured local latency contains no provider
// time at all.
//
// It is a load-harness double, not product behaviour: a real service reaches
// this seam through internal/simulation generation. Its purpose is to give the
// transport, coordinator, renderer, and storage stacks real work to do.
type LocalEngine struct {
	// OutputBytes is the size of the body produced for the emit command. The
	// large-output and large-paste cases set it to their own sizes.
	mu        sync.Mutex
	turns     int64
	pastes    int64
	emits     int64
	unknown   int64
	emitBytes atomic.Int64
}

var _ application.TurnEngine = (*LocalEngine)(nil)

// NewLocalEngine returns an engine whose emit command produces size bytes.
func NewLocalEngine(emitBytes int) *LocalEngine {
	engine := &LocalEngine{}
	engine.emitBytes.Store(int64(emitBytes))
	return engine
}

// LocalEngineStats counts what the double handled.
type LocalEngineStats struct {
	Turns     int64 `json:"turns"`
	Pastes    int64 `json:"pastes"`
	Emits     int64 `json:"emits"`
	Unknown   int64 `json:"unknown_commands"`
	EmitBytes int64 `json:"emit_bytes"`
}

// Stats snapshots the counters.
func (e *LocalEngine) Stats() LocalEngineStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return LocalEngineStats{
		Turns:     e.turns,
		Pastes:    e.pastes,
		Emits:     e.emits,
		Unknown:   e.unknown,
		EmitBytes: e.emitBytes.Load(),
	}
}

// Prepare assembles no model context: the double needs only the request it is
// handed at generation time.
func (e *LocalEngine) Prepare(context.Context, application.TurnRequest) (application.PreparedTurn, error) {
	return application.PreparedTurn{}, nil
}

// Generate returns one complete candidate without contacting a provider.
func (e *LocalEngine) Generate(_ context.Context, in application.GenerationInput) (application.StepOutcome, error) {
	req := in.Request
	e.mu.Lock()
	e.turns++
	if req.Input.Kind == application.InputPaste {
		e.pastes++
	}
	e.mu.Unlock()

	text, exit := e.respond(req)
	return application.StepOutcome{
		Phase: application.StepCandidate,
		Candidate: &application.TurnCandidate{
			CommitKey:  application.CommitKey{Turn: req.Turn, Logical: "loadharness-local"},
			Output:     application.CandidateOutput{Text: text},
			ExitStatus: exit,
		},
	}, nil
}

// respond produces the deterministic body for one accepted input.
func (e *LocalEngine) respond(req application.TurnRequest) (string, int) {
	if req.Input.Kind == application.InputPaste {
		return fmt.Sprintf("accepted paste of %d bytes", len(req.Input.Text)), 0
	}
	fields := strings.Fields(req.Input.Command)
	if len(fields) == 0 {
		return "", 0
	}
	switch fields[0] {
	case "pwd":
		return string(req.Context.CWD), 0
	case "echo":
		return strings.Join(fields[1:], " "), 0
	case "emit":
		return e.emit(req), 0
	case "slowlocal":
		// A bounded unit of synthetic local work used to widen the local
		// latency distribution without involving any provider.
		size := 256 << 10
		if len(fields) > 1 {
			if parsed, err := strconv.Atoi(fields[1]); err == nil && parsed > 0 && parsed <= (8<<20) {
				size = parsed
			}
		}
		return string(make([]byte, size)), 0
	case "top":
		return topSnapshot(), 0
	default:
		e.mu.Lock()
		e.unknown++
		e.mu.Unlock()
		return fmt.Sprintf("loadharness: %s: no provider account configured", fields[0]), 127
	}
}

// emit produces deterministic output lines of the configured size.
func (e *LocalEngine) emit(req application.TurnRequest) string {
	e.mu.Lock()
	e.emits++
	size := int(e.emitBytes.Load())
	e.mu.Unlock()
	if size <= 0 {
		size = 4096
	}
	var b strings.Builder
	b.Grow(size + 32)
	row := 0
	for b.Len() < size {
		fmt.Fprintf(&b, "row %06d %s\n", row, strings.Repeat("x", 64))
		row++
	}
	return b.String()
}

// topSnapshot is a fixed top-like refresh body: the mix scenario uses it to
// exercise repeated full-screen refreshes without a model.
func topSnapshot() string {
	var b strings.Builder
	b.WriteString("VibeOS simulated load 1.0.0 - session 1\n")
	b.WriteString("load average: 0.42 0.31 0.28\n\n")
	b.WriteString("  PID USER      PRI  NI  VIRT  RES  COMMAND\n")
	for i := 1; i <= 16; i++ {
		fmt.Fprintf(&b, "%5d user%02d    20   0  12Mi  4Mi  /sim/bin/worker%d\n", 1000+i, i, i%7)
	}
	return b.String()
}

// RoutedEngine drives real turns through internal/routing against the delayed
// provider double and the harness admission gate. It is what makes the
// backpressure measurement exercise the shipped routing policy rather than a
// hand-written loop.
type RoutedEngine struct {
	router *routing.Router
}

var _ application.TurnEngine = (*RoutedEngine)(nil)

// NewRoutedEngine returns an engine bound to router.
func NewRoutedEngine(router *routing.Router) *RoutedEngine {
	return &RoutedEngine{router: router}
}

// Prepare assembles no extra context: routing owns request assembly.
func (e *RoutedEngine) Prepare(context.Context, application.TurnRequest) (application.PreparedTurn, error) {
	return application.PreparedTurn{}, nil
}

// Generate executes one routed turn and converts the accepted response into a
// candidate. Routing failures propagate so the coordinator records a failed
// turn rather than a fabricated success.
func (e *RoutedEngine) Generate(ctx context.Context, in application.GenerationInput) (application.StepOutcome, error) {
	req := in.Request
	command := req.Input.Command
	if req.Input.Kind == application.InputPaste {
		command = "handle a pasted payload of " + strconv.Itoa(len(req.Input.Text)) + " bytes"
	}
	result, err := e.router.ExecuteTurn(ctx, routing.TurnRequest{
		Session: req.Session,
		Turn:    req.Turn,
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: "VibeShell load harness"},
			{Role: domain.RoleUser, Content: command},
		},
	})
	if err != nil {
		return application.StepOutcome{}, err
	}
	return application.StepOutcome{
		Phase: application.StepCandidate,
		Candidate: &application.TurnCandidate{
			CommitKey: application.CommitKey{Turn: req.Turn, Logical: "loadharness-routed"},
			Output:    application.CandidateOutput{Text: result.Response.Message.Content},
			Usage:     result.Response.Usage,
		},
		Route: result.Binding.Route,
	}, nil
}

// RejectTools is the ToolExecutor wired into every harness coordinator. The
// harness engines never request a tool batch, so any batch is a harness bug and
// is reported as an unavailability rather than ignored.
type RejectTools struct{}

var _ application.ToolExecutor = RejectTools{}

// Execute refuses every tool batch.
func (RejectTools) Execute(context.Context, application.ToolBatch) (application.ToolBatchResult, error) {
	return application.ToolBatchResult{}, domain.NewUnavailableError(
		domain.CodeShutdown, "load harness wires no tool executor", nil, nil)
}

// StaticSnapshots is the coordinator SnapshotSource for harness runs. A live
// configuration reload is out of scope for a capacity run; the pinned snapshot
// is stated once and held for the whole measurement.
type StaticSnapshots struct {
	Snapshot application.ConfigSnapshot
}

var _ application.SnapshotSource = (*StaticSnapshots)(nil)

// CurrentSnapshot returns the pinned snapshot.
func (s *StaticSnapshots) CurrentSnapshot(context.Context) (application.ConfigSnapshot, error) {
	return s.Snapshot, nil
}
