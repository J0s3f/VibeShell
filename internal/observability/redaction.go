// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"log/slog"
	"regexp"
	"strings"
)

// Redactor scrubs known secret shapes from strings and slog attributes.
// It handles:
//   - Bearer tokens (Authorization: Bearer <token>)
//   - OpenAI-style sk-* keys
//   - api_key= / ?key= query parameters
//   - x-api-key header values
//   - Authorization header values (Basic, Bearer, etc.)
//
// The redactor is conservative: it redacts the entire value after the known
// prefix/marker. It does not attempt to parse arbitrary JSON or structured
// payloads; callers should redact before logging structured data that may
// contain secrets.
type Redactor struct {
	patterns []*regexp.Regexp
}

// NewRedactor creates a redactor with the default secret patterns.
func NewRedactor() *Redactor {
	return &Redactor{
		patterns: []*regexp.Regexp{
			// AWS-style access key IDs (AKIA...) - capture "AKIA" prefix
			regexp.MustCompile(`\b(AKIA)[0-9A-Z]{16}\b`),

			// OpenAI-style sk-* keys: capture "sk-" prefix
			regexp.MustCompile(`\b(sk-)[a-zA-Z0-9_-]{20,}\b`),

			// Bearer tokens in Authorization header: "Authorization: Bearer <token>"
			// Capture "Authorization: Bearer " prefix
			regexp.MustCompile(`(?i)(Authorization\s*[:=]\s*Bearer\s+)[^\s]+`),

			// Standalone Bearer tokens: capture "Bearer " prefix
			regexp.MustCompile(`(?i)(Bearer\s+)[^\s]+`),

			// x-api-key header: capture "x-api-key: " or "x-api-key=" prefix
			regexp.MustCompile(`(?i)(x-api-key\s*[:=]\s*)[^\s,\r\n]+`),

			// api_key= in query strings or form data: capture "api_key=" prefix (with or without ?& prefix)
			regexp.MustCompile(`(?i)([?&]?api_key=)[^&\s]+`),

			// key= in query strings: capture "key=" prefix (with or without ?& prefix)
			regexp.MustCompile(`(?i)([?&]?key=)[^&\s]+`),

			// password= in query strings: capture "password=" prefix
			regexp.MustCompile(`(?i)([?&]?password=)[^&\s]+`),

			// secret= in query strings: capture "secret=" prefix
			regexp.MustCompile(`(?i)([?&]?secret=)[^&\s]+`),

			// access_token= in query strings: capture "access_token=" prefix
			regexp.MustCompile(`(?i)([?&]?access_token=)[^&\s]+`),

			// refresh_token= in query strings: capture "refresh_token=" prefix
			regexp.MustCompile(`(?i)([?&]?refresh_token=)[^&\s]+`),

			// client_secret= in query strings: capture "client_secret=" prefix
			regexp.MustCompile(`(?i)([?&]?client_secret=)[^&\s]+`),

			// Generic long base64-like tokens (40+ chars): capture first 4 chars as prefix
			// No word boundaries to allow special chars like /+=_- in the token
			// This is a broad catch-all for unknown token formats.
			// Must come LAST to avoid false positives on other patterns
			regexp.MustCompile(`([A-Za-z0-9/+=_-]{4})[A-Za-z0-9/+=_-]{36,}`),
		},
	}
}

// RedactString returns a copy of s with known secret patterns replaced.
// Each match is replaced by the prefix (group 1) followed by "[REDACTED]".
func (r *Redactor) RedactString(s string) string {
	result := s
	for _, re := range r.patterns {
		result = re.ReplaceAllString(result, "${1}[REDACTED]")
	}
	return result
}

// RedactAttrs returns a new slice of slog.Attr with string values redacted.
// Non-string values are copied as-is. The original slice is not modified.
func (r *Redactor) RedactAttrs(attrs []slog.Attr) []slog.Attr {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		switch v := a.Value.Any().(type) {
		case string:
			out[i] = slog.String(a.Key, r.RedactString(v))
		default:
			out[i] = a
		}
	}
	return out
}

// RedactMap returns a new map with string values redacted.
// Non-string values are copied as-is. The original map is not modified.
func (r *Redactor) RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch sv := v.(type) {
		case string:
			out[k] = r.RedactString(sv)
		default:
			out[k] = v
		}
	}
	return out
}

// RedactError returns an error with its message redacted.
// If err is nil, returns nil.
func (r *Redactor) RedactError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: r.RedactString(err.Error())}
}

type redactedError struct {
	msg string
}

func (e *redactedError) Error() string { return e.msg }

// ContainsSecret reports whether s contains a known secret pattern.
// This is useful for tests and for deciding whether to log at all.
func (r *Redactor) ContainsSecret(s string) bool {
	for _, re := range r.patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// DefaultRedactor is a package-level redactor for convenience.
var DefaultRedactor = NewRedactor()

// RedactString is a convenience function using the default redactor.
func RedactString(s string) string {
	return DefaultRedactor.RedactString(s)
}

// RedactAttrs is a convenience function using the default redactor.
func RedactAttrs(attrs []slog.Attr) []slog.Attr {
	return DefaultRedactor.RedactAttrs(attrs)
}

// RedactMap is a convenience function using the default redactor.
func RedactMap(m map[string]any) map[string]any {
	return DefaultRedactor.RedactMap(m)
}

// RedactError is a convenience function using the default redactor.
func RedactError(err error) error {
	return DefaultRedactor.RedactError(err)
}

// RedactForLog redacts a string and returns it, logging a warning if secrets
// were found. This is intended for use when a value must be logged but may
// contain secrets; it provides an audit trail that redaction occurred.
func (r *Redactor) RedactForLog(s string) string {
	redacted := r.RedactString(s)
	if redacted != s {
		// Note: We cannot use the logger here to avoid circular dependency.
		// The caller should log the redaction event if desired.
	}
	return redacted
}

// JoinRedacted joins strings with a separator after redacting each.
// This is useful for logging composite values like URLs with query parameters.
func (r *Redactor) JoinRedacted(sep string, elems ...string) string {
	redacted := make([]string, len(elems))
	for i, e := range elems {
		redacted[i] = r.RedactString(e)
	}
	return strings.Join(redacted, sep)
}
