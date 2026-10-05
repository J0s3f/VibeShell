package editor

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/terminal/input"
	"j0s.at/vibeshell/internal/terminal/screen"
)

// Limits bounds one editor's draft and history. Every field is required to be
// positive: an unbounded command line would let one paste grow a session's
// memory without limit.
type Limits struct {
	// MaxRunes is the largest draft, in runes, including newlines.
	MaxRunes int
	// MaxLines is the largest number of lines a draft may span, which is what
	// keeps a pasted file from becoming a wall of prompt.
	MaxLines int
	// MaxHistory is how many accepted lines the history keeps.
	MaxHistory int
}

// DefaultLimits returns bounds sized for an interactive shell: a 64 KiB draft,
// 256 lines, and a thousand remembered commands.
func DefaultLimits() Limits {
	return Limits{
		MaxRunes:   64 << 10,
		MaxLines:   256,
		MaxHistory: DefaultHistoryLimit,
	}
}

// ErrInvalidLimits reports an editor limit that is not positive.
var ErrInvalidLimits = errors.New("invalid editor limits")

// validate reports whether every bound is usable.
func (l Limits) validate() error {
	for name, value := range map[string]int{
		"MaxRunes":   l.MaxRunes,
		"MaxLines":   l.MaxLines,
		"MaxHistory": l.MaxHistory,
	} {
		if value <= 0 {
			return fmt.Errorf("%w: %s must be positive, got %d", ErrInvalidLimits, name, value)
		}
	}
	return nil
}

// EnterBehavior decides what Enter does. A shell submits the draft, while a
// multiline interaction inserts a newline instead.
type EnterBehavior int

const (
	// EnterSubmits accepts the draft as input.
	EnterSubmits EnterBehavior = iota
	// EnterInsertsNewline puts a newline into the draft.
	EnterInsertsNewline
)

// completion is the state of a completion in progress: where the word being
// completed starts and the candidates that matched it, so repeated Tab presses
// cycle through them instead of recomputing against a draft that has already
// been changed.
type completion struct {
	wordStart  int
	candidates []string
	next       int
}

// Editor holds the draft being edited. It is not safe for concurrent use; one
// session drives one editor.
type Editor struct {
	limits  Limits
	history *History
	draft   []rune
	cursor  int
	enter   EnterBehavior

	// pending is the draft saved when history navigation starts, so returning
	// past the newest entry restores what the user was typing.
	pending    []rune
	historyPos int
	navigating bool

	completion *completion
}

// New returns an empty editor with the given bounds.
func New(limits Limits) (*Editor, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &Editor{limits: limits, history: NewHistory(limits.MaxHistory)}, nil
}

// History returns the editor's history.
func (e *Editor) History() *History { return e.history }

// SetEnterBehavior chooses whether Enter submits the draft or inserts a
// newline.
func (e *Editor) SetEnterBehavior(behavior EnterBehavior) { e.enter = behavior }

// Draft returns the current text.
func (e *Editor) Draft() string { return string(e.draft) }

// Empty reports whether there is nothing to submit.
func (e *Editor) Empty() bool { return len(e.draft) == 0 }

// Multiline reports whether the draft spans more than one line.
func (e *Editor) Multiline() bool { return strings.ContainsRune(e.Draft(), '\n') }

// LineCount returns how many lines the draft spans.
func (e *Editor) LineCount() int { return strings.Count(e.Draft(), "\n") + 1 }

// Cursor returns the cursor position as a rune index into the draft.
func (e *Editor) Cursor() int { return e.cursor }

// Apply acts on one decoded input event. End of input and resize change no
// text, and bytes the decoder refused are not input.
func (e *Editor) Apply(event input.Event) Result {
	switch event.Kind {
	case input.KindPaste:
		return e.InsertString(event.Text)
	case input.KindEOF:
		return Result{Action: ActionEOF}
	case input.KindResize, input.KindRejected:
		return Result{Action: ActionNone}
	case input.KindKey:
		return e.applyKey(event)
	default:
		return Result{Action: ActionUnhandled}
	}
}

// InsertString adds text to the draft at the cursor. A bracketed multiline
// paste arrives here with its newlines intact, so one paste can produce a
// multiline draft.
func (e *Editor) InsertString(text string) Result {
	runes := make([]rune, 0, utf8.RuneCountInString(text))
	for _, r := range text {
		if editableRune(r) {
			runes = append(runes, r)
		}
	}
	inserted, truncated := e.insert(runes)
	if inserted == 0 {
		return Result{Action: ActionNone, Truncated: truncated}
	}
	e.resetNavigation()
	return Result{Action: ActionModified, Inserted: inserted, Truncated: truncated}
}

func (e *Editor) applyKey(event input.Event) Result {
	switch key := event.Key; key {
	case input.KeyRune:
		if !editableRune(event.Rune) {
			return Result{Action: ActionUnhandled}
		}
		inserted, truncated := e.insert([]rune{event.Rune})
		if inserted == 0 {
			return Result{Action: ActionNone, Truncated: truncated}
		}
		e.resetNavigation()
		return Result{Action: ActionModified, Inserted: inserted, Truncated: truncated}

	case input.KeyEnter:
		return e.enterPressed()

	case input.KeyTab:
		// Completion needs committed facts, which only the session has, so the
		// editor asks for them instead of guessing.
		return Result{Action: ActionRequestCompletion}

	case input.KeyBackspace, input.KeyCtrlH:
		return e.eraseBackward()

	case input.KeyDelete:
		return e.eraseForward()

	case input.KeyCtrlD:
		if e.Empty() {
			// On an empty prompt the end of input closes the session; on a
			// nonempty one it follows the local editor contract and deletes.
			return Result{Action: ActionEOF}
		}
		return e.eraseForward()

	case input.KeyLeft, input.KeyCtrlB:
		return e.moveHorizontal(-1)

	case input.KeyRight, input.KeyCtrlF:
		return e.moveHorizontal(1)

	case input.KeyUp:
		if e.Multiline() {
			return e.moveVertical(-1)
		}
		return e.historyPrevious()

	case input.KeyDown:
		if e.Multiline() {
			return e.moveVertical(1)
		}
		return e.historyNext()

	case input.KeyCtrlP:
		return e.historyPrevious()

	case input.KeyCtrlN:
		return e.historyNext()

	case input.KeyHome, input.KeyCtrlA:
		return e.moveTo(e.lineStart())

	case input.KeyEnd, input.KeyCtrlE:
		return e.moveTo(e.lineEnd())

	case input.KeyCtrlK:
		return e.eraseRange(e.cursor, e.lineEnd())

	case input.KeyCtrlU:
		return e.eraseRange(e.lineStart(), e.cursor)

	case input.KeyCtrlW:
		return e.eraseWordBackward()

	case input.KeyCtrlC:
		e.Cancel()
		return Result{Action: ActionCancel}

	case input.KeyCtrlL:
		return Result{Action: ActionClearScreen}

	case input.KeyEscape:
		if e.completion != nil {
			// Escape abandons a completion cycle without changing the draft.
			e.completion = nil
			return Result{Action: ActionModified}
		}
		return Result{Action: ActionNone}

	default:
		return Result{Action: ActionUnhandled}
	}
}

// editableRune reports whether a rune may enter the draft. Control bytes never
// do: a draft that contained them could carry a sequence into a command, and
// the editor is the last place that can refuse one.
func editableRune(r rune) bool {
	switch {
	case r == '\t' || r == '\n':
		return true
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return false
	}
	return true
}

// insert writes runes at the cursor and reports how many were inserted and
// whether the draft limits cut the rest.
func (e *Editor) insert(runes []rune) (int, bool) {
	inserted := 0
	truncated := false
	for _, r := range runes {
		switch {
		case len(e.draft) >= e.limits.MaxRunes:
			truncated = true
			continue
		case r == '\n' && e.LineCount() >= e.limits.MaxLines:
			truncated = true
			continue
		}
		e.draft = slices.Insert(e.draft, e.cursor, r)
		e.cursor++
		inserted++
	}
	return inserted, truncated
}

// Cancel discards the draft, any pending completion, and history navigation,
// leaving an empty prompt ready for input.
func (e *Editor) Cancel() {
	e.draft = e.draft[:0]
	e.cursor = 0
	e.completion = nil
	e.resetNavigation()
}

// SetDraft replaces the draft and places the cursor at its end, for a session
// that restores a draft or pre-fills one.
func (e *Editor) SetDraft(text string) {
	e.draft = []rune(text)
	e.cursor = len(e.draft)
	e.completion = nil
	e.resetNavigation()
}

// SetCursor places the cursor at a rune index, clamped to the draft, for a
// session that positions the cursor itself.
func (e *Editor) SetCursor(index int) Result {
	target := min(max(index, 0), len(e.draft))
	if target == e.cursor {
		return Result{Action: ActionNone}
	}
	e.cursor = target
	e.completion = nil
	return Result{Action: ActionModified}
}

func (e *Editor) enterPressed() Result {
	if e.enter == EnterInsertsNewline {
		inserted, truncated := e.insert([]rune{'\n'})
		if inserted == 0 {
			return Result{Action: ActionNone, Truncated: truncated}
		}
		e.resetNavigation()
		return Result{Action: ActionModified, Inserted: inserted, Truncated: truncated}
	}
	if e.Empty() {
		// An empty submission is not a command; the prompt is simply redrawn.
		return Result{Action: ActionNone}
	}
	text := e.Draft()
	e.draft = e.draft[:0]
	e.cursor = 0
	e.completion = nil
	e.resetNavigation()
	e.history.Append(text)
	return Result{Action: ActionSubmit, Text: text}
}

func (e *Editor) moveTo(index int) Result {
	if index == e.cursor {
		return Result{Action: ActionNone}
	}
	e.cursor = index
	e.completion = nil
	return Result{Action: ActionModified}
}

func (e *Editor) moveHorizontal(delta int) Result {
	if delta < 0 {
		return e.moveTo(prevBoundary(e.draft, e.cursor))
	}
	return e.moveTo(nextBoundary(e.draft, e.cursor))
}

// moveVertical moves the cursor one line up or down, keeping its column within
// the target line.
func (e *Editor) moveVertical(delta int) Result {
	start, end := lineBounds(e.draft, e.cursor)
	column := e.cursor - start
	var targetStart, targetEnd int
	if delta < 0 {
		if start == 0 {
			return Result{Action: ActionNone}
		}
		targetStart, targetEnd = lineBounds(e.draft, start-1)
	} else {
		if end >= len(e.draft) {
			return Result{Action: ActionNone}
		}
		targetStart, targetEnd = lineBounds(e.draft, end+1)
	}
	return e.moveTo(min(targetStart+column, targetEnd))
}

func (e *Editor) eraseBackward() Result {
	if e.cursor == 0 {
		return Result{Action: ActionNone}
	}
	return e.eraseRange(prevBoundary(e.draft, e.cursor), e.cursor)
}

func (e *Editor) eraseForward() Result {
	if e.cursor >= len(e.draft) {
		return Result{Action: ActionNone}
	}
	return e.eraseRange(e.cursor, nextBoundary(e.draft, e.cursor))
}

// eraseWordBackward deletes the word before the cursor, skipping the spaces in
// between, which is what the common readline binding does.
func (e *Editor) eraseWordBackward() Result {
	start := e.cursor
	for start > 0 && isSpaceRune(e.draft[start-1]) {
		start--
	}
	for start > 0 && isWordRune(e.draft[start-1]) {
		start--
	}
	return e.eraseRange(start, e.cursor)
}

func (e *Editor) eraseRange(start, end int) Result {
	if start >= end {
		return Result{Action: ActionNone}
	}
	e.draft = slices.Delete(e.draft, start, end)
	e.cursor = start
	e.completion = nil
	e.resetNavigation()
	return Result{Action: ActionModified}
}

func (e *Editor) lineStart() int {
	start := e.cursor
	for start > 0 && e.draft[start-1] != '\n' {
		start--
	}
	return start
}

func (e *Editor) lineEnd() int {
	end := e.cursor
	for end < len(e.draft) && e.draft[end] != '\n' {
		end++
	}
	return end
}

// historyPrevious recalls the next older line. The draft in progress is saved
// on the first press and restored when navigation returns past the newest
// entry, so recalling and editing do not destroy what was being typed.
func (e *Editor) historyPrevious() Result {
	if e.history.Len() == 0 {
		return Result{Action: ActionNone}
	}
	if !e.navigating {
		e.pending = append(e.pending[:0], e.draft...)
		e.historyPos = 0
		e.navigating = true
	} else if e.historyPos+1 >= e.history.Len() {
		return Result{Action: ActionNone}
	} else {
		e.historyPos++
	}
	entry, _ := e.history.At(e.historyPos)
	e.replaceDraft(entry)
	return Result{Action: ActionModified}
}

func (e *Editor) historyNext() Result {
	if !e.navigating {
		return Result{Action: ActionNone}
	}
	if e.historyPos == 0 {
		e.replaceDraft(string(e.pending))
		e.pending = e.pending[:0]
		e.navigating = false
		return Result{Action: ActionModified}
	}
	e.historyPos--
	entry, _ := e.history.At(e.historyPos)
	e.replaceDraft(entry)
	return Result{Action: ActionModified}
}

func (e *Editor) replaceDraft(text string) {
	e.draft = []rune(text)
	e.cursor = len(e.draft)
	e.completion = nil
}

// resetNavigation ends history navigation because the draft changed: a recall
// followed by an edit means the recalled line is no longer what the user is
// navigating through.
func (e *Editor) resetNavigation() {
	e.navigating = false
	e.historyPos = 0
	e.pending = e.pending[:0]
}

func (e *Editor) replaceDraftWord(start int, text string) {
	e.draft = slices.Replace(e.draft, start, e.cursor, []rune(text)...)
	e.cursor = start + utf8.RuneCountInString(text)
	e.resetNavigation()
}

// ApplyCompletion inserts the longest completion the candidates agree on, or
// cycles through them when they share no further prefix. Showing a completion
// never runs it: the draft is text until the user submits it.
func (e *Editor) ApplyCompletion(candidates []string) Result {
	if len(candidates) == 0 {
		e.completion = nil
		return Result{Action: ActionNone}
	}
	before := e.Draft()
	if e.completion != nil {
		e.nextCandidate()
		e.replaceDraftWord(e.completion.wordStart, e.completion.candidates[e.completion.next])
		return e.completionResult(before)
	}

	start := e.wordStart()
	word := string(e.draft[start:e.cursor])
	matches := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, word) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return Result{Action: ActionNone}
	}
	e.completion = &completion{wordStart: start, candidates: matches, next: -1}
	if len(matches) == 1 {
		e.completion = nil
		e.replaceDraftWord(start, matches[0])
		return e.completionResult(before)
	}
	prefix := longestCommonPrefix(matches)
	if len(prefix) <= len(word) {
		// Nothing further can be inserted without choosing, but the cycle stays
		// armed so the next Tab shows the alternatives one at a time.
		return Result{Action: ActionNone}
	}
	e.replaceDraftWord(start, prefix)
	return e.completionResult(before)
}

// nextCandidate advances a completion cycle to the next candidate worth
// showing. A candidate identical to the text already in the draft is skipped,
// because after the common prefix was inserted it would change nothing.
func (e *Editor) nextCandidate() {
	shown := string(e.draft[e.completion.wordStart:e.cursor])
	for range e.completion.candidates {
		e.completion.next = (e.completion.next + 1) % len(e.completion.candidates)
		if e.completion.candidates[e.completion.next] != shown {
			return
		}
	}
}

// completionResult reports a modification only when the draft actually changed,
// so a completed word is not needlessly repainted.
func (e *Editor) completionResult(before string) Result {
	if e.Draft() == before {
		return Result{Action: ActionNone}
	}
	return Result{Action: ActionModified}
}

// wordStart returns the index where the word under the cursor begins. Paths and
// arguments count as one word, so completing "src/te" after "cd src/te" works
// the way a user expects.
func (e *Editor) wordStart() int {
	start := e.cursor
	for start > 0 && isWordRune(e.draft[start-1]) {
		start--
	}
	return start
}

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// isWordRune reports whether a rune belongs to a completable word.
func isWordRune(r rune) bool {
	switch r {
	case '-', '_', '.', '/', '~', '=', ':', '@', '%', '+', ',':
		return true
	}
	return r > ' ' && r != 0x7f && !(r >= 0x80 && r <= 0x9f)
}

// longestCommonPrefix returns the longest prefix every candidate shares.
func longestCommonPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := values[0]
	for _, value := range values[1:] {
		limit := min(len(prefix), len(value))
		shared := 0
		for shared < limit && prefix[shared] == value[shared] {
			shared++
		}
		prefix = prefix[:shared]
		if prefix == "" {
			break
		}
	}
	return prefix
}

// prevBoundary returns the start of the grapheme before a rune index, so
// erasure removes a whole grapheme and never leaves a base rune without its
// marks.
func prevBoundary(runes []rune, index int) int {
	if index <= 0 {
		return 0
	}
	start := index - 1
	for start > 0 && screen.RuneWidth(runes[start]) == 0 {
		start--
	}
	return start
}

// nextBoundary returns the end of the grapheme at a rune index.
func nextBoundary(runes []rune, index int) int {
	if index >= len(runes) {
		return len(runes)
	}
	end := index + 1
	for end < len(runes) && screen.RuneWidth(runes[end]) == 0 {
		end++
	}
	return end
}

// lineBounds returns the rune range of the display line containing an index,
// excluding the newline that ends it.
func lineBounds(runes []rune, index int) (int, int) {
	start := min(index, len(runes))
	for start > 0 && runes[start-1] != '\n' {
		start--
	}
	end := min(index, len(runes))
	for end < len(runes) && runes[end] != '\n' {
		end++
	}
	return start, end
}
