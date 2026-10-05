package opencode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// category is the provider-neutral failure class derived from HTTP status
// plus structured error fields together; neither alone is trusted.
type category string

const (
	categoryAuthentication category = "authentication"
	categoryQuota          category = "quota"
	categoryRateLimit      category = "rate_limit"
	categoryModelNotFound  category = "model_not_found"
	categoryProviderOutage category = "provider_outage"
	categoryContextLength  category = "context_length"
	categoryBadRequest     category = "bad_request"
	categoryContentReject  category = "content_rejected"
	categoryUnknown        category = "unknown"
)

// failureClass maps an internal category onto the PLAN 9.1 routing failure
// class carried in domain.ErrorEnvelope.
func (c category) failureClass() domain.FailureClass {
	switch c {
	case categoryAuthentication:
		return domain.FailureInvalidCredential
	case categoryQuota:
		return domain.FailureQuotaExhausted
	case categoryRateLimit:
		return domain.FailureRateLimited
	case categoryModelNotFound:
		return domain.FailureModelNotFound
	case categoryProviderOutage:
		return domain.FailureProviderOutage
	case categoryContextLength:
		return domain.FailureContextTooLong
	case categoryBadRequest:
		return domain.FailureInvalidArguments
	case categoryContentReject:
		return domain.FailureContentRejected
	default:
		return domain.FailureUnknown
	}
}

// retryable reports whether the category warrants bounded retry/failover.
func (c category) retryable() bool {
	switch c {
	case categoryRateLimit, categoryProviderOutage:
		return true
	default:
		return false
	}
}

// providerError is the redacted, adapter-local error envelope. It never
// carries credential material: message and provider code pass through
// Redact. It is converted to domain.ErrorEnvelope at the Gateway boundary.
type providerError struct {
	Status       int
	Category     category
	Retryable    bool
	Message      string
	ProviderCode string
}

func (e *providerError) Error() string {
	return fmt.Sprintf("opencode: status=%d category=%s retryable=%t: %s",
		e.Status, e.Category, e.Retryable, e.Message)
}

// envelope converts to the domain error envelope for the ModelGateway
// boundary. RetryAfter carries parsed Retry-After milliseconds when the
// caller supplies it; route/account scope is attached by the caller.
func (e *providerError) envelope(route domain.RouteID, account domain.AccountID, retryAfter *int64, now int64) domain.ErrorEnvelope {
	env := domain.NewErrorEnvelope(e.Category.failureClass(), e.Message, route, account, now)
	env.StatusCode = e.Status
	env.RetryAfter = retryAfter
	if e.ProviderCode != "" {
		env.RawError = e.ProviderCode
	}
	return env
}

// checkStatus maps an HTTP response status plus its bounded body to either
// nil (success statuses) or a redacted *providerError. A 200 carrying an
// error envelope is an error: HTTP success alone is never success.
func checkStatus(status int, body []byte) error {
	if status >= 200 && status < 300 {
		if perr := sniffErrorEnvelope(status, body); perr != nil {
			return perr
		}
		return nil
	}
	return normalizeError(status, body)
}

// normalizeError maps status plus structured fields to the canonical
// envelope. Unknown shapes get bounded treatment (categoryUnknown), never
// silent success.
func normalizeError(status int, body []byte) *providerError {
	fields := extractErrorFields(body)
	msg := fields.message
	if msg == "" {
		msg = fmt.Sprintf("request failed with status %d", status)
	}
	code := fields.code
	lower := strings.ToLower(msg + " " + code + " " + fields.kind)
	cat := categoryUnknown
	switch {
	case status == 401 || status == 403 || containsAny(lower, []string{"invalid api key", "incorrect api key", "api key not valid", "not a valid key", "unauthorized", "revoked", "invalid_api_key", "authentication_error"}):
		cat = categoryAuthentication
	case status == 404 || containsAny(lower, []string{"model_not_found", "not_found", "does not exist", "no such model"}):
		cat = categoryModelNotFound
	case containsAny(lower, []string{"context_length", "context length", "too many tokens", "maximum context", "input_too_long", "max_tokens"}):
		cat = categoryContextLength
	case containsAny(lower, []string{"content_policy", "content_filter", "refusal", "blocked", "safety"}):
		cat = categoryContentReject
	case status == 402 || containsAny(lower, []string{"quota", "insufficient", "balance", "billing", "credit"}):
		cat = categoryQuota
	case status == 429 || containsAny(lower, []string{"rate_limit", "rate limit", "too many requests", "throttl"}):
		cat = categoryRateLimit
	case status >= 500 || containsAny(lower, []string{"overloaded", "unavailable", "timeout", "internal error"}):
		cat = categoryProviderOutage
	case status >= 400:
		cat = categoryBadRequest
	}
	if status >= 500 {
		cat = categoryProviderOutage
	}
	return &providerError{
		Status:       status,
		Category:     cat,
		Retryable:    cat.retryable(),
		Message:      Redact(truncate(msg, 500)),
		ProviderCode: Redact(truncate(code, 120)),
	}
}

// normalizeEnvelope builds a *providerError from an already-extracted
// error payload string (SSE error event or embedded error object).
func normalizeEnvelope(status int, payload string) *providerError {
	return normalizeError(status, []byte(payload))
}

// errorFields holds the structured fields of the three documented
// envelope shapes: OpenAI {error:{message,type,code}}, Anthropic
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

// sniffErrorEnvelope reports a *providerError when body is a JSON error
// envelope (the HTTP-200-with-error case). It returns nil for SSE
// payloads, empty bodies, and non-error JSON.
func sniffErrorEnvelope(status int, body []byte) *providerError {
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
	return normalizeError(status, body)
}

// sniffStreamError scans decoded SSE events for an error envelope when no
// content decoded successfully.
func sniffStreamError(events []SSEEvent) *providerError {
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
	return fmt.Errorf("opencode: %w: empty %s stream", errTruncated{}, what)
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

// Redact replaces credential-shaped material with [REDACTED]. It is
// applied to every error message, provider code, and log line the adapter
// emits. Key material itself is never passed through Redact as a
// substitute for not handling it: secrets travel only in the Authorization
// header value and are never formatted into errors.
func Redact(s string) string {
	out := s
	for _, re := range redactPatterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}
