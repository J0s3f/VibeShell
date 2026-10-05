package editor

import (
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/terminal/input"
)

func TestPainterRepaintsThePromptRegion(t *testing.T) {
	editor := newTestEditor(t)
	painter := NewPainter("vibeshell:~$ ")
	press(editor, "ls")

	frame := string(painter.Redraw(editor, 40))
	want := "\r\x1b[2Kvibeshell:~$ ls\x1b[16G"
	if frame != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
	if painter.Rows() != 1 {
		t.Fatalf("rows = %d, want 1", painter.Rows())
	}
}

func TestPainterErasesRowsAShorterDraftNoLongerNeeds(t *testing.T) {
	editor := newTestEditor(t)
	painter := NewPainter("$ ")
	editor.SetDraft("first\nsecond\nthird")
	painter.Redraw(editor, 20)
	if painter.Rows() != 3 {
		t.Fatalf("rows = %d, want the three lines of the draft", painter.Rows())
	}

	// Shrinking the draft to one line erases the two rows below it and comes
	// back, so no fragment of the longer prompt is left behind.
	editor.SetDraft("only")
	frame := string(painter.Redraw(editor, 20))
	want := "\x1b[2A\r\x1b[2K$ only\x1b[1B\r\x1b[2K\x1b[1B\r\x1b[2K\x1b[2A\x1b[7G"
	if frame != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
	if painter.Rows() != 1 {
		t.Fatalf("rows = %d after shrinking, want 1", painter.Rows())
	}
}

func TestPainterGrowsToMoreRowsWithoutScrolling(t *testing.T) {
	editor := newTestEditor(t)
	painter := NewPainter("$ ")
	editor.SetDraft("one")
	painter.Redraw(editor, 20)

	editor.SetDraft("one\ntwo")
	frame := string(painter.Redraw(editor, 20))
	// The cursor is still on the only prompt row, so nothing moves up and the
	// new row is written below with a cursor-down, which cannot scroll.
	want := "\r\x1b[2K$ one\x1b[1B\r\x1b[2Ktwo\x1b[4G"
	if frame != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
	if painter.Rows() != 2 {
		t.Fatalf("rows = %d, want 2", painter.Rows())
	}
}

func TestPainterPlacesTheCursorAfterAWideGrapheme(t *testing.T) {
	editor := newTestEditor(t)
	painter := NewPainter("$ ")
	editor.SetDraft("世")
	frame := string(painter.Redraw(editor, 20))
	want := "\r\x1b[2K$ 世\x1b[5G"
	if frame != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
}

func TestPainterEmitsOnlyItsOwnSequences(t *testing.T) {
	editor := newTestEditor(t)
	painter := NewPainter("> ")
	editor.Apply(input.Event{Kind: input.KindPaste, Text: "\x1b[2J\x1b]0;t\x07text"})
	frame := string(painter.Redraw(editor, 30))
	if count := strings.Count(frame, "\x1b[2K"); count != 1 {
		t.Fatalf("frame = %q holds %d erase sequences, want only its own", frame, count)
	}
	if strings.ContainsAny(frame, "\x07\x9b") {
		t.Fatalf("frame = %q, want no bell or control-sequence byte", frame)
	}
	// The pasted bytes survive as text, which is what a user pasting a sequence
	// expects to see.
	if !strings.Contains(frame, "[2J]0;ttext") {
		t.Fatalf("frame = %q, want the pasted text shown as text", frame)
	}
}
