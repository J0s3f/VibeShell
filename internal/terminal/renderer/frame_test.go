package renderer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/terminal/screen"
)

func TestRenderViewStoresTheFrameAsContent(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, "hello world"),
		Viewport:  domain.Viewport{Top: 0, Left: 0, Width: 20, Height: 3},
	}
	ref, size, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	if size != int64(len(store.stored(t, ref))) {
		t.Fatalf("size = %d, want the stored byte count", size)
	}
	if ref.Size != size {
		t.Fatalf("reference size = %d, want %d", ref.Size, size)
	}
	if store.mediaType != FrameMediaType {
		t.Fatalf("media type = %q, want %q", store.mediaType, FrameMediaType)
	}
	frame := string(store.stored(t, ref))
	if !strings.Contains(frame, "hello world") {
		t.Fatalf("frame = %q, want the buffer content", frame)
	}
	// Every row is positioned explicitly, so the frame never depends on the
	// client wrapping or on what it had on screen before.
	if got := strings.Count(frame, screen.CursorHome); got != 1 {
		t.Fatalf("frame holds %d home sequences, want 1", got)
	}
	if !strings.HasSuffix(frame, screen.ShowCursor) {
		t.Fatalf("frame = %q, want it to end with the cursor being shown", frame)
	}
	assertOnlyRendererSequences(t, store.stored(t, ref))
}

func TestRenderViewPositionsTheCursorWhereTheViewAsks(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, "first\nsecond"),
		Viewport:  domain.Viewport{Width: 10, Height: 4},
		Cursor:    domain.CursorPosition{Row: 1, Col: 3},
	}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := string(store.stored(t, ref))
	if !strings.HasSuffix(frame, "\x1b[2;4H"+screen.ShowCursor) {
		t.Fatalf("frame = %q, want the cursor placed at the view position", frame)
	}
}

func TestRenderViewLeavesTheCursorAfterTheContentWithoutOne(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, "only line"),
		Viewport:  domain.Viewport{Width: 20, Height: 4},
	}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := string(store.stored(t, ref))
	// The trailing state is where the session's prompt continues.
	if !strings.HasSuffix(frame, "\x1b[4;1H"+screen.ShowCursor) {
		t.Fatalf("frame = %q, want the cursor left on the row after the content", frame)
	}
}

func TestRenderViewUsesTheDefaultViewportWhenTheViewDeclaresNone(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{Mode: domain.AppViewModeStatus, BufferRef: put(t, store, "running")}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := string(store.stored(t, ref))
	if !strings.Contains(frame, "\x1b[24;1H") {
		t.Fatalf("frame = %q, want the default 24 row viewport", frame)
	}
}

func TestFullScreenModesTakeTheAlternateScreen(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	for _, mode := range []string{
		domain.AppViewModeForm, domain.AppViewModeTable, domain.AppViewModeEditor,
		domain.AppViewModePager, domain.AppViewModeStatus,
	} {
		view := domain.AppView{
			Mode:      mode,
			BufferRef: put(t, store, "content"),
			Viewport:  domain.Viewport{Width: 10, Height: 2},
		}
		ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
		if err != nil {
			t.Fatalf("RenderView(%s): %v", mode, err)
		}
		frame := string(store.stored(t, ref))
		if !strings.HasPrefix(frame, screen.EnterAlternateScreen) {
			t.Fatalf("frame for %s = %q, want it to open the alternate screen", mode, frame)
		}
		assertOnlyRendererSequences(t, store.stored(t, ref))
	}
}

func TestTextModeReleasesTheAlternateScreen(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, "done"),
		Viewport:  domain.Viewport{Width: 10, Height: 2},
	}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := string(store.stored(t, ref))
	if !strings.HasPrefix(frame, screen.LeaveAlternateScreen) {
		t.Fatalf("frame = %q, want it to hand the terminal back", frame)
	}
}

func TestStatusLineIsStyledAndSanitized(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:       domain.AppViewModePager,
		BufferRef:  put(t, store, "body"),
		Viewport:   domain.Viewport{Width: 20, Height: 2},
		StatusLine: "\x1b[2J\x1b]0;title\x07ready",
	}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := string(store.stored(t, ref))
	if !strings.Contains(frame, "\x1b[7m") {
		t.Fatalf("frame = %q, want the status line in reverse video", frame)
	}
	if !strings.Contains(frame, "[2J]0;titleready") {
		t.Fatalf("frame = %q, want the status text without its control bytes", frame)
	}
	assertOnlyRendererSequences(t, store.stored(t, ref))
}

func TestDataTextCannotActivateTerminalControls(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	hostile := strings.Join([]string{
		"\x1b[31mcolour\x1b[0m",
		"\x1b]52;c;cGF5bG9hZA==\x07",                // clipboard write
		"\x1b]8;;http://evil.test\x07x\x1b]8;;\x07", // hyperlink
		"\x1b]0;title\x07",                          // title
		"\x1bP+q544e\x1b\\",                         // device control
		"\x1b[2J\x1b[H\x1b[3J",                      // screen clearing
		"\u009b31m\u009dtitle\u009c",                // the C1 forms
		"plain text",
	}, "")
	view := domain.AppView{
		Mode:       domain.AppViewModeText,
		BufferRef:  put(t, store, hostile),
		Viewport:   domain.Viewport{Width: 60, Height: 4},
		StatusLine: "\x1b]2;status\x07status",
	}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := store.stored(t, ref)
	assertOnlyRendererSequences(t, frame)
	text := string(frame)
	for _, forbidden := range []string{
		"]52", "]8", "]0;", "]2;", "\x1bP", "\x1b\\", "cGF5bG9hZA==",
	} {
		// The bytes of a sequence are visible as text, but the introducer that
		// would make a terminal act on them must be gone.
		if strings.Contains(text, forbidden+"\x07") || strings.Contains(text, forbidden+"\x1b") {
			t.Fatalf("frame still carries the sequence %q", forbidden)
		}
	}
}

func TestRenderViewRejectsAViewTheDomainRefuses(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{Mode: "terminal-takeover"}
	_, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if !domain.IsValidationError(err) || domain.GetErrorCode(err) != domain.CodeInvalidAppView {
		t.Fatalf("error = %v, want an invalid app view", err)
	}
	if len(store.blobs) != 0 {
		t.Fatal("a view that fails validation must not be stored")
	}
}

func TestRenderViewRejectsAnUnknownPrimitiveAction(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:     domain.AppViewModeEditor,
		Viewport: domain.Viewport{Width: 10, Height: 2},
		KeyBindings: []domain.KeyBinding{
			{Key: "ctrl-x", Action: "fork_bomb"},
		},
	}
	if _, _, err := renderer.RenderView(context.Background(), testSession(t), view); !domain.IsValidationError(err) {
		t.Fatalf("error = %v, want an invalid app view", err)
	}
}

func TestRenderViewRefusesGeometryOutsideItsBounds(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	tests := map[string]domain.AppView{
		"empty viewport": {
			Mode:     domain.AppViewModeText,
			Viewport: domain.Viewport{Width: -4, Height: 2},
		},
		"zero width only": {
			Mode:     domain.AppViewModeText,
			Viewport: domain.Viewport{Width: 0, Height: 2},
		},
		"cursor below the viewport": {
			Mode:     domain.AppViewModeText,
			Viewport: domain.Viewport{Width: 10, Height: 2},
			Cursor:   domain.CursorPosition{Row: 5, Col: 0},
		},
		"cursor right of the viewport": {
			Mode:     domain.AppViewModeText,
			Viewport: domain.Viewport{Width: 10, Height: 2},
			Cursor:   domain.CursorPosition{Row: 0, Col: 10},
		},
		"negative cursor": {
			Mode:     domain.AppViewModeText,
			Viewport: domain.Viewport{Width: 10, Height: 2},
			Cursor:   domain.CursorPosition{Row: -1, Col: 0},
		},
	}
	for name, view := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := renderer.RenderView(context.Background(), testSession(t), view); !domain.IsValidationError(err) {
				t.Fatalf("error = %v, want an invalid app view", err)
			}
		})
	}
}

func TestRenderViewRefusesOversizedDeclarations(t *testing.T) {
	store := newMemoryStore()
	limits := DefaultLimits()
	limits.MaxStatusBytes = 8
	limits.MaxKeyBindings = 1
	limits.MaxMetadataBytes = 8
	renderer, err := New(store, limits)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := map[string]domain.AppView{
		"long status line": {
			Mode: domain.AppViewModePager, Viewport: domain.Viewport{Width: 10, Height: 2},
			StatusLine: "a status line that is far too long",
		},
		"too many key bindings": {
			Mode: domain.AppViewModeEditor, Viewport: domain.Viewport{Width: 10, Height: 2},
			KeyBindings: []domain.KeyBinding{
				{Key: "ctrl-a", Action: domain.ActionMoveCursor},
				{Key: "ctrl-b", Action: domain.ActionMoveCursor},
			},
		},
		"key binding without a key": {
			Mode: domain.AppViewModeEditor, Viewport: domain.Viewport{Width: 10, Height: 2},
			KeyBindings: []domain.KeyBinding{
				{Action: domain.ActionMoveCursor},
			},
		},
		"large metadata": {
			Mode: domain.AppViewModeTable, Viewport: domain.Viewport{Width: 10, Height: 2},
			Metadata: []byte(`{"rows":[[1,2,3],[4,5,6]]}`),
		},
	}
	for name, view := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := renderer.RenderView(context.Background(), testSession(t), view)
			if !domain.IsLimitError(err) && !domain.IsValidationError(err) {
				t.Fatalf("error = %v, want the declaration refused", err)
			}
		})
	}
}

func TestRenderViewRefusesABufferLargerThanTheLimit(t *testing.T) {
	store := newMemoryStore()
	limits := DefaultLimits()
	limits.MaxBufferBytes = 8
	renderer, err := New(store, limits)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, "far more than eight bytes"),
		Viewport:  domain.Viewport{Width: 10, Height: 2},
	}
	_, _, err = renderer.RenderView(context.Background(), testSession(t), view)
	if !domain.IsLimitError(err) || domain.GetErrorCode(err) != domain.CodeOutputTooLarge {
		t.Fatalf("error = %v, want an output-too-large limit error", err)
	}
	if store.getCalls != 0 {
		t.Fatal("an oversized buffer must be refused before it is read")
	}
}

func TestRenderViewRefusesAFrameLargerThanTheLimit(t *testing.T) {
	store := newMemoryStore()
	limits := DefaultLimits()
	limits.MaxFrameBytes = 64
	renderer, err := New(store, limits)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, strings.Repeat("x", 40)),
		Viewport:  domain.Viewport{Width: 20, Height: 3},
	}
	_, _, err = renderer.RenderView(context.Background(), testSession(t), view)
	if !domain.IsLimitError(err) {
		t.Fatalf("error = %v, want a limit error", err)
	}
}

func TestRenderViewReportsAContentStoreFailure(t *testing.T) {
	store := newMemoryStore()
	store.putErr = context.DeadlineExceeded
	renderer := newTestRenderer(t, store)
	view := domain.AppView{Mode: domain.AppViewModeText, Viewport: domain.Viewport{Width: 10, Height: 2}}
	_, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the store failure passed through", err)
	}
}

func TestRenderViewDetectsAWrongReportedSize(t *testing.T) {
	store := newMemoryStore()
	store.sizeDelta = 7
	renderer := newTestRenderer(t, store)
	view := domain.AppView{Mode: domain.AppViewModeText, Viewport: domain.Viewport{Width: 10, Height: 2}}
	_, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if !domain.IsInternalError(err) || domain.GetErrorCode(err) != domain.CodeInvariantViolation {
		t.Fatalf("error = %v, want an invariant violation", err)
	}
}

func TestRenderViewPropagatesABufferReadFailure(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, "content"),
		Viewport:  domain.Viewport{Width: 10, Height: 2},
	}
	store.getErr = context.Canceled
	if _, _, err := renderer.RenderView(context.Background(), testSession(t), view); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the read failure passed through", err)
	}
}

func TestRenderViewShowsWideAndCombiningContent(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:      domain.AppViewModeText,
		BufferRef: put(t, store, "e\u0301 世 tab:\there"),
		Viewport:  domain.Viewport{Width: 20, Height: 2},
	}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := string(store.stored(t, ref))
	if !strings.Contains(frame, "e\u0301 世 tab:") {
		t.Fatalf("frame = %q, want the wide grapheme and the mark kept", frame)
	}
	if !strings.Contains(frame, "here") {
		t.Fatalf("frame = %q, want the text after the tab", frame)
	}
	assertOnlyRendererSequences(t, store.stored(t, ref))
}

func TestRenderViewHidesTheCursorWhenTheScreenDoes(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	view := domain.AppView{
		Mode:      domain.AppViewModePager,
		BufferRef: put(t, store, "scrolling"),
		Viewport:  domain.Viewport{Width: 10, Height: 2},
	}
	ref, _, err := renderer.RenderView(context.Background(), testSession(t), view)
	if err != nil {
		t.Fatalf("RenderView: %v", err)
	}
	frame := string(store.stored(t, ref))
	if !strings.HasSuffix(frame, screen.ShowCursor) {
		t.Fatalf("frame = %q, want the cursor shown by default", frame)
	}
	assertOnlyRendererSequences(t, store.stored(t, ref))
}

func TestWriteTextSanitizesAndEndsTheLine(t *testing.T) {
	store := newMemoryStore()
	renderer := newTestRenderer(t, store)
	size, err := renderer.WriteText(context.Background(), testSession(t), "plain \x1b]52;c;x\x07text\nmore")
	if err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	var frame string
	for _, data := range store.blobs {
		frame = string(data)
	}
	if !strings.HasPrefix(frame, screen.ResetStyle) {
		t.Fatalf("frame = %q, want the style reset first", frame)
	}
	if !strings.HasSuffix(frame, "\r\n") {
		t.Fatalf("frame = %q, want the output to end on a fresh line", frame)
	}
	if !strings.Contains(frame, "plain ]52;c;xtext\r\nmore") {
		t.Fatalf("frame = %q, want the text with its control bytes removed and embedded newlines normalized", frame)
	}
	if size != int64(len(frame)) {
		t.Fatalf("size = %d, want %d", size, len(frame))
	}
	assertOnlyRendererSequences(t, []byte(frame))
}

func TestWriteTextRefusesOversizedText(t *testing.T) {
	store := newMemoryStore()
	limits := DefaultLimits()
	limits.MaxBufferBytes = 8
	renderer, err := New(store, limits)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := renderer.WriteText(context.Background(), testSession(t), "far too long"); !domain.IsLimitError(err) {
		t.Fatalf("error = %v, want a limit error", err)
	}
	if len(store.blobs) != 0 {
		t.Fatal("oversized text must not be stored")
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	if _, err := New(nil, DefaultLimits()); !errors.Is(err, ErrInvalidLimits) {
		t.Fatal("a renderer without a content store must be refused")
	}
	limits := DefaultLimits()
	limits.MaxFrameBytes = 0
	if _, err := New(newMemoryStore(), limits); !errors.Is(err, ErrInvalidLimits) {
		t.Fatal("a renderer without a frame limit must be refused")
	}
	limits = DefaultLimits()
	limits.DefaultSize = screen.Size{}
	if _, err := New(newMemoryStore(), limits); !errors.Is(err, ErrInvalidLimits) {
		t.Fatal("a renderer without a default viewport must be refused")
	}
}
