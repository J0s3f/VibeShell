package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// lineResult is a result whose view and effects are valid, so the tests below
// exercise only the line-based interactive lifecycle.
func lineResult() domain.AppResult {
	return domain.AppResult{View: domain.AppView{Mode: domain.AppViewModeText}}
}

// TestValidateResultAcceptsOneShotAndInteractiveResults checks the two shapes a
// generated application may take: neither field set (one-shot) and the
// continuation fields set while the app stays foreground. Prompt is optional
// even when the app waits, because an app may reuse the shell's own prompt.
func TestValidateResultAcceptsOneShotAndInteractiveResults(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.AppResult)
	}{
		{"one-shot", func(*domain.AppResult) {}},
		{"exited", func(r *domain.AppResult) { r.Exited = true }},
		{"awaiting input", func(r *domain.AppResult) { r.AwaitingInput = true }},
		{"awaiting input with prompt", func(r *domain.AppResult) {
			r.AwaitingInput = true
			r.Prompt = "> "
		}},
		{"exited with a trailing prompt", func(r *domain.AppResult) {
			r.Exited = true
			r.Prompt = "> "
		}},
		{"prompt at the bound", func(r *domain.AppResult) {
			r.Prompt = strings.Repeat(">", domain.MaxAppPromptBytes)
		}},
	}
	for _, tc := range cases {
		res := lineResult()
		tc.mutate(&res)
		if err := domain.ValidateResult(res); err != nil {
			t.Errorf("ValidateResult(%s) = %v, want success", tc.name, err)
		}
	}
}

// TestValidateResultRejectsAwaitingInputAndExited checks that an app cannot ask
// for another line and for termination in the same result: the shell would have
// no defined state to leave the app in.
func TestValidateResultRejectsAwaitingInputAndExited(t *testing.T) {
	res := lineResult()
	res.AwaitingInput = true
	res.Exited = true

	err := domain.ValidateResult(res)
	if err == nil {
		t.Fatal("ValidateResult accepted awaiting_input with exited, want error")
	}
	if !domain.IsValidationError(err) {
		t.Errorf("ValidateResult error %v is not a validation error", err)
	}
	var typed *domain.DomainError
	if !errors.As(err, &typed) {
		t.Fatalf("ValidateResult error %v is not a domain error", err)
	}
	if typed.Code != domain.CodeInvalidAppResult {
		t.Errorf("ValidateResult error code = %q, want %q", typed.Code, domain.CodeInvalidAppResult)
	}
	if !strings.Contains(err.Error(), "awaiting_input") {
		t.Errorf("ValidateResult error %q does not name the conflicting field", err)
	}
}

// TestValidateResultRejectsOverlongPrompt checks the prompt bound at its
// boundary. A prompt is displayed verbatim by the terminal layer, so an
// unbounded one would let an artifact own the screen.
func TestValidateResultRejectsOverlongPrompt(t *testing.T) {
	res := lineResult()
	res.Prompt = strings.Repeat(">", domain.MaxAppPromptBytes+1)

	err := domain.ValidateResult(res)
	if err == nil {
		t.Fatal("ValidateResult accepted a prompt past the bound, want error")
	}
	if !domain.IsValidationError(err) {
		t.Errorf("ValidateResult error %v is not a validation error", err)
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("ValidateResult error %q does not name the offending field", err)
	}
}

// TestAppResultContinuationFieldsRoundTripAsOptional checks that the two fields
// are omitted when unset, so an app that never becomes interactive serializes
// exactly as it did before them, and that a set prompt decodes back unchanged.
func TestAppResultContinuationFieldsRoundTripAsOptional(t *testing.T) {
	oneShot := lineResult()
	raw, err := json.Marshal(oneShot)
	if err != nil {
		t.Fatalf("marshal one-shot result: %v", err)
	}
	for _, key := range []string{`"awaiting_input"`, `"prompt"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("one-shot result %s carries %s; want it omitted", raw, key)
		}
	}

	interactive := lineResult()
	interactive.AwaitingInput = true
	interactive.Prompt = "moon> "
	rawInteractive, err := json.Marshal(interactive)
	if err != nil {
		t.Fatalf("marshal interactive result: %v", err)
	}
	var back domain.AppResult
	if err := json.Unmarshal(rawInteractive, &back); err != nil {
		t.Fatalf("unmarshal interactive result: %v", err)
	}
	if !back.AwaitingInput {
		t.Error("awaiting_input did not survive the round trip")
	}
	if back.Prompt != interactive.Prompt {
		t.Errorf("prompt round-trip = %q, want %q", back.Prompt, interactive.Prompt)
	}
}
