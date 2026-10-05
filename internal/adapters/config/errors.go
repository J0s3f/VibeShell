package config

import (
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// maxProblemsInError bounds an operator-facing message; the full problem
// list stays available on the struct for tooling.
const maxProblemsInError = 25

// Problem is one configuration defect together with the exact JSON path it
// was found at. Paths use the document's own names ("tiers[1].routes[0]"),
// so an operator can jump straight to the offending line.
type Problem struct {
	Path    string
	Message string
}

// String renders "path: message", or just the message for document-wide
// problems such as a JSON syntax error.
func (p Problem) String() string {
	if p.Path == "" {
		return p.Message
	}
	return p.Path + ": " + p.Message
}

// ValidationError collects every problem found in one pass so an operator
// fixes a configuration once instead of iterating. It unwraps into one
// *domain.DomainError per problem (category validation, code invalid_config),
// so errors.Is and errors.As keep working against the shared failure
// vocabulary while the message stays human-readable.
type ValidationError struct {
	Problems []Problem
}

// Error implements the error interface with a bounded, ordered summary.
func (e *ValidationError) Error() string {
	if len(e.Problems) == 0 {
		return "configuration invalid"
	}
	shown := e.Problems
	if len(shown) > maxProblemsInError {
		shown = shown[:maxProblemsInError]
	}
	parts := make([]string, 0, len(shown))
	for _, p := range shown {
		parts = append(parts, p.String())
	}
	msg := fmt.Sprintf("%d validation problem(s): %s", len(e.Problems), strings.Join(parts, "; "))
	if len(e.Problems) > len(shown) {
		msg += fmt.Sprintf("; ... and %d more", len(e.Problems)-len(shown))
	}
	return msg
}

// Unwrap exposes one typed domain error per problem for errors.Is/errors.As.
func (e *ValidationError) Unwrap() []error {
	errs := make([]error, 0, len(e.Problems))
	for _, p := range e.Problems {
		details := map[string]string{}
		if p.Path != "" {
			details["path"] = p.Path
		}
		errs = append(errs, &domain.DomainError{
			Category: domain.CategoryValidation,
			Code:     domain.CodeInvalidConfig,
			Message:  p.String(),
			Details:  details,
		})
	}
	return errs
}

// newValidationError builds an error from problems, or nil when there are
// none, so callers can write "if err := newValidationError(problems); err".
func newValidationError(problems []Problem) error {
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}
