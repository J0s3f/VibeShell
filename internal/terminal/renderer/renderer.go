// Package renderer implements ports.TerminalRenderer: it validates a
// declarative view and emits the ANSI frame that shows it (PLAN 6.1, 6.2).
//
// The renderer is the only place in the application that writes terminal
// control sequences. A view carries data, not sequences: content and status
// text are written through the screen model, which keeps the cursor-motion
// controls and drops every other control byte, so no view can set the terminal
// title, open a hyperlink, or hand text to the clipboard. Frames are stored
// through the content port and returned as content references, so the research
// record keeps the exact bytes a session emitted.
package renderer

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/terminal/screen"
)

// FrameMediaType identifies stored ANSI frames in the content store.
const FrameMediaType = "application/x-vibeshell-ansi-frame"

// Limits bounds what one frame may contain. The renderer refuses oversized work
// before it allocates or stores anything, because a view is data from a
// generated application and must not be able to make the service produce
// unbounded output.
type Limits struct {
	// MaxFrameBytes is the largest frame the renderer will store.
	MaxFrameBytes int
	// MaxBufferBytes is the largest view buffer the renderer will read.
	MaxBufferBytes int64
	// MaxStatusBytes is the largest status line the renderer will show.
	MaxStatusBytes int
	// MaxKeyBindings is the largest number of key bindings a view may declare.
	MaxKeyBindings int
	// MaxMetadataBytes is the largest mode-specific metadata the renderer will
	// carry into the record without interpreting it.
	MaxMetadataBytes int
	// DefaultSize is the viewport used for a view that declares none.
	DefaultSize screen.Size
}

// DefaultLimits returns bounds sized for one SSH session: a megabyte frame, a
// megabyte buffer, a 1 KiB status line, and the 80x24 viewport a client gets
// when it asks for no window size.
func DefaultLimits() Limits {
	return Limits{
		MaxFrameBytes:    1 << 20,
		MaxBufferBytes:   1 << 20,
		MaxStatusBytes:   1 << 10,
		MaxKeyBindings:   64,
		MaxMetadataBytes: 64 << 10,
		DefaultSize:      screen.Size{Cols: 80, Rows: 24},
	}
}

// ErrInvalidLimits reports a renderer limit that is not usable.
var ErrInvalidLimits = errors.New("invalid renderer limits")

func (l Limits) validate() error {
	for name, value := range map[string]int{
		"MaxFrameBytes":    l.MaxFrameBytes,
		"MaxBufferBytes":   int(l.MaxBufferBytes),
		"MaxStatusBytes":   l.MaxStatusBytes,
		"MaxKeyBindings":   l.MaxKeyBindings,
		"MaxMetadataBytes": l.MaxMetadataBytes,
	} {
		if value <= 0 {
			return fmt.Errorf("%w: %s must be positive, got %d", ErrInvalidLimits, name, value)
		}
	}
	if !l.DefaultSize.Valid() {
		return fmt.Errorf("%w: default size %dx%d", ErrInvalidLimits, l.DefaultSize.Cols, l.DefaultSize.Rows)
	}
	return nil
}

// Renderer renders validated views into content-addressed ANSI frames.
type Renderer struct {
	store  ports.ContentStore
	limits Limits
}

var _ ports.TerminalRenderer = (*Renderer)(nil)

// New returns a renderer that stores frames through the given content port.
func New(store ports.ContentStore, limits Limits) (*Renderer, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: content store is required", ErrInvalidLimits)
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &Renderer{store: store, limits: limits}, nil
}

// RenderView validates a declarative view, renders it, and stores the frame as
// an immutable content reference. A view that fails validation never reaches
// the screen model, and a frame that would exceed the bounds is refused rather
// than truncated. The session is not part of the frame: a stored frame is a
// session-independent record of what was emitted, and the session layer
// records which session it belongs to.
func (r *Renderer) RenderView(ctx context.Context, _ domain.SessionID, view domain.AppView) (domain.ContentRef, int64, error) {
	if err := domain.ValidateView(view); err != nil {
		return domain.EmptyContentRef(), 0, domain.NewValidationError(
			domain.CodeInvalidAppView, "app view failed domain validation",
			map[string]string{"mode": view.Mode, "cause": err.Error()},
		)
	}
	plan, err := r.plan(view)
	if err != nil {
		return domain.EmptyContentRef(), 0, err
	}
	content, err := r.buffer(ctx, view.BufferRef)
	if err != nil {
		return domain.EmptyContentRef(), 0, err
	}
	frame, err := buildFrame(plan, content)
	if err != nil {
		return domain.EmptyContentRef(), 0, err
	}
	if len(frame) > r.limits.MaxFrameBytes {
		return domain.EmptyContentRef(), 0, domain.NewLimitError(
			domain.CodeOutputTooLarge, "rendered frame exceeds the frame limit",
			map[string]string{
				"bytes": strconv.Itoa(len(frame)),
				"limit": strconv.Itoa(r.limits.MaxFrameBytes),
			},
		)
	}
	return r.storeFrame(ctx, frame)
}

// WriteText emits plain output with the cursor left at the start of a fresh
// line, which is where the session's prompt continues. The text is sanitized,
// so output that contains control sequences shows them as nothing rather than
// acting on the terminal.
func (r *Renderer) WriteText(ctx context.Context, _ domain.SessionID, text string) (int64, error) {
	if int64(len(text)) > r.limits.MaxBufferBytes {
		return 0, domain.NewLimitError(
			domain.CodeOutputTooLarge, "text output exceeds the buffer limit",
			map[string]string{
				"bytes": strconv.FormatInt(int64(len(text)), 10),
				"limit": strconv.FormatInt(r.limits.MaxBufferBytes, 10),
			},
		)
	}
	frame := make([]byte, 0, len(text)+16)
	frame = append(frame, screen.ResetStyle...)
	frame = append(frame, screen.NormalizeLineFeeds(screen.SanitizeText(text))...)
	frame = append(frame, screen.ResetStyle...)
	frame = append(frame, screen.CarriageReturn...)
	frame = append(frame, screen.LineFeed...)
	if len(frame) > r.limits.MaxFrameBytes {
		return 0, domain.NewLimitError(
			domain.CodeOutputTooLarge, "text frame exceeds the frame limit",
			map[string]string{"limit": strconv.Itoa(r.limits.MaxFrameBytes)},
		)
	}
	_, size, err := r.storeFrame(ctx, frame)
	return size, err
}

// storeFrame stores a frame and returns its reference and exact byte count. The
// size the content store reports is checked against the bytes written, because
// a frame recorded with the wrong size would make the research record and the
// transcript disagree.
func (r *Renderer) storeFrame(ctx context.Context, frame []byte) (domain.ContentRef, int64, error) {
	ref, err := r.store.Put(ctx, frame, FrameMediaType)
	if err != nil {
		return domain.EmptyContentRef(), 0, err
	}
	if ref.Size != int64(len(frame)) {
		return domain.EmptyContentRef(), 0, domain.NewInternalError(
			domain.CodeInvariantViolation, "content store reported the wrong frame size",
			fmt.Errorf("stored %d bytes but reported %d", len(frame), ref.Size),
		)
	}
	return ref, ref.Size, nil
}

// buffer reads a view's content, refusing a reference larger than the limit
// before any read happens.
func (r *Renderer) buffer(ctx context.Context, ref *domain.ContentRef) (string, error) {
	if ref == nil {
		return "", nil
	}
	if ref.Size > r.limits.MaxBufferBytes {
		return "", domain.NewLimitError(
			domain.CodeOutputTooLarge, "view buffer exceeds the buffer limit",
			map[string]string{
				"hash":  ref.Hash.String(),
				"bytes": strconv.FormatInt(ref.Size, 10),
				"limit": strconv.FormatInt(r.limits.MaxBufferBytes, 10),
			},
		)
	}
	data, err := r.store.Get(ctx, *ref, 0, r.limits.MaxBufferBytes)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// fullScreenModes are the view modes that take over the whole terminal and
// therefore need the alternate screen. The text mode stays on the primary
// screen, next to the shell's own output.
var fullScreenModes = map[string]bool{
	domain.AppViewModeForm:   true,
	domain.AppViewModeTable:  true,
	domain.AppViewModeEditor: true,
	domain.AppViewModePager:  true,
	domain.AppViewModeStatus: true,
}

// plan is a validated view reduced to what the frame needs.
type plan struct {
	size      screen.Size
	cursorRow int
	cursorCol int
	status    string
	altScreen bool
}

// plan validates the renderer-side bounds of a view and resolves its geometry.
// domain.ValidateView checks the mode and the approved actions; the checks here
// add what the renderer alone can decide, because only the renderer knows the
// size a frame may have.
func (r *Renderer) plan(view domain.AppView) (plan, error) {
	size := r.limits.DefaultSize
	if view.Viewport.Width > 0 || view.Viewport.Height > 0 {
		size = screen.Size{Cols: view.Viewport.Width, Rows: view.Viewport.Height}
	}
	if err := r.validateGeometry(size, view.Cursor); err != nil {
		return plan{}, err
	}
	if len(view.KeyBindings) > r.limits.MaxKeyBindings {
		return plan{}, domain.NewLimitError(
			domain.CodeInvalidAppView, "view declares too many key bindings",
			map[string]string{"count": strconv.Itoa(len(view.KeyBindings))},
		)
	}
	for _, binding := range view.KeyBindings {
		if binding.Key == "" {
			return plan{}, domain.NewValidationError(
				domain.CodeInvalidAppView, "key binding without a key",
				map[string]string{"action": binding.Action},
			)
		}
	}
	if len(view.StatusLine) > r.limits.MaxStatusBytes {
		return plan{}, domain.NewLimitError(
			domain.CodeInvalidAppView, "status line exceeds its limit",
			map[string]string{"bytes": strconv.Itoa(len(view.StatusLine))},
		)
	}
	if len(view.Metadata) > r.limits.MaxMetadataBytes {
		return plan{}, domain.NewLimitError(
			domain.CodeInvalidAppView, "view metadata exceeds its limit",
			map[string]string{"bytes": strconv.Itoa(len(view.Metadata))},
		)
	}
	// A view that declares no cursor leaves it where its content ended, which
	// is the trailing state the session's prompt continues from.
	cursorRow, cursorCol := view.Cursor.Row, view.Cursor.Col
	if cursorRow == 0 && cursorCol == 0 {
		cursorRow, cursorCol = size.Rows-1, 0
	}
	return plan{
		size:      size,
		cursorRow: cursorRow,
		cursorCol: cursorCol,
		status:    view.StatusLine,
		altScreen: fullScreenModes[view.Mode],
	}, nil
}

// validateGeometry refuses a viewport or cursor position the screen model cannot
// address. The transport, not the application, owns the real window size, so a
// generated application cannot claim a viewport outside the bounds.
func (r *Renderer) validateGeometry(size screen.Size, cursor domain.CursorPosition) error {
	if !size.Valid() {
		return domain.NewValidationError(
			domain.CodeInvalidAppView, "view viewport has no cells",
			map[string]string{"width": strconv.Itoa(size.Cols), "height": strconv.Itoa(size.Rows)},
		)
	}
	if size.Cols*size.Rows > r.limits.MaxFrameBytes {
		return domain.NewLimitError(
			domain.CodeInvalidAppView, "view viewport has more cells than the frame limit allows",
			map[string]string{"width": strconv.Itoa(size.Cols), "height": strconv.Itoa(size.Rows)},
		)
	}
	if cursor.Row < 0 || cursor.Col < 0 || cursor.Row >= size.Rows || cursor.Col >= size.Cols {
		return domain.NewValidationError(
			domain.CodeInvalidAppView, "view cursor lies outside its viewport",
			map[string]string{
				"row":    strconv.Itoa(cursor.Row),
				"col":    strconv.Itoa(cursor.Col),
				"width":  strconv.Itoa(size.Cols),
				"height": strconv.Itoa(size.Rows),
			},
		)
	}
	return nil
}
