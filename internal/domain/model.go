package domain

import (
	"encoding/json"
	"fmt"
)

// Canonical message role.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

// Message is a provider-neutral canonical message.
type Message struct {
	Role       MessageRole `json:"role"`
	Content    string      `json:"content,omitempty"`      // text content
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`   // assistant tool calls
	ToolCallID string      `json:"tool_call_id,omitempty"` // tool result reference
	Name       string      `json:"name,omitempty"`         // tool name for tool messages
}

// ToolCall represents a tool invocation request.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolDefinition is a provider-neutral tool definition.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`       // JSON Schema
	Strict      bool            `json:"strict,omitempty"` // enforce strict schema
}

// ModelRequest is the canonical request to a model gateway.
type ModelRequest struct {
	RouteID     RouteID          `json:"route_id"`
	AccountID   AccountID        `json:"account_id"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	MaxTokens   int              `json:"max_tokens"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	DeadlineMs  int64            `json:"deadline_ms"`        // absolute deadline
	RequestID   string           `json:"request_id"`         // for idempotency
	Metadata    json.RawMessage  `json:"metadata,omitempty"` // route-specific extras
}

// ModelResponse is the canonical response from a model gateway.
type ModelResponse struct {
	RequestID    string       `json:"request_id"`
	RouteID      RouteID      `json:"route_id"`
	AccountID    AccountID    `json:"account_id"`
	Message      Message      `json:"message"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage"`
	LatencyMs    int64        `json:"latency_ms"`
	Timestamp    int64        `json:"timestamp"` // unix milliseconds
}

// FinishReason indicates why the model stopped generating.
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"
	FinishReasonLength        FinishReason = "length"
	FinishReasonToolCalls     FinishReason = "tool_calls"
	FinishReasonContentFilter FinishReason = "content_filter"
	FinishReasonError         FinishReason = "error"
	FinishReasonCancelled     FinishReason = "cancelled"
)

// FailureClass classifies model/provider failures for routing decisions (PLAN 9.1).
type FailureClass string

const (
	// Credential/account failures
	FailureInvalidCredential FailureClass = "invalid_credential"
	FailureQuotaExhausted    FailureClass = "quota_exhausted"
	FailureRateLimited       FailureClass = "rate_limited"

	// Model/provider failures
	FailureModelNotFound  FailureClass = "model_not_found"
	FailureProviderOutage FailureClass = "provider_outage"
	FailureNetworkTimeout FailureClass = "network_timeout"

	// Request failures
	FailureContextTooLong   FailureClass = "context_too_long"
	FailureInvalidArguments FailureClass = "invalid_arguments"
	FailureInvalidResponse  FailureClass = "invalid_response"
	FailureContentRejected  FailureClass = "content_rejected"

	// User/system failures
	FailureUserCancelled FailureClass = "user_cancelled"
	FailureWorldConflict FailureClass = "world_conflict"

	// Unknown
	FailureUnknown FailureClass = "unknown"
)

// AllFailureClasses returns all known failure classes.
func AllFailureClasses() []FailureClass {
	return []FailureClass{
		FailureInvalidCredential, FailureQuotaExhausted, FailureRateLimited,
		FailureModelNotFound, FailureProviderOutage, FailureNetworkTimeout,
		FailureContextTooLong, FailureInvalidArguments, FailureInvalidResponse,
		FailureContentRejected, FailureUserCancelled, FailureWorldConflict,
		FailureUnknown,
	}
}

// IsRetryable returns true if the failure class suggests a retry may succeed.
func (f FailureClass) IsRetryable() bool {
	switch f {
	case FailureRateLimited, FailureProviderOutage, FailureNetworkTimeout,
		FailureContextTooLong, FailureInvalidResponse:
		return true
	default:
		return false
	}
}

// IsCredentialScope returns true if the failure is credential-scoped.
func (f FailureClass) IsCredentialScope() bool {
	return f == FailureInvalidCredential
}

// IsAccountScope returns true if the failure is account-scoped.
func (f FailureClass) IsAccountScope() bool {
	return f == FailureQuotaExhausted || f == FailureRateLimited
}

// IsRouteScope returns true if the failure is route-scoped.
func (f FailureClass) IsRouteScope() bool {
	return f == FailureModelNotFound || f == FailureInvalidResponse
}

// IsProviderScope returns true if the failure is provider-scoped.
func (f FailureClass) IsProviderScope() bool {
	return f == FailureProviderOutage
}

// Error implements the error interface so that a bare failure class can be
// used directly with errors.Is/As (e.g. errors.Is(err, FailureQuotaExhausted)).
func (f FailureClass) Error() string { return string(f) }

// ErrorEnvelope wraps a failure with structured details. Route and account
// are pointers so absent references are omitted instead of emitting
// placeholder IDs that fail identity validation.
type ErrorEnvelope struct {
	Class      FailureClass `json:"class"`
	Message    string       `json:"message"`
	RouteID    *RouteID     `json:"route_id,omitempty"`
	AccountID  *AccountID   `json:"account_id,omitempty"`
	RetryAfter *int64       `json:"retry_after_ms,omitempty"` // milliseconds
	StatusCode int          `json:"status_code,omitempty"`
	RawError   string       `json:"raw_error,omitempty"` // redacted provider error
	Timestamp  int64        `json:"timestamp"`           // unix milliseconds
}

// Error implements the error interface.
func (e ErrorEnvelope) Error() string {
	return fmt.Sprintf("%s: %s", e.Class, e.Message)
}

// Is implements errors.Is for FailureClass.
func (e ErrorEnvelope) Is(target error) bool {
	if te, ok := target.(ErrorEnvelope); ok {
		return e.Class == te.Class
	}
	if tf, ok := target.(FailureClass); ok {
		return e.Class == tf
	}
	return false
}

// As implements errors.As for ErrorEnvelope.
func (e ErrorEnvelope) As(target interface{}) bool {
	if te, ok := target.(*ErrorEnvelope); ok {
		*te = e
		return true
	}
	if tf, ok := target.(*FailureClass); ok {
		*tf = e.Class
		return true
	}
	return false
}

// NewErrorEnvelope creates an error envelope. The caller supplies the
// timestamp from the Clock port so domain construction stays deterministic.
func NewErrorEnvelope(class FailureClass, message string, route RouteID, account AccountID, nowUnixMilli int64) ErrorEnvelope {
	env := ErrorEnvelope{
		Class:     class,
		Message:   message,
		Timestamp: nowUnixMilli,
	}
	if !route.IsZero() {
		env.RouteID = &route
	}
	if !account.IsZero() {
		env.AccountID = &account
	}
	return env
}

// RouteCandidate represents a candidate model route for selection.
type RouteCandidate struct {
	RouteID        RouteID     `json:"route_id"`
	AccountID      AccountID   `json:"account_id"`
	Tier           int         `json:"tier"`
	Priority       int         `json:"priority"` // within tier
	HealthState    HealthState `json:"health_state"`
	QuotaRemaining int64       `json:"quota_remaining,omitempty"` // estimated
}

// Tier represents a routing tier (ordered list of candidates).
type Tier int

// Account represents a provider account with quota grouping.
type Account struct {
	ID                AccountID `json:"id"`
	QuotaGroup        string    `json:"quota_group"`
	PermittedProducts []string  `json:"permitted_products"` // product route IDs
	KeyRefs           []KeyRef  `json:"key_refs"`
	Enabled           bool      `json:"enabled"`
}

// QuotaGroup represents a shared quota across accounts.
type QuotaGroup struct {
	ID         string      `json:"id"`
	AccountIDs []AccountID `json:"account_ids"`
	Limit      int64       `json:"limit,omitempty"` // requests or tokens per window
	WindowMs   int64       `json:"window_ms,omitempty"`
	Used       int64       `json:"used"`
	ResetAt    int64       `json:"reset_at,omitempty"` // unix milliseconds
}

// AttemptHistory records the attempt history for a turn.
type AttemptHistory struct {
	TurnID         TurnID          `json:"turn_id"`
	Attempts       []AttemptRecord `json:"attempts"`
	CurrentAttempt int             `json:"current_attempt"`
	MaxAttempts    int             `json:"max_attempts"`
	TotalLatencyMs int64           `json:"total_latency_ms"`
}

// AttemptRecord records a single model attempt.
type AttemptRecord struct {
	AttemptID AttemptID      `json:"attempt_id"`
	RouteID   RouteID        `json:"route_id"`
	AccountID AccountID      `json:"account_id"`
	StartTime int64          `json:"start_time"`
	EndTime   int64          `json:"end_time,omitempty"`
	Result    AttemptResult  `json:"result"`
	Error     *ErrorEnvelope `json:"error,omitempty"`
	Usage     Usage          `json:"usage,omitempty"`
}

// AttemptResult is the outcome of an attempt.
type AttemptResult string

const (
	AttemptResultSuccess         AttemptResult = "success"
	AttemptResultFailed          AttemptResult = "failed"
	AttemptResultTimeout         AttemptResult = "timeout"
	AttemptResultCancelled       AttemptResult = "cancelled"
	AttemptResultContentRejected AttemptResult = "content_rejected"
)
