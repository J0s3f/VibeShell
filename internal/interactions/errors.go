package interactions

import "errors"

// Package errors. Both of them are domain validation errors so callers can
// classify them with errors.Is against domain.ErrorCategory instead of matching
// package-local strings.
var (
	// errKeySyntax reports an unparsable chord or key sequence.
	errKeySyntax = errors.New("invalid key syntax")
	// errEditTarget reports an operation that needs an editable target in a view
	// that has none, such as a buffer edit on a table cell grid.
	errEditTarget = errors.New("view has no editable target")
)
