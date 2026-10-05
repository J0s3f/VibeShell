package editor

import (
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/terminal/input"
)

func newTestEditor(t *testing.T) *Editor {
	t.Helper()
	editor, err := New(DefaultLimits())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return editor
}

// press applies the events that typing text produces.
func press(editor *Editor, text string) {
	for _, r := range text {
		editor.Apply(input.KeyEvent(r))
	}
}

// pressKey applies one named key.
func pressKey(editor *Editor, key input.Key) Result {
	return editor.Apply(input.NamedKeyEvent(key))
}

func assertAction(t *testing.T, got Result, want Action) {
	t.Helper()
	if got.Action != want {
		t.Fatalf("action = %s, want %s", got.Action, want)
	}
}

func TestTypingAndSubmitting(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "ls -la")
	if editor.Draft() != "ls -la" {
		t.Fatalf("draft = %q, want ls -la", editor.Draft())
	}
	result := pressKey(editor, input.KeyEnter)
	assertAction(t, result, ActionSubmit)
	if result.Text != "ls -la" {
		t.Fatalf("submitted %q, want ls -la", result.Text)
	}
	if !editor.Empty() {
		t.Fatalf("draft = %q after submitting, want it empty", editor.Draft())
	}
	if got := editor.History().Entries(); len(got) != 1 || got[0] != "ls -la" {
		t.Fatalf("history = %q, want the accepted command", got)
	}
}

func TestSubmittingAnEmptyDraftDoesNothing(t *testing.T) {
	editor := newTestEditor(t)
	assertAction(t, pressKey(editor, input.KeyEnter), ActionNone)
	if editor.History().Len() != 0 {
		t.Fatal("an empty submission must not enter the history")
	}
}

func TestBackspaceRemovesAWholeGrapheme(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "世")
	assertAction(t, pressKey(editor, input.KeyBackspace), ActionModified)
	if !editor.Empty() {
		t.Fatalf("draft = %q, want the wide grapheme removed", editor.Draft())
	}

	press(editor, "e\u0301x")
	// One backspace removes the x.
	pressKey(editor, input.KeyBackspace)
	// The next removes the base rune together with its combining mark.
	pressKey(editor, input.KeyBackspace)
	if editor.Draft() != "" {
		t.Fatalf("draft = %q, want the grapheme removed whole", editor.Draft())
	}
	assertAction(t, pressKey(editor, input.KeyBackspace), ActionNone)
}

func TestDeleteAndEndOfInput(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "abc")
	pressKey(editor, input.KeyLeft)
	assertAction(t, pressKey(editor, input.KeyDelete), ActionModified)
	if editor.Draft() != "ab" {
		t.Fatalf("draft = %q, want ab", editor.Draft())
	}
	assertAction(t, pressKey(editor, input.KeyCtrlD), ActionNone)
	if editor.Draft() != "ab" {
		t.Fatalf("draft = %q, want ab", editor.Draft())
	}
	// With nothing left to erase, Ctrl-D closes the session instead.
	pressKey(editor, input.KeyBackspace)
	pressKey(editor, input.KeyBackspace)
	assertAction(t, pressKey(editor, input.KeyCtrlD), ActionEOF)
	assertAction(t, editor.Apply(input.EOFEvent()), ActionEOF)
}

func TestArrowAndHomeEndMovement(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "abc")
	assertAction(t, pressKey(editor, input.KeyHome), ActionModified)
	if editor.Cursor() != 0 {
		t.Fatalf("cursor = %d after Home, want 0", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyCtrlA), ActionNone)
	assertAction(t, pressKey(editor, input.KeyEnd), ActionModified)
	if editor.Cursor() != 3 {
		t.Fatalf("cursor = %d after End, want 3", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyCtrlE), ActionNone)
	assertAction(t, pressKey(editor, input.KeyCtrlB), ActionModified)
	if editor.Cursor() != 2 {
		t.Fatalf("cursor = %d after ctrl-b, want 2", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyCtrlF), ActionModified)
	if editor.Cursor() != 3 {
		t.Fatalf("cursor = %d after ctrl-f, want 3", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyRight), ActionNone)
	for range 3 {
		assertAction(t, pressKey(editor, input.KeyLeft), ActionModified)
	}
	if editor.Cursor() != 0 {
		t.Fatalf("cursor = %d, want 0", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyLeft), ActionNone)
}

func TestMovementSkipsCombiningMarks(t *testing.T) {
	editor := newTestEditor(t)
	editor.SetDraft("e\u0301x")
	// One press steps over the x, the next over the whole grapheme.
	assertAction(t, pressKey(editor, input.KeyLeft), ActionModified)
	if editor.Cursor() != 2 {
		t.Fatalf("cursor = %d, want 2", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyLeft), ActionModified)
	if editor.Cursor() != 0 {
		t.Fatalf("cursor = %d, want 0: a mark moves with its base rune", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyRight), ActionModified)
	if editor.Cursor() != 2 {
		t.Fatalf("cursor = %d, want 2 after moving right over the grapheme", editor.Cursor())
	}
}

func TestEmacsEraseKeys(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "one two three")
	// Move into the line so the erase keys have something to remove.
	for range 6 {
		pressKey(editor, input.KeyLeft)
	}
	assertAction(t, pressKey(editor, input.KeyCtrlK), ActionModified)
	if editor.Draft() != "one two" {
		t.Fatalf("draft = %q after ctrl-k, want the rest of the line erased", editor.Draft())
	}

	editor.SetDraft("alpha beta")
	pressKey(editor, input.KeyCtrlW)
	if editor.Draft() != "alpha " {
		t.Fatalf("draft = %q after ctrl-w, want the last word erased", editor.Draft())
	}
	editor.SetDraft("alpha   ")
	pressKey(editor, input.KeyCtrlW)
	if editor.Draft() != "" {
		t.Fatalf("draft = %q after ctrl-w over spaces, want the spaces and word erased", editor.Draft())
	}

	editor.SetDraft("alpha beta")
	pressKey(editor, input.KeyEnd)
	assertAction(t, pressKey(editor, input.KeyCtrlU), ActionModified)
	if editor.Draft() != "" {
		t.Fatalf("draft = %q after ctrl-u at the line end, want the line erased", editor.Draft())
	}

	editor.SetDraft("alpha beta")
	editor.SetCursor(5)
	assertAction(t, pressKey(editor, input.KeyCtrlU), ActionModified)
	if editor.Draft() != " beta" {
		t.Fatalf("draft = %q after ctrl-u mid line, want the text before the cursor erased", editor.Draft())
	}
	pressKey(editor, input.KeyCtrlK)
	if editor.Draft() != "" {
		t.Fatalf("draft = %q after ctrl-k, want the rest of the line erased", editor.Draft())
	}
}

func TestHistoryNavigationRestoresTheDraft(t *testing.T) {
	editor := newTestEditor(t)
	for _, line := range []string{"first", "second", "third"} {
		press(editor, line)
		pressKey(editor, input.KeyEnter)
	}
	press(editor, "work in progress")

	assertAction(t, pressKey(editor, input.KeyUp), ActionModified)
	if editor.Draft() != "third" {
		t.Fatalf("draft = %q, want the newest history entry", editor.Draft())
	}
	assertAction(t, pressKey(editor, input.KeyUp), ActionModified)
	if editor.Draft() != "second" {
		t.Fatalf("draft = %q, want the entry before it", editor.Draft())
	}
	assertAction(t, pressKey(editor, input.KeyCtrlP), ActionModified)
	if editor.Draft() != "first" {
		t.Fatalf("draft = %q after ctrl-p, want the oldest entry", editor.Draft())
	}
	assertAction(t, pressKey(editor, input.KeyUp), ActionNone)
	assertAction(t, pressKey(editor, input.KeyCtrlN), ActionModified)
	assertAction(t, pressKey(editor, input.KeyDown), ActionModified)
	assertAction(t, pressKey(editor, input.KeyDown), ActionModified)
	if editor.Draft() != "work in progress" {
		t.Fatalf("draft = %q, want the draft in progress restored", editor.Draft())
	}
	assertAction(t, pressKey(editor, input.KeyDown), ActionNone)
}

func TestEditingAfterRecallEndsHistoryNavigation(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "old")
	pressKey(editor, input.KeyEnter)
	press(editor, "half")
	pressKey(editor, input.KeyUp)
	press(editor, "x")
	assertAction(t, pressKey(editor, input.KeyDown), ActionNone)
	if editor.Draft() != "oldx" {
		t.Fatalf("draft = %q, want the edit kept and navigation ended", editor.Draft())
	}
}

func TestHistorySkipsBlankAndRepeatedLines(t *testing.T) {
	history := NewHistory(4)
	history.Append("same")
	history.Append("same")
	history.Append("   ")
	history.Append("")
	history.Append("other")
	if got := history.Entries(); len(got) != 2 || got[0] != "same" || got[1] != "other" {
		t.Fatalf("history = %q, want the repeated and blank lines refused", got)
	}
	for _, line := range []string{"a", "b", "c", "d"} {
		history.Append(line)
	}
	if got := history.Entries(); len(got) != 4 || got[0] != "a" {
		t.Fatalf("history = %q, want the four newest lines", got)
	}
	history.Clear()
	if history.Len() != 0 {
		t.Fatal("Clear left entries behind")
	}
	if _, ok := history.At(0); ok {
		t.Fatal("At returned an entry from an empty history")
	}
}

func TestBracketedMultilinePaste(t *testing.T) {
	editor := newTestEditor(t)
	result := editor.Apply(input.Event{Kind: input.KindPaste, Text: "echo one\necho two\ttabbed"})
	if result.Inserted != len("echo one\necho two\ttabbed") {
		t.Fatalf("inserted %d runes, want the whole paste", result.Inserted)
	}
	if editor.Draft() != "echo one\necho two\ttabbed" {
		t.Fatalf("draft = %q, want the paste with its newlines", editor.Draft())
	}
	if !editor.Multiline() || editor.LineCount() != 2 {
		t.Fatalf("multiline = %t with %d lines, want two", editor.Multiline(), editor.LineCount())
	}
}

func TestUpAndDownMoveLinesInAMultilineDraft(t *testing.T) {
	editor := newTestEditor(t)
	editor.SetDraft("first line\nsecond line")
	assertAction(t, pressKey(editor, input.KeyUp), ActionModified)
	if editor.Cursor() != len("first line") {
		t.Fatalf("cursor = %d, want the end of the first line", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyUp), ActionNone)
	assertAction(t, pressKey(editor, input.KeyDown), ActionModified)
	// The column is kept, so the cursor lands in the same place on the longer
	// line rather than at its end.
	if editor.Cursor() != 21 {
		t.Fatalf("cursor = %d, want 21", editor.Cursor())
	}
	assertAction(t, pressKey(editor, input.KeyDown), ActionNone)

	// The column is kept within the target line.
	editor.SetDraft("long first line\nab")
	editor.SetCursor(4)
	pressKey(editor, input.KeyDown)
	if editor.Cursor() != len("long first line\nab") {
		t.Fatalf("cursor = %d on a shorter line, want it clamped to the line end", editor.Cursor())
	}
	// Home and End work on the line the cursor is on.
	editor.SetDraft("first line\nsecond line")
	editor.SetCursor(12)
	pressKey(editor, input.KeyHome)
	if editor.Cursor() != 11 {
		t.Fatalf("cursor = %d after Home on the second line, want 11", editor.Cursor())
	}
	pressKey(editor, input.KeyEnd)
	if editor.Cursor() != len("first line\nsecond line") {
		t.Fatalf("cursor = %d after End, want the end of the line", editor.Cursor())
	}
}

func TestEnterInsertsANewlineWhenTheInteractionNeedsOne(t *testing.T) {
	editor := newTestEditor(t)
	editor.SetEnterBehavior(EnterInsertsNewline)
	press(editor, "one")
	assertAction(t, pressKey(editor, input.KeyEnter), ActionModified)
	press(editor, "two")
	if editor.Draft() != "one\ntwo" {
		t.Fatalf("draft = %q, want a newline between the two lines", editor.Draft())
	}
	if editor.History().Len() != 0 {
		t.Fatal("inserting a newline must not record history")
	}
}

func TestControlBytesNeverEnterTheDraft(t *testing.T) {
	editor := newTestEditor(t)
	result := editor.Apply(input.Event{Kind: input.KindPaste, Text: "a\x1b[31mb\x1b]0;title\x07c\u009bd\x00e\x7ff"})
	if editor.Draft() != "a[31mb]0;titlecdef" {
		t.Fatalf("draft = %q, want the control bytes removed", editor.Draft())
	}
	if result.Truncated {
		t.Fatal("shortening the draft is not truncation")
	}
	// A key event carrying a control rune is not input either.
	assertAction(t, editor.Apply(input.KeyEvent('\x1b')), ActionUnhandled)
	if strings.ContainsRune(editor.Draft(), 0x1b) {
		t.Fatal("an escape byte entered the draft")
	}
}

func TestControlCCancelsAndEscapeDropsACompletionCycle(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "ls")
	editor.ApplyCompletion([]string{"lsof", "lsblk"})
	pressKey(editor, input.KeyCtrlC)
	if !editor.Empty() {
		t.Fatalf("draft = %q after ctrl-c, want it empty", editor.Draft())
	}
	if editor.History().Len() != 0 {
		t.Fatal("a cancelled draft must not enter the history")
	}

	press(editor, "l")
	editor.ApplyCompletion([]string{"lsof", "lsblk"})
	before := editor.Draft()
	assertAction(t, pressKey(editor, input.KeyEscape), ActionModified)
	if editor.Draft() != before {
		t.Fatalf("draft = %q after escape, want the completion cycle dropped", editor.Draft())
	}
}

func TestTabAsksForCompletionInsteadOfGuessing(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "cd /et")
	assertAction(t, pressKey(editor, input.KeyTab), ActionRequestCompletion)
	if editor.Draft() != "cd /et" {
		t.Fatalf("draft = %q, want it unchanged until candidates arrive", editor.Draft())
	}
}

func TestApplyCompletionInsertsTheCommonPrefix(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "cd /sr")
	// The candidates share only the text already typed, so nothing is inserted
	// and the cycle is armed for the next Tab.
	result := editor.ApplyCompletion([]string{"/src", "/srv", "/etc"})
	assertAction(t, result, ActionNone)
	if editor.Draft() != "cd /sr" {
		t.Fatalf("draft = %q, want the shared prefix only", editor.Draft())
	}
	assertAction(t, editor.ApplyCompletion([]string{"/src", "/srv", "/etc"}), ActionModified)
	if editor.Draft() != "cd /src" {
		t.Fatalf("draft = %q, want the first candidate", editor.Draft())
	}
	assertAction(t, editor.ApplyCompletion([]string{"/src", "/srv", "/etc"}), ActionModified)
	if editor.Draft() != "cd /srv" {
		t.Fatalf("draft = %q, want the second candidate", editor.Draft())
	}
	assertAction(t, editor.ApplyCompletion([]string{"/src", "/srv", "/etc"}), ActionModified)
	if editor.Draft() != "cd /src" {
		t.Fatalf("draft = %q, want the cycle to wrap around", editor.Draft())
	}
	// No candidates ends the cycle and keeps the draft as it is.
	assertAction(t, editor.ApplyCompletion(nil), ActionNone)
	if editor.Draft() != "cd /src" {
		t.Fatalf("draft = %q, want it kept when no candidates arrive", editor.Draft())
	}
	// A later completion starts a new word under the cursor.
	editor.SetDraft("cd /s")
	assertAction(t, editor.ApplyCompletion([]string{"/src", "/srcx"}), ActionModified)
	if editor.Draft() != "cd /src" {
		t.Fatalf("draft = %q, want the shared prefix inserted", editor.Draft())
	}
	assertAction(t, editor.ApplyCompletion([]string{"/src", "/srcx"}), ActionModified)
	if editor.Draft() != "cd /srcx" {
		t.Fatalf("draft = %q, want the candidate that differs from the prefix", editor.Draft())
	}
}

func TestApplyCompletionFinishesASingleCandidate(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "cat READ")
	editor.ApplyCompletion([]string{"README.md", "notes.txt"})
	if editor.Draft() != "cat README.md" {
		t.Fatalf("draft = %q, want the only candidate completed", editor.Draft())
	}
	// The word is already complete, so completing again changes nothing.
	assertAction(t, editor.ApplyCompletion([]string{"README.md"}), ActionNone)
	if editor.Draft() != "cat README.md" {
		t.Fatalf("draft = %q, want it unchanged", editor.Draft())
	}
}

func TestApplyCompletionIgnoresCandidatesThatDoNotMatch(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "cd /zzz")
	assertAction(t, editor.ApplyCompletion([]string{"/src", "/srv"}), ActionNone)
	if editor.Draft() != "cd /zzz" {
		t.Fatalf("draft = %q, want it unchanged", editor.Draft())
	}
}

func TestShowedCompletionDoesNotRun(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "rm ")
	editor.ApplyCompletion([]string{"rm -rf /", "rm -i"})
	if editor.History().Len() != 0 {
		t.Fatal("completing must not record a command")
	}
	if action := pressKey(editor, input.KeyEnter).Action; action != ActionSubmit {
		t.Fatalf("action = %s, want the draft to stay text until the user submits it", action)
	}
}

func TestInsertionStopsAtTheDraftLimits(t *testing.T) {
	editor, err := New(Limits{MaxRunes: 8, MaxLines: 2, MaxHistory: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := editor.Apply(input.Event{Kind: input.KindPaste, Text: "aaa\nbbb\nccc"})
	if !result.Truncated {
		t.Fatal("a paste past the limits must report truncation")
	}
	// The line limit refuses the third line and the rune limit the rest, and
	// whatever fit before them is kept.
	if got := editor.Draft(); got != "aaa\nbbbc" {
		t.Fatalf("draft = %q, want the part that fits", got)
	}
	if result.Inserted != 8 {
		t.Fatalf("inserted %d runes, want the eight that fit", result.Inserted)
	}
}

func TestNewRejectsUnboundedLimits(t *testing.T) {
	for name, limits := range map[string]Limits{
		"runes":   {MaxRunes: 0, MaxLines: 2, MaxHistory: 2},
		"lines":   {MaxRunes: 2, MaxLines: -1, MaxHistory: 2},
		"history": {MaxRunes: 2, MaxLines: 2, MaxHistory: 0},
	} {
		if _, err := New(limits); err == nil {
			t.Fatalf("New accepted the %s limit %+v", name, limits)
		}
	}
}

func TestUnhandledKeysLeaveTheDraftAlone(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "ab")
	for _, key := range []input.Key{input.KeyPageUp, input.KeyF1, input.KeyInsert, input.KeyShiftTab} {
		result := pressKey(editor, key)
		if result.Action != ActionUnhandled {
			t.Fatalf("%s produced %s, want ActionUnhandled", key, result.Action)
		}
		if editor.Draft() != "ab" {
			t.Fatalf("draft = %q after %s, want it unchanged", editor.Draft(), key)
		}
	}
	assertAction(t, pressKey(editor, input.KeyCtrlL), ActionClearScreen)
	if editor.Draft() != "ab" {
		t.Fatalf("draft = %q after ctrl-l, want it unchanged", editor.Draft())
	}
}

func TestResizeAndRejectedInputChangeNothing(t *testing.T) {
	editor := newTestEditor(t)
	press(editor, "ab")
	assertAction(t, editor.Apply(input.ResizeEvent(120, 40)), ActionNone)
	assertAction(t, editor.Apply(input.RejectedEvent([]byte("\x1b[c"))), ActionNone)
	if editor.Draft() != "ab" {
		t.Fatalf("draft = %q, want it unchanged", editor.Draft())
	}
}

func TestActionNamesAreDistinct(t *testing.T) {
	actions := []Action{
		ActionNone, ActionModified, ActionSubmit, ActionCancel, ActionEOF,
		ActionRequestCompletion, ActionUnhandled, ActionClearScreen,
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if seen[action.String()] {
			t.Fatalf("action name %q is used twice", action)
		}
		seen[action.String()] = true
	}
}
