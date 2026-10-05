// Package worldpaths qualifies PLAN 5.2/5.3/5.5 path handling in a
// self-contained spike: absolute resolution from a session cwd, containment
// inside the simulated namespace, and strict component validation.
//
// All resolution is pure string computation over domain.ValidPath values.
// Nothing here touches a host filesystem, clock, or random source.
package worldpaths

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/domain"
)

// Bounds mirror the domain contract and are re-checked on raw input so
// over-long values fail before normalization.
const (
	maxPathLen = domain.MaxPathLen
	maxNameLen = domain.MaxNameLen
	// maxSymlinkDepth matches the conventional Linux ELOOP bound.
	maxSymlinkDepth = 40
)

var (
	ErrEmptyInput   = errors.New("path input is empty")
	ErrSymlinkLoop  = errors.New("symlink resolution loop")
	ErrSymlinkDepth = errors.New("symlink resolution depth exceeded")
	ErrBadComponent = errors.New("invalid path component")
)

// ValidateComponent rejects a single path component before it can enter the
// simulated namespace. It rejects empty components, embedded slashes, NUL,
// other C0/C1 controls and DEL, and over-long names. All other UTF-8,
// including visually confusable or canonically equivalent-but-byte-distinct
// names, is accepted: equality stays byte-exact so two visually identical
// names never silently collide (see unicode test).
func ValidateComponent(name string) error {
	if name == "" {
		return errors.Join(ErrBadComponent, errors.New("empty component"))
	}
	if strings.ContainsRune(name, '/') {
		return errors.Join(ErrBadComponent, errors.New("component contains slash"))
	}
	if len(name) > maxNameLen {
		return errors.Join(ErrBadComponent, domain.ErrNameTooLong)
	}
	if !utf8.ValidString(name) {
		return errors.Join(ErrBadComponent, errors.New("component is not valid UTF-8"))
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) || r == 0x7f {
			return errors.Join(ErrBadComponent, errors.New("component contains control character"))
		}
	}
	if name == "." || name == ".." {
		return errors.Join(ErrBadComponent, errors.New("dot component is not a storable name"))
	}
	return nil
}

// rejectRawInput applies namespace-wide input hygiene: no NUL, no control
// characters, no empty input, and a byte-length cap so oversized inputs fail
// fast at the boundary.
func rejectRawInput(s string) error {
	if s == "" {
		return ErrEmptyInput
	}
	if len(s) > maxPathLen {
		return domain.ErrPathTooLong
	}
	if strings.Contains(s, "\x00") {
		return domain.ErrPathContainsNull
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' {
			// Tab cannot appear in a path either, but report it the same way.
			return errors.Join(domain.NewValidationError(domain.CodeInvalidPath, "path contains control character", nil), errors.New("path contains control character"))
		}
		if r == 0x7f {
			return errors.Join(domain.NewValidationError(domain.CodeInvalidPath, "path contains DEL", nil), errors.New("path contains DEL"))
		}
	}
	if !utf8.ValidString(s) {
		return errors.Join(domain.NewValidationError(domain.CodeInvalidPath, "path is not valid UTF-8", nil), errors.New("path is not valid UTF-8"))
	}
	return nil
}

// ResolveInput deterministically resolves a user-supplied path (absolute or
// relative to cwd) to an absolute simulated path. Dot segments are
// normalized, ".." above root clamps at root, and symlinks from the provided
// table are expanded inside the namespace. The result is always a
// domain.ValidPath; no host path can ever be produced because the computation
// never leaves the component stack.
func ResolveInput(cwd domain.ValidPath, input string, symlinks map[string]string) (domain.ValidPath, error) {
	if err := rejectRawInput(input); err != nil {
		return "", err
	}
	var stack []string
	if strings.HasPrefix(input, "/") {
		stack = nil
	} else {
		if cwd != "/" {
			stack = strings.Split(string(cwd)[1:], "/")
		}
	}
	rawParts := strings.Split(input, "/")
	for _, part := range rawParts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			// Above root stays at root: containment, not an escape.
			continue
		default:
			if err := ValidateComponent(part); err != nil {
				return "", err
			}
			if len("/"+strings.Join(append(append([]string{}, stack...), part), "/")) > maxPathLen {
				return "", domain.ErrPathTooLong
			}
			stack = append(stack, part)
		}
	}
	resolved := expandSymlinks(stack, symlinks)
	if err, failed := resolved.err, resolved.failed; failed {
		return "", err
	}
	stack = resolved.stack
	if len(stack) == 0 {
		return domain.RootPath(), nil
	}
	canonical := "/" + strings.Join(stack, "/")
	parsed, err := domain.ParsePath(canonical)
	if err != nil {
		return "", err
	}
	return parsed, nil
}

type symlinkResult struct {
	stack  []string
	err    error
	failed bool
}

// expandSymlinks walks the component stack and expands entries found in the
// symlink table. Both absolute and relative targets stay inside the simulated
// namespace; loops and excessive depth are errors, never host access.
func expandSymlinks(stack []string, symlinks map[string]string) symlinkResult {
	if len(symlinks) == 0 {
		return symlinkResult{stack: stack}
	}
	current := append([]string{}, stack...)
	for depth := 0; depth <= maxSymlinkDepth; depth++ {
		progress := false
		for i := range current {
			prefix := "/" + strings.Join(current[:i+1], "/")
			if len(current) == 0 {
				prefix = "/"
			}
			target, ok := symlinks[prefix]
			if !ok {
				continue
			}
			if depth == maxSymlinkDepth {
				return symlinkResult{err: ErrSymlinkDepth, failed: true}
			}
			rest := append([]string{}, current[i+1:]...)
			var base []string
			if strings.HasPrefix(target, "/") {
				base = nil
			} else {
				base = append([]string{}, current[:i]...)
			}
			for _, part := range strings.Split(target, "/") {
				switch part {
				case "", ".":
					continue
				case "..":
					if len(base) > 0 {
						base = base[:len(base)-1]
					}
					continue
				default:
					if err := ValidateComponent(part); err != nil {
						return symlinkResult{err: err, failed: true}
					}
					base = append(base, part)
				}
			}
			current = append(base, rest...)
			progress = true
			break // restart scan from root after each expansion
		}
		if !progress {
			return symlinkResult{stack: current}
		}
		// Detect direct self-loop quickly: if we expanded max times, report loop.
		if depth == maxSymlinkDepth {
			return symlinkResult{err: ErrSymlinkLoop, failed: true}
		}
	}
	return symlinkResult{err: ErrSymlinkDepth, failed: true}
}
