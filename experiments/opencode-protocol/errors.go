package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ErrorCategory is the redacted, provider-neutral failure class. It is
// derived from HTTP status plus structured error fields together: neither
// alone is trusted.
type ErrorCategory string

const (
	CategoryAuthentication ErrorCategory = "authentication"
	CategoryQuota          ErrorCategory = "quota"
	CategoryRateLimit      ErrorCategory = "rate_limit"
	CategoryModelNotFound  ErrorCategory = "model_not_found"
	CategoryProviderOutage ErrorCategory = "provider_outage"
	CategoryContextLength  ErrorCategory = "context_length"
	CategoryBadRequest     ErrorCategory = "bad_request"
	CategoryContentReject  ErrorCategory = "content_rejected"
	CategoryUnknown        ErrorCategory = "unknown"
)

// ProviderError is the canonical redacted error envelope. It never carries
// credential material: Message and ProviderCode pass through Redact.
type ProviderError struct {
	Status       int
	Category     ErrorCategory
	Retryable    bool
	Message      string
	ProviderCode string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("gateway: status=%d category=%s retryable=%t: %s",
		e.Status, e.Category, e.Retryable, e.Message)
}

// CheckStatus maps an HTTP response status plus its bounded body to either
// nil (success statuses) or a redacted *ProviderError. A 200 carrying an
// error envelope is an error: HTTP success alone is never success.
func CheckStatus(status int, body []byte) error {
	if status >= 200 && status < 300 {
		if perr := sniffErrorEnvelope(status, body); perr != nil {
			return perr
		}
		return nil
	}
	return NormalizeError(status, body)
}

// NormalizeError maps status plus structured fields to the canonical
// envelope. Unknown shapes get bounded treatment (CategoryUnknown), never
// silent success.
func NormalizeError(status int, body []byte) *ProviderError {
	fields := extractErrorFields(body)
	msg := fields.message
	if msg == "" {
		msg = fmt.Sprintf("request failed with status %d", status)
	}
	code := fields.code
	lower := strings.ToLower(msg + " " + code + " " + fields.kind)
	category := CategoryUnknown
	retryable := false
	switch {
	case status == 401 || status == 403 || containsAny(lower, []string{"invalid api key", "incorrect api key", "api key not valid", "not a valid key", "unauthorized", "revoked", "invalid_api_key", "authentication_error"}):
		category = CategoryAuthentication
	case status == 404 || containsAny(lower, []string{"model_not_found", "not_found", "does not exist", "no such model"}):
		category = CategoryModelNotFound
	case containsAny(lower, []string{"context_length", "context length", "too many tokens", "maximum context", "input_too_long", "max_tokens"}):
		category = CategoryContextLength
	case containsAny(lower, []string{"content_policy", "content_filter", "refusal", "blocked", "safety"}):
		category = CategoryContentReject
	case status == 402 || containsAny(lower, []string{"quota", "insufficient", "balance", "billing", "credit"}):
		category = CategoryQuota
	case status == 429 || containsAny(lower, []string{"rate_limit", "rate limit", "too many requests", "throttl"}):
		category = CategoryRateLimit
		retryable = true
	case status >= 500 || containsAny(lower, []string{"overloaded", "unavailable", "timeout", "internal error"}):
		category = CategoryProviderOutage
		retryable = true
	case status >= 400:
		category = CategoryBadRequest
	}
	if status >= 500 {
		category = CategoryProviderOutage
		retryable = true
	}
	return &ProviderError{
		Status:       status,
		Category:     category,
		Retryable:    retryable,
		Message:      Redact(truncate(msg, 500)),
		ProviderCode: Redact(truncate(code, 120)),
	}
}

// normalizeEnvelope builds a *ProviderError from an already-extracted error
// payload string (SSE error event or embedded error object).
func normalizeEnvelope(status int, payload string) *ProviderError {
	return NormalizeError(status, []byte(payload))
}

// errorFields holds the structured fields of the three documented envelope
// shapes: OpenAI {error:{message,type,code}}, Anthropic
// {type,error:{type,message}}, Gemini {error:{message,code,status}}.
type errorFields struct {
	message string
	code    string
	kind    string
}

func extractErrorFields(body []byte) errorFields {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return errorFields{message: truncate(strings.TrimSpace(trimmed), 500)}
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(body, &v); err != nil {
		return errorFields{}
	}
	var out errorFields
	if raw, ok := v["error"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out.message = s
		} else {
			var obj struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    any    `json:"code"`
				Status  string `json:"status"`
			}
			if json.Unmarshal(raw, &obj) == nil {
				out.message = obj.Message
				out.kind = obj.Type + " " + obj.Status
				out.code = stringifyCode(obj.Code)
			}
		}
	}
	if out.message == "" {
		var s string
		if raw, ok := v["message"]; ok && json.Unmarshal(raw, &s) == nil {
			out.message = s
		}
	}
	if t, ok := v["type"]; ok {
		var s string
		if json.Unmarshal(t, &s) == nil {
			out.kind += " " + s
		}
	}
	if out.code == "" {
		if raw, ok := v["code"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				out.code = s
			} else {
				out.code = stringifyCodeRaw(raw)
			}
		}
	}
	return out
}

func stringifyCode(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case float64:
		return fmt.Sprintf("%.0f", c)
	default:
		return fmt.Sprintf("%v", c)
	}
}

func stringifyCodeRaw(raw json.RawMessage) string {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return fmt.Sprintf("%.0f", f)
	}
	return ""
}

// sniffErrorEnvelope reports a *ProviderError when body is a JSON error
// envelope (the HTTP-200-with-error case). It returns nil for SSE payloads,
// empty bodies, and non-error JSON.
func sniffErrorEnvelope(status int, body []byte) *ProviderError {
	trimmed := strings.TrimSpace(string(body))
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	fields := extractErrorFields(body)
	if fields.message == "" && !strings.Contains(trimmed, `"error"`) {
		return nil
	}
	if fields.message == "" {
		return nil
	}
	return NormalizeError(status, body)
}

// sniffStreamError scans decoded SSE events for an error envelope when no
// content decoded successfully.
func sniffStreamError(events []SSEEvent) *ProviderError {
	for _, ev := range events {
		if ev.Type == "error" {
			return normalizeEnvelope(200, strings.TrimSpace(ev.Data))
		}
		if perr := sniffErrorEnvelope(200, []byte(ev.Data)); perr != nil {
			return perr
		}
	}
	return nil
}

// emptyStreamError reports a bare-body error envelope when a 200 response
// carried JSON instead of SSE, and a truncation error otherwise. HTTP
// success alone is never stream success.
func emptyStreamError(bare []string, what string) error {
	if len(bare) > 0 {
		if perr := sniffErrorEnvelope(200, []byte(strings.Join(bare, "\n"))); perr != nil {
			return perr
		}
	}
	return fmt.Errorf("gateway: %w: empty %s stream", errTruncated, what)
}

// chatErrorMessage renders an embedded chat error object for normalization.
func chatErrorMessage(v any) string {
	b, _ := json.Marshal(map[string]any{"error": v})
	return string(b)
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// Credential-shaped patterns that must never survive into errors or logs.
// The values below are illustrative; Redact removes anything with these
// shapes regardless of origin.
var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|secret)\s*[:=]\s*['"]?[^\s'"]+`),
	regexp.MustCompile(`(?i)([?&](?:key|api_key|token|auth)=)[^&\s]+`),
	regexp.MustCompile(`x-api-key\s*:\s*[^\s]+`),
	regexp.MustCompile(`(?i)authorization\s*:\s*[^\n\r]+`),
}

// Redact replaces credential-shaped material with [REDACTED]. It is applied
// to every error message, provider code, and log line the gateway emits.
func Redact(s string) string {
	out := s
	for _, re := range redactPatterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}
