package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// TestUnknownToolRejected verifies the server-owned allowlist: a model
// request for a tool that does not exist is a typed failure, and no handler
// runs.
func TestUnknownToolRejected(t *testing.T) {
	registry, _, _, _, _, _, _, _, _, _ := newTestRegistry()
	call := testCallContext()

	_, err := registry.Execute(context.Background(), call, ToolCall{
		Name:      "shell",
		Arguments: json.RawMessage(`{"command":"ls"}`),
	})
	if err == nil {
		t.Fatal("expected unknown-tool failure, got success")
	}
	var de *domain.DomainError
	if !errors.As(err, &de) {
		t.Fatalf("error is %T, want *domain.DomainError", err)
	}
	if de.Code != CodeUnknownTool {
		t.Errorf("code = %q, want %q", de.Code, CodeUnknownTool)
	}
	if de.Category != domain.CategoryValidation {
		t.Errorf("category = %q, want %q", de.Category, domain.CategoryValidation)
	}
	// A near-miss name must not accidentally match a real tool.
	for _, name := range []string{"world.lookup2", "WORLD.LOOKUP", "exec", "bash", ""} {
		if _, err := registry.Execute(context.Background(), call, ToolCall{Name: name}); err == nil {
			t.Errorf("tool %q: expected unknown-tool failure, got success", name)
		} else if errors.As(err, &de) && de.Code != CodeUnknownTool {
			t.Errorf("tool %q: code = %q, want %q", name, de.Code, CodeUnknownTool)
		}
	}
}

// TestEveryAllowlistedToolHasAHandler guards against a registry entry
// pointing at a nil handler.
func TestEveryAllowlistedToolHasAHandler(t *testing.T) {
	registry, _, _, _, _, _, _, _, _, _ := newTestRegistry()
	for name := range registry.handlers {
		if registry.handlers[name] == nil {
			t.Errorf("tool %q has a nil handler", name)
		}
	}
}
