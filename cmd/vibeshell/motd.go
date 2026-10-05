package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/presentation"
	"j0s.at/vibeshell/internal/routing"
)

// motdPromptVersion is the version this build publishes for the
// administrator MOTD prompt. It matches presentation.DefaultPromptVersion so
// MOTDService accepts the provider's answer.
const motdPromptVersion = presentation.DefaultPromptVersion

// filePromptProvider supplies the MOTD prompt text loaded from the configured
// `prompts.motd` file at startup. The file is administrator-owned and
// validated on load, so reading it once is sufficient for this build's
// no-reload lifecycle.
type filePromptProvider struct {
	text string
}

var _ presentation.MOTDPromptProvider = (*filePromptProvider)(nil)

// Prompt returns the stored prompt for the requested version. An unknown
// version is an error rather than a silently substituted prompt.
func (p *filePromptProvider) Prompt(_ context.Context, version string) (presentation.MOTDPrompt, error) {
	if version != motdPromptVersion {
		return presentation.MOTDPrompt{}, fmt.Errorf("unknown MOTD prompt version %q", version)
	}
	return presentation.MOTDPrompt{Version: motdPromptVersion, Text: p.text}, nil
}

// newFilePromptProvider loads the prompt file. A relative path is resolved
// against the configuration directory, matching the loader's rule.
func newFilePromptProvider(cfg *config.Config, configDir string) (*filePromptProvider, error) {
	if cfg.Prompts == nil || cfg.Prompts.Motd == "" {
		return nil, fmt.Errorf("configuration has no prompts.motd file")
	}
	file := cfg.Prompts.Motd
	if !path.IsAbs(file) {
		file = path.Join(configDir, file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read MOTD prompt %q: %w", file, err)
	}
	return &filePromptProvider{text: string(raw)}, nil
}

// failoverExecutor runs one routed model request with the router's health and
// failover. It is the seam the MOTD and generation paths use instead of the
// gateway directly, so both pick up per-purpose model selection and switch to
// the next eligible model when one fails or returns an unusable answer.
type failoverExecutor interface {
	Execute(ctx context.Context, req routing.ExecuteRequest) (routing.ExecuteResult, error)
}

// gatewayMOTDGenerator renders the MOTD through the routed model executor. The
// router selects the purpose-eligible route and fails over on a provider fault
// or an empty answer, so a single broken model cannot deny every login.
type gatewayMOTDGenerator struct {
	executor  failoverExecutor
	clock     ports.Clock
	maxWaitMs int64
}

var _ presentation.MOTDGenerator = (*gatewayMOTDGenerator)(nil)

// Generate builds the canonical MOTD model request and performs one routed
// inference. It carries no tools and no scope policy: the request is
// presentation data only (PLAN 7.4).
func (g *gatewayMOTDGenerator) Generate(ctx context.Context, req presentation.MOTDGenerationRequest) (string, error) {
	if g.executor == nil {
		return "", fmt.Errorf("no model router configured")
	}
	deadline := g.clock.NowUnixMilli() + g.maxWaitMs
	mreq := req.ModelRequest(domain.RouteID{}, domain.AccountID{}, "motd-"+req.Inputs.Username, deadline)
	result, err := g.executor.Execute(ctx, routing.ExecuteRequest{
		Purpose:    domain.PurposeMOTD,
		Messages:   mreq.Messages,
		MaxTokens:  mreq.MaxTokens,
		DeadlineMs: deadline,
		Accept: func(resp domain.ModelResponse) error {
			if strings.TrimSpace(resp.Message.Content) == "" {
				return fmt.Errorf("model returned no welcome text")
			}
			return nil
		},
	})
	if err != nil {
		return "", err
	}
	return result.Response.Message.Content, nil
}
