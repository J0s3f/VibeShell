// Package opencodecli implements ports.ModelGateway by invoking the OpenCode
// CLI, which is an authenticated client for models the direct HTTP API cannot
// reach (notably the Console free tier, which is gated to OpenCode clients).
//
// It runs `opencode run -m <provider>/<model> --format json <prompt>` and
// collects the text parts of the NDJSON event stream. The CLI owns its own
// credentials; this adapter never handles a secret.
package opencodecli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Runner executes the CLI. It is injectable so tests never spawn a process.
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// execRunner is the production Runner over os/exec.
type execRunner struct {
	// Dir is the child's working directory. Empty inherits the parent's.
	Dir string
}

func (r execRunner) Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Always hand the child an explicit stdin, even when empty, so it sees
	// immediate EOF instead of inheriting (and holding open) our own stdin.
	cmd.Stdin = bytes.NewReader(stdin)
	if r.Dir != "" {
		cmd.Dir = r.Dir
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}

// Adapter is the CLI-backed provider.
type Adapter struct {
	// Binary is the CLI executable. Empty selects "opencode" from PATH.
	Binary string
	// ProviderID is the CLI provider id used in the -m argument, e.g.
	// "opencode" for the Console models or "opencode-go" for Go models.
	ProviderID string
	// Models maps each route to the CLI model id (without the provider prefix).
	Models map[domain.RouteID]string
	// Clock stamps error envelopes. Nil uses time.Now.
	Clock ports.Clock
	// Runner executes the CLI. Nil selects os/exec.
	Runner Runner
	// WorkingDir is the process working directory. Empty inherits.
	WorkingDir string
}

var _ ports.ModelGateway = (*Adapter)(nil)

func (a *Adapter) clock() ports.Clock {
	if a.Clock != nil {
		return a.Clock
	}
	return wallClock{}
}

func (a *Adapter) binary() string {
	if a.Binary != "" {
		return a.Binary
	}
	return "opencode"
}

func (a *Adapter) providerID() string {
	if a.ProviderID != "" {
		return a.ProviderID
	}
	return "opencode"
}

func (a *Adapter) runner() Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return execRunner{Dir: a.WorkingDir}
}

// Request runs one CLI inference and returns the collected assistant text.
func (a *Adapter) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	now := a.clock().NowUnixMilli()
	fail := func(class domain.FailureClass, msg string) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, domain.NewErrorEnvelope(class, msg, req.RouteID, req.AccountID, now)
	}

	model, ok := a.Models[req.RouteID]
	if !ok || model == "" {
		return fail(domain.FailureModelNotFound, fmt.Sprintf("no CLI model for route %s", req.RouteID.String()))
	}
	if len(req.Messages) == 0 {
		return fail(domain.FailureInvalidArguments, "no messages in request")
	}

	if req.DeadlineMs > 0 {
		remaining := req.DeadlineMs - now
		if remaining <= 0 {
			return fail(domain.FailureNetworkTimeout, "request deadline already exceeded")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(req.DeadlineMs))
		defer cancel()
	}

	args := []string{"run", "-m", a.providerID() + "/" + model, "--format", "json", messagesToPrompt(req.Messages)}
	start := a.clock().NowUnixMilli()
	stdout, stderr, err := a.runner().Run(ctx, a.binary(), args, nil)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return fail(domain.FailureNetworkTimeout, "CLI request deadline exceeded")
		}
		return fail(domain.FailureProviderOutage, "CLI invocation failed: "+summarize(stderr, err))
	}

	text, err := parseEvents(stdout)
	if err != nil {
		return fail(domain.FailureInvalidResponse, "CLI returned an unusable response: "+err.Error())
	}
	if strings.TrimSpace(text) == "" {
		return fail(domain.FailureInvalidResponse, "CLI returned no text")
	}
	end := a.clock().NowUnixMilli()
	latency := end - start
	if latency < 0 {
		latency = 0
	}
	return domain.ModelResponse{
		RequestID:    req.RequestID,
		RouteID:      req.RouteID,
		AccountID:    req.AccountID,
		Message:      domain.Message{Role: domain.RoleAssistant, Content: text},
		FinishReason: domain.FinishReasonStop,
		LatencyMs:    latency,
		Timestamp:    end,
	}, nil
}

// cliEvent is one NDJSON event from `opencode run --format json`. Only the
// text parts matter to a non-interactive caller.
type cliEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
	Error string `json:"error"`
}

// parseEvents concatenates the text parts of the CLI's NDJSON stream.
func parseEvents(out []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var b strings.Builder
	sawEvent := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev cliEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // non-JSON diagnostic lines are ignored
		}
		sawEvent = true
		if ev.Type == "text" && ev.Part.Text != "" {
			b.WriteString(ev.Part.Text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if !sawEvent {
		return "", fmt.Errorf("no JSON events")
	}
	return b.String(), nil
}

// messagesToPrompt flattens the canonical messages into the single prompt the
// CLI accepts, preserving order and role boundaries with blank lines.
func messagesToPrompt(messages []domain.Message) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n\n")
}

// summarize renders a short, bounded diagnostic from the CLI's stderr.
func summarize(stderr []byte, err error) string {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = err.Error()
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// wallClock is the default Clock.
type wallClock struct{}

func (wallClock) NowUnixMilli() int64   { return time.Now().UnixMilli() }
func (wallClock) MonotonicNanos() int64 { return time.Now().UnixNano() }
