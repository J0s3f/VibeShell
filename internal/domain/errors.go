package domain

import (
	"errors"
	"fmt"
)

// ErrorCategory categorizes domain errors for errors.Is/errors.As.
type ErrorCategory string

const (
	CategoryValidation  ErrorCategory = "validation"  // input validation failed
	CategoryConflict    ErrorCategory = "conflict"    // concurrent modification conflict
	CategoryNotFound    ErrorCategory = "not_found"   // entity not found
	CategoryDenied      ErrorCategory = "denied"      // permission/policy denied
	CategoryLimit       ErrorCategory = "limit"       // resource limit exceeded
	CategoryUnavailable ErrorCategory = "unavailable" // dependency unavailable
	CategoryCancelled   ErrorCategory = "cancelled"   // operation cancelled
	CategoryInternal    ErrorCategory = "internal"    // unexpected internal error
)

// Error implements the error interface so that a bare category can be used
// directly with errors.Is/As (e.g. errors.Is(err, CategoryDenied)).
// Without this method the type assertions in DomainError.Is fail to compile,
// since an assertion target must implement error.
func (c ErrorCategory) Error() string { return string(c) }

// DomainError is a structured domain error supporting errors.Is/errors.As.
type DomainError struct {
	Category ErrorCategory     `json:"category"`
	Code     string            `json:"code"`              // machine-readable code
	Message  string            `json:"message"`           // human-readable message
	Details  map[string]string `json:"details,omitempty"` // structured details
	Err      error             `json:"-"`                 // wrapped error
}

// Error implements the error interface.
func (e *DomainError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Category, e.Code, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Code)
}

// Unwrap implements errors.Unwrap for error chaining.
func (e *DomainError) Unwrap() error {
	return e.Err
}

// Is implements errors.Is for ErrorCategory and DomainError.
func (e *DomainError) Is(target error) bool {
	if te, ok := target.(*DomainError); ok {
		return e.Category == te.Category && e.Code == te.Code
	}
	if cat, ok := target.(ErrorCategory); ok {
		return e.Category == cat
	}
	return false
}

// As implements errors.As for DomainError.
func (e *DomainError) As(target interface{}) bool {
	if te, ok := target.(*DomainError); ok {
		*te = *e
		return true
	}
	if cat, ok := target.(*ErrorCategory); ok {
		*cat = e.Category
		return true
	}
	return false
}

// NewValidationError creates a validation error.
func NewValidationError(code, message string, details map[string]string) *DomainError {
	return &DomainError{
		Category: CategoryValidation,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}

// NewConflictError creates a conflict error.
func NewConflictError(code, message string, details map[string]string) *DomainError {
	return &DomainError{
		Category: CategoryConflict,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}

// NewNotFoundError creates a not found error.
func NewNotFoundError(code, message string, details map[string]string) *DomainError {
	return &DomainError{
		Category: CategoryNotFound,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}

// NewDeniedError creates a denied error.
func NewDeniedError(code, message string, details map[string]string) *DomainError {
	return &DomainError{
		Category: CategoryDenied,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}

// NewLimitError creates a limit error.
func NewLimitError(code, message string, details map[string]string) *DomainError {
	return &DomainError{
		Category: CategoryLimit,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}

// NewUnavailableError creates an unavailable error.
func NewUnavailableError(code, message string, details map[string]string, err error) *DomainError {
	return &DomainError{
		Category: CategoryUnavailable,
		Code:     code,
		Message:  message,
		Details:  details,
		Err:      err,
	}
}

// NewCancelledError creates a cancelled error.
func NewCancelledError(code, message string, details map[string]string) *DomainError {
	return &DomainError{
		Category: CategoryCancelled,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}

// NewInternalError creates an internal error.
func NewInternalError(code, message string, err error) *DomainError {
	return &DomainError{
		Category: CategoryInternal,
		Code:     code,
		Message:  message,
		Err:      err,
	}
}

// WrapError wraps an error with a category and code.
func WrapError(err error, category ErrorCategory, code, message string) *DomainError {
	return &DomainError{
		Category: category,
		Code:     code,
		Message:  message,
		Err:      err,
	}
}

// Common error codes.
const (
	// Validation
	CodeInvalidInput       = "invalid_input"
	CodeInvalidIdentity    = "invalid_identity"
	CodeInvalidPath        = "invalid_path"
	CodeInvalidScope       = "invalid_scope"
	CodeInvalidMutation    = "invalid_mutation"
	CodeInvalidAppManifest = "invalid_app_manifest"
	CodeInvalidAppView     = "invalid_app_view"
	CodeInvalidAppResult   = "invalid_app_result"
	CodeInvalidConfig      = "invalid_config"
	CodeInvalidRoutePolicy = "invalid_route_policy"

	// Conflict
	CodeConcurrentModification = "concurrent_modification"
	CodeRevisionMismatch       = "revision_mismatch"
	CodeDuplicateKey           = "duplicate_key"
	CodeStaleRead              = "stale_read"

	// Not Found
	CodeUserNotFound       = "user_not_found"
	CodeSessionNotFound    = "session_not_found"
	CodeTurnNotFound       = "turn_not_found"
	CodeEventNotFound      = "event_not_found"
	CodeNodeNotFound       = "node_not_found"
	CodeNamespaceNotFound  = "namespace_not_found"
	CodeAppNotFound        = "app_not_found"
	CodeAppVersionNotFound = "app_version_not_found"
	CodeContentNotFound    = "content_not_found"
	CodeRouteNotFound      = "route_not_found"
	CodeAccountNotFound    = "account_not_found"

	// Denied
	CodeScopeDenied          = "scope_denied"
	CodeSharingDisabled      = "sharing_disabled"
	CodePermissionDenied     = "permission_denied"
	CodeAuthenticationFailed = "authentication_failed"
	CodeAuthorizationFailed  = "authorization_failed"

	// Limit
	CodeRateLimited        = "rate_limited"
	CodeQuotaExceeded      = "quota_exceeded"
	CodeMaxAttemptsReached = "max_attempts_reached"
	CodeContextTooLong     = "context_too_long"
	CodeOutputTooLarge     = "output_too_large"
	CodeMaxSessionsReached = "max_sessions_reached"
	CodeMaxAppsReached     = "max_apps_reached"
	CodeStorageFull        = "storage_full"

	// Unavailable
	CodeModelUnavailable    = "model_unavailable"
	CodeProviderUnavailable = "provider_unavailable"
	CodeDatabaseUnavailable = "database_unavailable"
	CodeSandboxUnavailable  = "sandbox_unavailable"
	CodeNetworkUnavailable  = "network_unavailable"
	// CodeRecordingUnavailable reports that permanent recording can no longer
	// continue (PLAN 10.3): the durable store is read-only, out of space, or
	// otherwise unwritable, so semantic work must stop rather than run without
	// logs.
	CodeRecordingUnavailable = "recording_unavailable"

	// Cancelled
	CodeUserCancelled = "user_cancelled"
	CodeTimeout       = "timeout"
	CodeShutdown      = "shutdown"

	// Internal
	CodeSerializationFailed   = "serialization_failed"
	CodeDeserializationFailed = "deserialization_failed"
	CodeInvariantViolation    = "invariant_violation"
)

// IsValidationError returns true if the error is a validation error.
func IsValidationError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryValidation
}

// IsConflictError returns true if the error is a conflict error.
func IsConflictError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryConflict
}

// IsNotFoundError returns true if the error is a not found error.
func IsNotFoundError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryNotFound
}

// IsDeniedError returns true if the error is a denied error.
func IsDeniedError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryDenied
}

// IsLimitError returns true if the error is a limit error.
func IsLimitError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryLimit
}

// IsUnavailableError returns true if the error is an unavailable error.
func IsUnavailableError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryUnavailable
}

// IsCancelledError returns true if the error is a cancelled error.
func IsCancelledError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryCancelled
}

// IsInternalError returns true if the error is an internal error.
func IsInternalError(err error) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Category == CategoryInternal
}

// GetErrorCode extracts the error code from a domain error.
func GetErrorCode(err error) string {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// GetErrorCategory extracts the error category from a domain error.
func GetErrorCategory(err error) ErrorCategory {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Category
	}
	return ""
}
