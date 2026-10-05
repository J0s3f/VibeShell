package main

import (
	"context"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/routing"
)

// contentMaterializer generates the plausible content of a path that does not
// exist yet, so an invented file receives an AI-generated presentation and then
// persists. A nil materializer keeps the truthful "no such file" behavior.
type contentMaterializer interface {
	Materialize(ctx context.Context, path domain.ValidPath, hint string) ([]byte, error)
}

// Bounds for one materialization request.
const (
	materializeMaxTokens   = 2048
	materializeTimeoutMs   = 60000
	materializeMaxFileSize = 64 << 10
)

// worldGenerator materializes content through the routed model executor, using
// the generation purpose so it shares the configured generation route.
type worldGenerator struct {
	executor failoverExecutor
	clock    ports.Clock
}

func (g *worldGenerator) Materialize(ctx context.Context, path domain.ValidPath, hint string) ([]byte, error) {
	if g.executor == nil {
		return nil, fmt.Errorf("no model executor configured")
	}
	deadline := g.clock.NowUnixMilli() + materializeTimeoutMs
	result, err := g.executor.Execute(ctx, routing.ExecuteRequest{
		Purpose:    domain.PurposeGeneration,
		Messages:   []domain.Message{materializePrompt(path, hint)},
		MaxTokens:  materializeMaxTokens,
		DeadlineMs: deadline,
		Accept: func(resp domain.ModelResponse) error {
			if strings.TrimSpace(resp.Message.Content) == "" {
				return fmt.Errorf("model returned no content")
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	content := []byte(result.Response.Message.Content)
	if len(content) > materializeMaxFileSize {
		content = content[:materializeMaxFileSize]
	}
	return content, nil
}

// materializePrompt asks for the contents of one simulated file. The path is
// data, not an instruction.
func materializePrompt(path domain.ValidPath, hint string) domain.Message {
	system := "You write the contents of files in a simulated Unix-like filesystem. " +
		"Return only the file's text, with no commentary, no markdown fences, and no explanation. " +
		"Keep it short and plausible for the file's name and location."
	user := fmt.Sprintf("Write the contents of the simulated file %q.", path.String())
	if hint != "" {
		user += " Context: " + hint
	}
	return domain.Message{Role: domain.RoleUser, Content: system + "\n\n" + user}
}
