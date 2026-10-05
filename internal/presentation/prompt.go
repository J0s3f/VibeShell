package presentation

import (
	"fmt"
	"regexp"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// Prompt validation bounds (PLAN 7.4, 11). The administrator may
// replace the MOTD wording, but the version must stay addressable
// and the text must stay bounded, printable terminal-safe prose.
// Permission safety does not depend on prompt wording: the
// generation request (MOTDGenerationRequest.ModelRequest) carries
// no tool definitions, no scope policy, and no execution
// capability, so no prompt text can expand hard permissions or
// enable real execution. ValidatePrompt enforces the reload-time
// usability half of that rule; the request-shape half is enforced
// structurally and covered by tests.
const (
	// MaxPromptVersionLen bounds the prompt version string.
	MaxPromptVersionLen = 64
	// MaxPromptTextBytes bounds administrator prompt text so a
	// mis-edited prompt cannot turn a login message into an
	// unbounded context payload.
	MaxPromptTextBytes = 8192
)

// promptVersionPattern keeps versions addressable in research
// records and file names: lowercase letters, digits, dot,
// underscore, and dash, starting with an alphanumeric.
var promptVersionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidatePromptVersion reports whether a prompt version is usable.
func ValidatePromptVersion(version string) error {
	if version == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt version is empty", nil)
	}
	if len(version) > MaxPromptVersionLen {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt version exceeds the length limit", map[string]string{
			"limit": fmt.Sprint(MaxPromptVersionLen),
		})
	}
	if !promptVersionPattern.MatchString(version) {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt version is not addressable", map[string]string{
			"version": version,
		})
	}
	return nil
}

// ValidatePrompt reports whether a prompt is safe to store and use.
// It checks addressing, bounds, and printability. It never grants
// capabilities: a prompt that passes validation still generates
// through MOTDGenerationRequest, which carries no tools, no scope
// policy, and no execution channel.
func ValidatePrompt(p MOTDPrompt) error {
	if err := ValidatePromptVersion(p.Version); err != nil {
		return err
	}
	if strings.TrimSpace(p.Text) == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt text is empty", map[string]string{
			"version": p.Version,
		})
	}
	if len(p.Text) > MaxPromptTextBytes {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt text exceeds the size limit", map[string]string{
			"version": p.Version,
			"limit":   fmt.Sprint(MaxPromptTextBytes),
		})
	}
	if strings.IndexFunc(p.Text, isForbiddenRune) >= 0 {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt text contains control characters", map[string]string{
			"version": p.Version,
		})
	}
	if strings.Contains(p.Text, "\x1b") {
		return domain.NewValidationError(domain.CodeInvalidInput, "MOTD prompt text contains escape sequences", map[string]string{
			"version": p.Version,
		})
	}
	return nil
}

// promptPlaceholderPattern matches one {{variable_name}} placeholder.
var promptPlaceholderPattern = regexp.MustCompile(`\{\{[A-Za-z0-9_]+\}\}`)

// motdEmptyRenderings is the documented empty rendering for every MOTD prompt
// variable in prompts/manifest.json. The manifest is per-prompt: the same
// variable name can have a different empty rendering in another slot, so each
// wired slot carries its own table. tests/contract cross-checks this table
// against the manifest's MOTD slot.
var motdEmptyRenderings = map[string]string{
	"system_name":        "VibeOS",
	"shell_name":         "VibeShell",
	"simulated_hostname": "vibeshell",
	"displayed_username": "user",
	"session_started_at": "an unrecorded time",
	"session_elapsed":    "less than a minute",
	"sharing_mode":       "sharing off",
	"recording_status":   "recording off",
	"motd_line_budget":   "12",
	"prompt_version":     "unversioned",
}

// RenderPrompt substitutes every {{variable_name}} with the supplied session
// value or, when no value is supplied, the documented empty rendering for that
// slot. A placeholder absent from both maps is rejected: the renderer fails
// closed rather than sending a raw placeholder to the model.
func RenderPrompt(template string, values, empty map[string]string) (string, error) {
	var undocumented []string
	rendered := promptPlaceholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "{{"), "}}")
		if value := values[name]; value != "" {
			return value
		}
		if fallback, ok := empty[name]; ok {
			return fallback
		}
		undocumented = append(undocumented, name)
		return match
	})
	if len(undocumented) > 0 {
		return "", domain.NewValidationError(domain.CodeInvalidInput,
			"prompt contains undocumented placeholders",
			map[string]string{"placeholders": strings.Join(undocumented, ", ")})
	}
	return rendered, nil
}

// RenderMOTDPrompt renders the MOTD slot with its documented empty renderings.
func RenderMOTDPrompt(template string, values map[string]string) (string, error) {
	return RenderPrompt(template, values, motdEmptyRenderings)
}
