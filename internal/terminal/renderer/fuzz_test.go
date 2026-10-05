package renderer

import (
	"context"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// FuzzRenderView checks the invariant the renderer exists for: whatever a
// generated application puts in a view, the stored frame contains no terminal
// control sequence that the renderer did not emit itself.
func FuzzRenderView(f *testing.F) {
	f.Add("plain content", "text", 8, 2, false)
	f.Add("\x1b[31mcolour\x1b[0m", "pager", 12, 3, true)
	f.Add("\x1b]52;c;cGF5bG9hZA==\x07", "table", 10, 2, false)
	f.Add("\x1b]0;title\x07\u009b", "status", 6, 4, true)
	f.Add("\x1b]8;;http://evil.test\x07x", "editor", 20, 2, false)
	f.Add("世界 wide é marks", "form", 4, 3, false)

	modes := []string{
		domain.AppViewModeText, domain.AppViewModeForm, domain.AppViewModeTable,
		domain.AppViewModeEditor, domain.AppViewModePager, domain.AppViewModeStatus,
		"not-a-mode", "",
	}

	f.Fuzz(func(t *testing.T, content, mode string, cols, rows int, status bool) {
		if len(content) > 2048 || cols < 1 || cols > 64 || rows < 1 || rows > 32 {
			t.Skip()
		}
		store := newMemoryStore()
		renderer := newTestRenderer(t, store)
		// The mode is derived from the fuzzed string so every declared mode and
		// one invalid mode are all explored.
		mode = modes[len(mode)%len(modes)]
		view := domain.AppView{
			Mode:      mode,
			BufferRef: put(t, store, content),
			Viewport:  domain.Viewport{Width: cols, Height: rows},
			Cursor:    domain.CursorPosition{Row: len(content) % rows, Col: len(mode) % cols},
		}
		if status {
			view.StatusLine = content
		}
		stored := len(store.blobs)
		ref, size, err := renderer.RenderView(context.Background(), testSession(t), view)
		if err != nil {
			// A view the renderer refuses is a correct outcome; what matters is
			// that refusing it stores no frame.
			if len(store.blobs) != stored {
				t.Fatalf("a refused view still stored a frame (%d)", len(store.blobs)-stored)
			}
			return
		}
		frame := store.stored(t, ref)
		if size != int64(len(frame)) {
			t.Fatalf("size = %d, want %d", size, len(frame))
		}
		assertOnlyRendererSequences(t, frame)
	})
}

// FuzzWriteText checks that plain output cannot introduce a control sequence
// either, whatever it contains.
func FuzzWriteText(f *testing.F) {
	f.Add("plain output\n")
	f.Add("\x1b]52;c;cGF5bG9hZA==\x07")
	f.Add("\x1b[2J\x1b[H\u009b")
	f.Add("\x1bP+q544e\x1b\\")

	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 4096 {
			t.Skip()
		}
		store := newMemoryStore()
		renderer := newTestRenderer(t, store)
		size, err := renderer.WriteText(context.Background(), testSession(t), text)
		if err != nil {
			return
		}
		var frame string
		for _, data := range store.blobs {
			frame = string(data)
		}
		if size != int64(len(frame)) {
			t.Fatalf("size = %d, want %d", size, len(frame))
		}
		assertOnlyRendererSequences(t, []byte(frame))
	})
}
