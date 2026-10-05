package presentation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// MOTD generation defaults (PLAN 7.4). The administrator
// configures the prompt version; the line budget and byte bound
// are hard presentation limits that survive any prompt edit.
const (
	// DefaultPromptVersion is the versioned prompt requested when
	// configuration does not name another permitted version.
	DefaultPromptVersion = "motd-v1"
	// DefaultMOTDLineBudget bounds the presented MOTD length.
	DefaultMOTDLineBudget = 20
	// DefaultMOTDMaxBytes bounds the sanitized MOTD size.
	DefaultMOTDMaxBytes = 4096
	// MaxMOTDCompletionTokens bounds the generation request so a
	// slow or runaway model cannot turn a login message into an
	// unbounded answer. The bound must exceed the reasoning a
	// reasoning model emits before its answer: at 256 tokens the
	// streamed answer was empty because the chain of thought consumed
	// the whole budget, so the MOTD fell back. 1024 leaves room for
	// the reasoning and a short welcome.
	MaxMOTDCompletionTokens = 1024
	// MaxUsernameLen bounds the displayed username input.
	MaxUsernameLen = 255
)

// MOTDInputs are the factual inputs to per-session MOTD
// generation (PLAN 7.4). They are the complete set of session
// facts the policy may use: nothing else — no host data, no
// previous logins, no user activity — enters the request, so the
// generated message cannot fabricate it.
type MOTDInputs struct {
	// Username is the displayed (already validated and escaped)
	// username of the session principal.
	Username string `json:"username"`
	// SessionTimeUnixMilli is the session start time (UTC,
	// wall-clock milliseconds).
	SessionTimeUnixMilli int64 `json:"session_time_unix_milli"`
	// SharingEnabled reports the effective sharing mode.
	SharingEnabled bool `json:"sharing_enabled"`
	// PermanentRecording reports whether the permanent
	// research-recording policy is active for this session.
	PermanentRecording bool `json:"permanent_recording"`
}

// Validate reports whether the inputs are well formed. The
// username must be non-empty, printable, and bounded; the
// session time must be a real timestamp.
func (in MOTDInputs) Validate() error {
	if in.Username == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD username is empty", nil)
	}
	if len(in.Username) > MaxUsernameLen {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD username exceeds the display limit", map[string]string{
			"limit": fmt.Sprint(MaxUsernameLen),
		})
	}
	if strings.IndexFunc(in.Username, isForbiddenRune) >= 0 {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD username contains control characters", nil)
	}
	if in.SessionTimeUnixMilli <= 0 {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD session time is missing", nil)
	}
	return nil
}

// serviceUnavailableMOTD is the truthful fallback shown when
// inference is unavailable (PLAN 12.3). It states the situation
// plainly and fabricates no host details, logins, or activity.
const serviceUnavailableMOTD = "Welcome to VibeOS, a simulated GNU/Hurd-style environment whose shell is VibeShell.\n" +
	"The generated login message is unavailable because the model service is not reachable right now."

// MOTDPrompt is a validated, versioned MOTD generation prompt.
// The administrator controls the text; the version identifies the
// exact prompt used for a session so research records can
// reproduce it.
type MOTDPrompt struct {
	Version string `json:"version"`
	Text    string `json:"text"`
}

// MOTDGenerationRequest is the exact request the policy sends to
// the generator. It deliberately contains no tool definitions, no
// scope policy, and no execution capability: a presentation
// prompt is data and can change only the wording of the welcome
// within the configured line budget. An edited prompt therefore
// cannot expand hard tool/scope permissions or enable real
// execution.
type MOTDGenerationRequest struct {
	Prompt   MOTDPrompt     `json:"prompt"`
	Inputs   MOTDInputs     `json:"inputs"`
	Identity SystemIdentity `json:"identity"`
}

// ModelRequest builds the canonical provider request for MOTD
// generation. The request carries no tools (Tools stays nil) and
// exactly two messages: the administrator prompt as the system
// message and the factual inputs as the user message. Route,
// account, and deadline are supplied by the session coordinator,
// never by the prompt.
func (r MOTDGenerationRequest) ModelRequest(route domain.RouteID, account domain.AccountID, requestID string, deadlineUnixMilli int64) domain.ModelRequest {
	user, _ := json.Marshal(motdFacts{
		Username:             r.Inputs.Username,
		SessionTimeUnixMilli: r.Inputs.SessionTimeUnixMilli,
		SessionTimeUTC:       time.UnixMilli(r.Inputs.SessionTimeUnixMilli).UTC().Format(time.RFC3339),
		SharingEnabled:       r.Inputs.SharingEnabled,
		PermanentRecording:   r.Inputs.PermanentRecording,
		SystemName:           r.Identity.SystemName,
		ShellName:            r.Identity.ShellName,
	})
	return domain.ModelRequest{
		RouteID:   route,
		AccountID: account,
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: r.Prompt.Text},
			{Role: domain.RoleUser, Content: string(user)},
		},
		MaxTokens:  MaxMOTDCompletionTokens,
		DeadlineMs: deadlineUnixMilli,
		RequestID:  requestID,
	}
}

// motdFacts is the deterministic JSON payload handed to the
// model: the only session facts generation may use.
type motdFacts struct {
	Username             string `json:"username"`
	SessionTimeUnixMilli int64  `json:"session_time_unix_milli"`
	SessionTimeUTC       string `json:"session_time_utc"`
	SharingEnabled       bool   `json:"sharing_enabled"`
	PermanentRecording   bool   `json:"permanent_recording"`
	SystemName           string `json:"system_name"`
	ShellName            string `json:"shell_name"`
}

// MOTDPromptProvider supplies the administrator-controlled,
// versioned MOTD generation prompt. The production adapter loads
// the prompts/ files owned by the configuration/simulation
// workstream; tests inject a double.
type MOTDPromptProvider interface {
	// Prompt returns the prompt stored under version, or an error
	// when the version is unknown or the prompt store is down.
	Prompt(ctx context.Context, version string) (MOTDPrompt, error)
}

// MOTDGenerator renders a raw MOTD from a prompt and its factual
// inputs. The production adapter calls ports.ModelGateway with
// the request built by ModelRequest; tests inject a double. The
// generator has no authority over presentation policy: whatever
// it returns is validated, sanitized, and budgeted here.
type MOTDGenerator interface {
	// Generate returns raw, unsanitized MOTD text.
	Generate(ctx context.Context, req MOTDGenerationRequest) (string, error)
}

// MOTDConfig configures one MOTD policy.
type MOTDConfig struct {
	// PromptVersion selects the prompt from the provider.
	PromptVersion string `json:"prompt_version"`
	// LineBudget is the maximum number of presented MOTD lines.
	LineBudget int `json:"line_budget"`
	// MaxBytes is the maximum size of the sanitized MOTD.
	MaxBytes int `json:"max_bytes"`
}

// DefaultMOTDConfig returns the default MOTD configuration.
func DefaultMOTDConfig() MOTDConfig {
	return MOTDConfig{
		PromptVersion: DefaultPromptVersion,
		LineBudget:    DefaultMOTDLineBudget,
		MaxBytes:      DefaultMOTDMaxBytes,
	}
}

// Validate reports whether the configuration is usable. Bounds
// are validated once at construction so every later generation
// uses an already-checked policy.
func (c MOTDConfig) Validate() error {
	if err := ValidatePromptVersion(c.PromptVersion); err != nil {
		return err
	}
	if c.LineBudget <= 0 {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD line budget must be positive", map[string]string{
			"line_budget": fmt.Sprint(c.LineBudget),
		})
	}
	if c.MaxBytes <= 0 {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD byte bound must be positive", map[string]string{
			"max_bytes": fmt.Sprint(c.MaxBytes),
		})
	}
	return nil
}

// MOTDResult is the policy-approved MOTD for one session. The
// caller records PromptVersion, Inputs, and Text in the research
// log (PLAN 7.4); the result carries them so recording needs no
// second lookup.
type MOTDResult struct {
	// Text is the sanitized MOTD without a trailing newline.
	Text string `json:"text"`
	// PromptVersion is the exact prompt version that produced it.
	PromptVersion string `json:"prompt_version"`
	// Lines is the number of presented lines.
	Lines int `json:"lines"`
	// Truncated reports whether the line budget cut the text.
	Truncated bool `json:"truncated"`
	// ServiceUnavailable reports whether the truthful fallback
	// replaced a failed generation.
	ServiceUnavailable bool `json:"service_unavailable"`
}

// MOTDService is the trusted per-session MOTD presentation
// policy. It composes the prompt provider, the generator, and
// the hard sanitization/budget rules; neither the prompt nor the
// generator can relax those rules.
type MOTDService struct {
	config    MOTDConfig
	provider  MOTDPromptProvider
	generator MOTDGenerator
	identity  SystemIdentity
}

// NewMOTDService validates the configuration and identity before
// wiring the policy, so a misconfigured service fails at
// construction rather than at first login.
func NewMOTDService(config MOTDConfig, provider MOTDPromptProvider, generator MOTDGenerator, identity SystemIdentity) (*MOTDService, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt provider is required", nil)
	}
	if generator == nil {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "MOTD generator is required", nil)
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	return &MOTDService{config: config, provider: provider, generator: generator, identity: identity}, nil
}

// GenerateMOTD produces the MOTD for one accepted shell session.
// Generation runs at most once per session; the caller owns that
// lifecycle.
//
// Policy violations (invalid inputs, unusable prompt store)
// return a *domain.DomainError. A generator failure never
// fabricates a welcome: the result carries the truthful
// service-unavailable message with ServiceUnavailable set and a
// nil error, so the session can proceed without pretending
// inference succeeded (PLAN 12.3).
func (s *MOTDService) GenerateMOTD(ctx context.Context, inputs MOTDInputs) (MOTDResult, error) {
	if err := inputs.Validate(); err != nil {
		return MOTDResult{}, err
	}
	prompt, err := s.provider.Prompt(ctx, s.config.PromptVersion)
	if err != nil {
		return MOTDResult{}, domain.NewUnavailableError("prompt_unavailable",
			"the MOTD prompt store is unavailable", map[string]string{"prompt_version": s.config.PromptVersion}, err)
	}
	if prompt.Version != s.config.PromptVersion {
		return MOTDResult{}, domain.NewValidationError(domain.CodeInvalidInput,
			"prompt store returned a different version than requested", map[string]string{
				"requested": s.config.PromptVersion,
				"received":  prompt.Version,
			})
	}
	if err := ValidatePrompt(prompt); err != nil {
		return MOTDResult{}, err
	}

	// The trusted renderer substitutes the documented session facts before the
	// prompt is sent; the model never receives the raw file (prompts/README.md).
	rendered, err := RenderMOTDPrompt(prompt.Text, s.motdVariables(inputs))
	if err != nil {
		return MOTDResult{}, err
	}
	req := MOTDGenerationRequest{
		Prompt:   MOTDPrompt{Version: prompt.Version, Text: rendered},
		Inputs:   inputs,
		Identity: s.identity,
	}
	raw, err := s.generator.Generate(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return MOTDResult{}, domain.NewCancelledError("motd_cancelled", "MOTD generation was cancelled", nil)
		}
		return s.serviceUnavailableResult(), nil
	}

	text := SanitizeMOTD(raw)
	if text == "" {
		// An empty answer is not a welcome; report the truth
		// rather than show a blank login.
		return s.serviceUnavailableResult(), nil
	}
	return s.approve(text, false), nil
}

// motdVariables maps the documented MOTD variables to this session's facts.
// A variable with no fact here falls back to its documented empty rendering;
// session_elapsed is intentionally absent because generation happens at session
// start, where the documented "less than a minute" rendering is accurate.
func (s *MOTDService) motdVariables(in MOTDInputs) map[string]string {
	return map[string]string{
		"system_name":        s.identity.SystemName,
		"shell_name":         s.identity.ShellName,
		"simulated_hostname": s.identity.Hostname,
		"displayed_username": in.Username,
		"session_started_at": time.UnixMilli(in.SessionTimeUnixMilli).UTC().Format(time.RFC3339),
		"sharing_mode":       sharingToken(in.SharingEnabled),
		"recording_status":   recordingToken(in.PermanentRecording),
		"motd_line_budget":   fmt.Sprint(s.config.LineBudget),
		"prompt_version":     s.config.PromptVersion,
	}
}

// sharingToken renders the effective sharing mode as a short token.
func sharingToken(enabled bool) string {
	if enabled {
		return "sharing on"
	}
	return "sharing off"
}

// recordingToken renders the permanent-recording status as a short token.
func recordingToken(enabled bool) string {
	if enabled {
		return "recording on"
	}
	return "recording off"
}

func (s *MOTDService) serviceUnavailableResult() MOTDResult {
	return s.approve(serviceUnavailableMOTD, true)
}

// approve applies the hard byte and line bounds to policy-approved
// text and records whether truncation happened.
func (s *MOTDService) approve(text string, unavailable bool) MOTDResult {
	if len(text) > s.config.MaxBytes {
		text = truncateToBytes(text, s.config.MaxBytes)
	}
	budgeted, truncated := applyLineBudget(text, s.config.LineBudget)
	return MOTDResult{
		Text:               budgeted,
		PromptVersion:      s.config.PromptVersion,
		Lines:              countLines(budgeted),
		Truncated:          truncated,
		ServiceUnavailable: unavailable,
	}
}

func countLines(text string) int {
	return strings.Count(text, "\n") + 1
}

func truncateToBytes(text string, max int) string {
	if len(text) <= max {
		return text
	}
	// Cut on a rune boundary so multi-byte characters survive.
	cut := max
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func utf8RuneStart(b byte) bool {
	// A UTF-8 continuation byte has the form 10xxxxxx.
	return b&0xC0 != 0x80
}
