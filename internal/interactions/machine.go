package interactions

import (
	"encoding/json"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// OutcomeKind says what one Apply did, so the host knows whether to render,
// wait for another chord, or hand the key to the application.
type OutcomeKind string

const (
	// OutcomeApplied means a primitive action ran and the view changed.
	OutcomeApplied OutcomeKind = "applied"
	// OutcomePending means the keys so far are the beginning of a declared
	// multi-key command; nothing changed yet.
	OutcomePending OutcomeKind = "pending"
	// OutcomeUnknown means no declared binding claimed the key. The host must
	// offer it to the application's handler and, when the handler cannot act,
	// route it to the model-driven extension path. It must never fall through
	// to a real executable.
	OutcomeUnknown OutcomeKind = "unknown"
	// OutcomeExited means the interaction already asked to leave.
	OutcomeExited OutcomeKind = "exited"
)

// Input is one key delivery from the terminal layer: a chord sequence (usually
// a single chord, occasionally a small run from fast typing) plus the printable
// text that arrived with it. Text belongs to the first chord of the run, which
// is what a text-entry binding consumes.
type Input struct {
	Chords Sequence
	Text   string
}

// EventPayload is the bounded application data an emitted semantic event
// carries. It is data for the application's handler, never a command the
// runtime executes.
type EventPayload struct {
	// Name is the application's own event name.
	Name string `json:"name"`
	// Command is the text the application collected in its prompt line.
	Command string `json:"command,omitempty"`
	// Buffer is the current document text, when the binding asked for it.
	Buffer string `json:"buffer,omitempty"`
	// Selection is the selected text, when the binding asked for it.
	Selection string `json:"selection,omitempty"`
	// Mode is the interaction mode at emission time.
	Mode string `json:"mode,omitempty"`
	// Chord is the canonical key sequence that triggered the event.
	Chord string `json:"chord,omitempty"`
	// Fields are the application's scalar event data, bounded by the limits.
	Fields map[string]string `json:"fields,omitempty"`
}

// Size is the payload's bounded footprint, used to enforce the event limits.
func (p EventPayload) Size() int {
	size := len(p.Name) + len(p.Command) + len(p.Buffer) + len(p.Selection) +
		len(p.Mode) + len(p.Chord) + eventFieldsSize(p.Fields)
	return size
}

// Outcome is the result of one Apply.
type Outcome struct {
	Kind    OutcomeKind
	View    domain.AppView
	State   State
	Events  []EventPayload
	Pending Sequence
	// Chord is the canonical spelling of the key that went unclaimed, set only
	// for OutcomeUnknown so the host can build the handler's input event.
	Chord string
}

// cursorSpace is the address space the cursor lives in for a view mode.
type cursorSpace uint8

const (
	// spaceBuffer addresses document runes (text, editor, and pager views).
	spaceBuffer cursorSpace = iota
	// spaceCells addresses table cells (table and status views).
	spaceCells
	// spaceFields addresses form fields, whose values are one-line documents.
	spaceFields
)

func cursorSpaceFor(viewMode string) cursorSpace {
	switch viewMode {
	case domain.AppViewModeTable, domain.AppViewModeStatus:
		return spaceCells
	case domain.AppViewModeForm:
		return spaceFields
	default:
		return spaceBuffer
	}
}

// Machine applies key input to a validated Spec and application State. It is
// the whole interaction runtime: no I/O, no clock, no randomness, no terminal.
type Machine struct {
	spec    Spec
	state   State
	screen  ScreenMetrics
	limits  Limits
	pending Sequence
	table   []binding
}

// New validates a spec against a view mode and builds a machine. Every
// declaration and every piece of state is bounded here, so a later key press
// cannot exceed a limit that was already known to be too large.
func New(spec Spec, state State, screen ScreenMetrics, limits Limits) (*Machine, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	limits = limits.withDefaults()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := screen.Validate(); err != nil {
		return nil, err
	}
	if spec.Mode == "" {
		spec.Mode = spec.DefaultMode
	}
	if state.Mode == "" {
		state.Mode = spec.Mode
	}
	m := &Machine{spec: spec, state: state, screen: screen, limits: limits}
	if err := m.rebuildTable(); err != nil {
		return nil, err
	}
	if err := m.checkStateBounds(); err != nil {
		return nil, err
	}
	if err := m.clampViewport(); err != nil {
		return nil, err
	}
	return m, nil
}

// Spec returns the interaction description the machine runs.
func (m *Machine) Spec() Spec { return m.spec }

// State returns the current interaction state.
func (m *Machine) State() State { return m.state.clone() }

// Limits returns the enforced bounds.
func (m *Machine) Limits() Limits { return m.limits }

// Screen returns the current screen metrics.
func (m *Machine) Screen() ScreenMetrics { return m.screen }

// SetScreen applies a resize: the viewport and the cursor are re-clamped to the
// new size, and nothing else changes.
func (m *Machine) SetScreen(screen ScreenMetrics) error {
	if err := screen.Validate(); err != nil {
		return err
	}
	m.screen = screen
	return m.checkStateBounds()
}

// Replace installs the spec and state an application handler returned. It is how
// a handler switches modes, replaces buffer or table data, and reports a new
// status line. The result is validated and bounded exactly like New, because
// application data is untrusted input to this package too.
func (m *Machine) Replace(spec Spec, state State) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.Mode == "" {
		spec.Mode = spec.DefaultMode
	}
	if state.Mode == "" {
		state.Mode = spec.Mode
	}
	previousSpec, previousState := m.spec, m.state
	m.spec = spec
	m.state = state
	m.pending = nil
	if err := m.rebuildTable(); err != nil {
		m.spec, m.state = previousSpec, previousState
		return err
	}
	if err := m.checkStateBounds(); err != nil {
		m.spec, m.state = previousSpec, previousState
		return err
	}
	return m.clampViewport()
}

// Apply feeds one input delivery through the declared bindings.
func (m *Machine) Apply(in Input) (Outcome, error) {
	if len(in.Chords) == 0 {
		return Outcome{}, domain.NewValidationError(domain.CodeInvalidInput,
			"input carries no chords", nil)
	}
	if m.state.Exited {
		return m.outcome(OutcomeExited, nil, m.pending), nil
	}
	var events []EventPayload
	kind := OutcomeApplied
	unbound := ""
	for i, chord := range in.Chords {
		text := ""
		if i == 0 {
			text = in.Text
		}
		pressKind, pressed, err := m.press(chord, text)
		if err != nil {
			return Outcome{}, err
		}
		kind = pressKind
		events = append(events, pressed...)
		if pressKind == OutcomeUnknown {
			unbound = chord.String()
		}
		if pressKind == OutcomeUnknown {
			break
		}
	}
	if err := m.checkEventLimits(events); err != nil {
		return Outcome{}, err
	}
	outcome := m.outcome(kind, events, m.pending)
	outcome.Chord = unbound
	return outcome, nil
}

// press applies one chord, keeping any partial multi-key command pending.
func (m *Machine) press(chord Chord, text string) (OutcomeKind, []EventPayload, error) {
	candidate := m.pending.Append(chord)
	match, exact := m.findBinding(candidate, chord, typedText(chord, text))
	switch {
	case exact:
		events, err := m.applyBinding(match, typedText(chord, text))
		if err != nil {
			return OutcomeApplied, nil, err
		}
		m.pending = nil
		if err := m.checkStateBounds(); err != nil {
			return OutcomeApplied, nil, err
		}
		return OutcomeApplied, events, nil
	case match != nil:
		m.pending = candidate
		return OutcomePending, nil, nil
	default:
		m.pending = nil
		return OutcomeUnknown, nil, nil
	}
}

// findBinding resolves a chord sequence against the current mode's table. A
// binding on the text input class matches any printable chord or any delivery
// that carries text; every other binding matches chord for chord, and a
// sequence that is only a prefix of a declared command keeps the machine
// waiting.
func (m *Machine) findBinding(candidate Sequence, chord Chord, text string) (*binding, bool) {
	var partial *binding
	for i := range m.table {
		keys := m.table[i].Keys
		if isTextBinding(keys) {
			if _, printable := chord.PrintableText(); printable || text != "" {
				return &m.table[i], true
			}
			continue
		}
		if candidate.Equal(keys) {
			return &m.table[i], true
		}
		if candidate.IsPrefixOf(keys) {
			partial = &m.table[i]
		}
	}
	return partial, false
}

// isTextBinding reports whether a binding claims the printable-text input class.
func isTextBinding(keys Sequence) bool {
	return len(keys) == 1 && keys[0].Kind == KeyNamed && keys[0].Named == KeyText
}

// typedText is the text an edit consumes: the run the terminal delivered with
// the event, or the printable character itself when the event carried no run.
func typedText(chord Chord, text string) string {
	if text != "" {
		return text
	}
	if printable, ok := chord.PrintableText(); ok {
		return printable
	}
	return ""
}

func (m *Machine) outcome(kind OutcomeKind, events []EventPayload, pending Sequence) Outcome {
	view, err := m.view()
	if err != nil {
		// The view was validated when the machine was built and after every
		// applied binding; a failure here would be an internal invariant.
		view = domain.AppView{Mode: m.spec.View.Mode}
	}
	return Outcome{Kind: kind, View: view, State: m.state.clone(), Events: events, Pending: pending}
}

// Equal reports whether two sequences are identical.
func (s Sequence) Equal(other Sequence) bool {
	if len(s) != len(other) {
		return false
	}
	for i := range s {
		if s[i] != other[i] {
			return false
		}
	}
	return true
}

// rebuildTable decodes the effective binding table of the current mode.
func (m *Machine) rebuildTable() error {
	table, err := m.spec.effectiveBindings(m.state.Mode)
	if err != nil {
		return err
	}
	if exceeded(int64(len(table)), int64(m.limits.MaxBindings)) {
		return limitExceeded("max_bindings", int64(len(table)), int64(m.limits.MaxBindings))
	}
	for i := range table {
		if exceeded(int64(len(table[i].Keys)), int64(m.limits.MaxChordLength)) {
			return limitExceeded("max_chord_length", int64(len(table[i].Keys)), int64(m.limits.MaxChordLength))
		}
		if err := checkBindingForView(m.spec.View.Mode, table[i].Action, table[i].Params); err != nil {
			return err
		}
	}
	m.table = table
	return nil
}

// checkBindingForView rejects a binding whose action cannot mean anything in the
// declared view mode. Catching it here means the host never has to interpret a
// runtime failure as a missing key.
func checkBindingForView(viewMode, action string, params Params) error {
	space := cursorSpaceFor(viewMode)
	switch action {
	case domain.ActionEditBuffer:
		if space == spaceCells {
			return specError("view mode %q has no editable target for edit_buffer", viewMode)
		}
	case domain.ActionSearchBuffer:
		if space != spaceBuffer {
			return specError("view mode %q has no buffer to search", viewMode)
		}
	case domain.ActionMoveCursor:
		if params.Move.Target == MoveFieldPrevious || params.Move.Target == MoveFieldNext {
			if space != spaceFields {
				return specError("view mode %q cannot move between fields", viewMode)
			}
		}
	}
	return nil
}

// checkStateBounds enforces every bound that depends on the current state.
func (m *Machine) checkStateBounds() error {
	if err := m.editDoc().checkDocBounds(m.limits); err != nil {
		return err
	}
	if err := checkTableBounds(m.state.Table, m.limits); err != nil {
		return err
	}
	if exceeded(int64(len(m.state.Fields)), int64(m.limits.MaxFields)) {
		return limitExceeded("max_fields", int64(len(m.state.Fields)), int64(m.limits.MaxFields))
	}
	for i, field := range m.state.Fields {
		if exceeded(int64(len(field)), int64(m.limits.MaxFieldBytes)) {
			return limitExceeded("max_field_bytes", int64(len(field)), int64(m.limits.MaxFieldBytes))
		}
		if m.spec.View.Mode == domain.AppViewModeForm && i >= len(m.spec.View.Fields) {
			return specError("form value %d has no declared field", i)
		}
	}
	if exceeded(int64(len(m.state.Status)), int64(m.limits.MaxStatusBytes)) {
		return limitExceeded("max_status_bytes", int64(len(m.state.Status)), int64(m.limits.MaxStatusBytes))
	}
	if len(m.state.Undo) > m.limits.MaxUndoDepth {
		return limitExceeded("max_undo_depth", int64(len(m.state.Undo)), int64(m.limits.MaxUndoDepth))
	}
	if len(m.state.Redo) > m.limits.MaxUndoDepth {
		return limitExceeded("max_undo_depth", int64(len(m.state.Redo)), int64(m.limits.MaxUndoDepth))
	}
	if exceeded(int64(len(m.state.PromptText)), int64(m.limits.MaxEventBytes)) {
		return limitExceeded("max_event_bytes", int64(len(m.state.PromptText)), int64(m.limits.MaxEventBytes))
	}
	return nil
}

func (m *Machine) checkEventLimits(events []EventPayload) error {
	if exceeded(int64(len(events)), int64(m.limits.MaxEventsPerKey)) {
		return limitExceeded("max_events_per_key", int64(len(events)), int64(m.limits.MaxEventsPerKey))
	}
	for _, event := range events {
		if exceeded(int64(event.Size()), int64(m.limits.MaxEventBytes)) {
			return limitExceeded("max_event_bytes", int64(event.Size()), int64(m.limits.MaxEventBytes))
		}
	}
	return nil
}

// view renders the validated renderer-facing view. Every binding of the current
// mode is published so the trusted renderer can draw key hints, and the whole
// result is re-validated with the domain contract before it leaves.
func (m *Machine) view() (domain.AppView, error) {
	metadata, err := m.spec.View.metadata()
	if err != nil {
		return domain.AppView{}, err
	}
	bindings := make([]domain.KeyBinding, 0, len(m.table))
	for _, b := range m.table {
		raw, err := jsonRaw(b.Params)
		if err != nil {
			return domain.AppView{}, err
		}
		bindings = append(bindings, domain.KeyBinding{
			Key:    b.Keys.String(),
			Action: b.Action,
			Params: raw,
		})
	}
	cursor := m.focusCursor()
	view := domain.AppView{
		Mode:        m.spec.View.Mode,
		BufferRef:   m.state.Buffer.Ref,
		Cursor:      domain.CursorPosition{Row: cursor.Line, Col: cursor.Column},
		Viewport:    m.state.Viewport,
		StatusLine:  m.statusText(),
		KeyBindings: bindings,
		Metadata:    metadata,
	}
	if err := domain.ValidateView(view); err != nil {
		return domain.AppView{}, domain.NewValidationError(domain.CodeInvalidAppView,
			"interaction produced an invalid view: "+err.Error(), nil)
	}
	return view, nil
}

// View returns the current renderer-facing view.
func (m *Machine) View() (domain.AppView, error) { return m.view() }

// focusCursor is the cursor in the view's focus space.
func (m *Machine) focusCursor() Cursor {
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		return m.state.Table.Cursor
	case spaceFields:
		doc, err := m.fieldDoc()
		if err != nil {
			return Cursor{}
		}
		return doc.cursor
	default:
		return m.state.Buffer.Cursor
	}
}

// ---------------------------------------------------------------------------
// Edit target
// ---------------------------------------------------------------------------

// editDoc returns the editable document of the current view, bounded.
func (m *Machine) editDoc() bufferDoc {
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		return bufferDoc{lines: []string{""}, single: true}
	case spaceFields:
		doc, err := m.fieldDoc()
		if err != nil {
			return bufferDoc{lines: []string{""}, single: true}
		}
		return doc
	default:
		doc := bufferDoc{lines: m.state.Buffer.Lines, cursor: m.state.Buffer.Cursor, anchor: m.state.Buffer.Anchor}
		if len(doc.lines) == 0 {
			doc.lines = []string{""}
		}
		return doc
	}
}

// fieldDoc returns the focused form field as a one-line document.
func (m *Machine) fieldDoc() (bufferDoc, error) {
	if cursorSpaceFor(m.spec.View.Mode) != spaceFields {
		return bufferDoc{}, errEditTarget
	}
	if len(m.spec.View.Fields) == 0 {
		return bufferDoc{}, errEditTarget
	}
	focus := m.state.Focus
	if focus < 0 || focus >= len(m.spec.View.Fields) {
		focus = 0
	}
	value := ""
	if focus < len(m.state.Fields) {
		value = m.state.Fields[focus]
	}
	policy := m.spec.columnPolicy(m.state.Mode)
	return bufferDoc{
		lines:  []string{value},
		cursor: clampedFieldCursor(m.state, value, policy),
		single: true,
	}, nil
}

// storeDoc writes the edited document back into the state, in the space the
// view mode uses. In a form view the interaction buffer cursor holds the
// position inside the focused field, so one cursor serves both spaces.
func (m *Machine) storeDoc(doc bufferDoc) {
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		// Table cells are not editable; checkBindingForView rejects such a
		// binding before it can reach here.
	case spaceFields:
		focus := m.state.Focus
		if focus < 0 || focus >= len(m.spec.View.Fields) {
			focus = 0
		}
		for len(m.state.Fields) <= focus {
			m.state.Fields = append(m.state.Fields, "")
		}
		m.state.Fields[focus] = doc.line(0)
		m.state.Buffer.Cursor = doc.cursor
	default:
		m.state.Buffer.Lines = normalizeDocument(doc.lines)
		m.state.Buffer.Cursor = doc.cursor
		m.state.Buffer.Anchor = doc.anchor
	}
}

func clampedFieldCursor(state State, value string, policy ColumnPolicy) Cursor {
	// The field's column is tracked in the buffer cursor; forms share the one
	// position across the single-line document they edit.
	cursor := state.Buffer.Cursor
	doc := bufferDoc{lines: []string{value}}
	return doc.clampPosition(cursor, policy)
}

// ---------------------------------------------------------------------------
// Bounds on the editable document
// ---------------------------------------------------------------------------

// applyEdit runs one buffer edit on the view's edit target. The edit is applied
// to a copy and only stored once every bound holds, so a refused edit leaves
// the document exactly as it was.
func (m *Machine) applyEdit(op EditOp, params EditParams, inputText string) error {
	switch op {
	case EditUndo:
		return m.undo()
	case EditRedo:
		return m.redo()
	}
	if op == EditInsert && params.Text == TextFromInput && inputText == "" {
		// Nothing was typed; the binding is satisfied by doing nothing.
		return nil
	}
	policy := m.spec.columnPolicy(m.state.Mode)
	original := m.editDoc()
	original.cursor = original.clampPosition(original.cursor, policy)
	before := bufferDoc{lines: original.lines}
	if err := before.checkDocBounds(m.limits); err != nil {
		return err
	}
	working := original.clone()
	if err := working.apply(op, params, inputText, policy); err != nil {
		return err
	}
	working.cursor = working.clampPosition(working.cursor, policy)
	after := bufferDoc{lines: working.lines}
	if err := after.checkDocBounds(m.limits); err != nil {
		return err
	}
	m.pushUndo(original)
	m.markDirty()
	m.storeDoc(working)
	m.ensureVisible()
	return nil
}

// apply performs one buffer edit on the document.
func (d *bufferDoc) apply(op EditOp, params EditParams, inputText string, policy ColumnPolicy) error {
	text := params.Literal
	if params.Text == TextFromInput {
		text = inputText
	}
	count := params.Count
	if count <= 0 {
		count = 1
	}
	switch op {
	case EditInsert:
		if d.single && strings.Contains(text, "\n") {
			return specError("a single-line field cannot receive a newline")
		}
		d.cursor = d.insertText(d.cursor, text)
	case EditReplace:
		if start, end, ok := d.selection(); ok {
			d.cursor = d.deleteRange(start, end)
			d.anchor = nil
		}
		if text != "" {
			d.cursor = d.insertText(d.cursor, text)
		}
	case EditDeleteBefore:
		if start, end, ok := d.selection(); ok {
			d.cursor = d.deleteRange(start, end)
			d.anchor = nil
			return nil
		}
		if d.cursor.Column > 0 {
			d.cursor = d.deleteRange(Cursor{Line: d.cursor.Line, Column: d.cursor.Column - 1}, d.cursor)
		} else if d.cursor.Line > 0 {
			d.cursor = d.deleteRange(Cursor{Line: d.cursor.Line - 1, Column: lineLen(d.line(d.cursor.Line - 1))}, d.cursor)
		}
	case EditDeleteAfter:
		if start, end, ok := d.selection(); ok {
			d.cursor = d.deleteRange(start, end)
			d.anchor = nil
			return nil
		}
		if d.cursor.Column < lineLen(d.line(d.cursor.Line)) {
			d.cursor = d.deleteRange(d.cursor, Cursor{Line: d.cursor.Line, Column: d.cursor.Column + 1})
		} else if d.cursor.Line < len(d.lines)-1 {
			d.cursor = d.deleteRange(d.cursor, Cursor{Line: d.cursor.Line + 1, Column: 0})
		}
	case EditDeleteSelection:
		if start, end, ok := d.selection(); ok {
			d.cursor = d.deleteRange(start, end)
			d.anchor = nil
		}
	case EditDeleteLine:
		for i := 0; i < count; i++ {
			d.cursor = d.deleteLine(d.cursor, policy)
		}
	case EditDeleteToLineEnd:
		end := Cursor{Line: d.cursor.Line, Column: lineLen(d.line(d.cursor.Line))}
		if end.Column > d.cursor.Column {
			d.cursor = d.deleteRange(d.cursor, end)
		}
	case EditDeleteWordBefore:
		if d.cursor.Column > 0 {
			d.cursor = d.deleteWordBefore()
		}
	case EditNewline:
		if d.single {
			return specError("a single-line field cannot be split")
		}
		d.cursor = d.insertText(d.cursor, "\n")
	case EditJoinLine:
		d.cursor, _ = d.joinLine(d.cursor, policy)
	}
	return nil
}

// deleteWordBefore removes the word before the cursor, together with the
// whitespace that separates it from the cursor.
func (d *bufferDoc) deleteWordBefore() Cursor {
	runes := d.lineRunes(d.cursor.Line)
	boundary := 0
	for i := d.cursor.Column - 1; i >= 0; i-- {
		if !isWordRune(runes[i]) {
			boundary = i + 1
			break
		}
		boundary = i
	}
	return d.deleteRange(Cursor{Line: d.cursor.Line, Column: boundary}, d.cursor)
}

// normalizeDocument guarantees at least one line.
func normalizeDocument(lines []string) []string {
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// pushUndo records the pre-edit document, clearing the redo stack.
func (m *Machine) pushUndo(doc bufferDoc) {
	entry := Snapshot{Lines: append([]string(nil), doc.lines...), Cursor: doc.cursor}
	if doc.anchor != nil {
		anchor := *doc.anchor
		entry.Anchor = &anchor
	}
	m.state.Undo = append(m.state.Undo, entry)
	if len(m.state.Undo) > m.limits.MaxUndoDepth {
		m.state.Undo = append([]Snapshot(nil), m.state.Undo[len(m.state.Undo)-m.limits.MaxUndoDepth:]...)
	}
	m.state.Redo = nil
}

func (m *Machine) undo() error {
	if len(m.state.Undo) == 0 {
		m.state.Status = statusNothingToUndo
		return nil
	}
	entry := m.state.Undo[len(m.state.Undo)-1]
	m.state.Undo = m.state.Undo[:len(m.state.Undo)-1]
	m.state.Redo = append(m.state.Redo, snapshotOf(m.currentDoc()))
	m.restore(entry)
	return nil
}

func (m *Machine) redo() error {
	if len(m.state.Redo) == 0 {
		m.state.Status = statusNothingToRedo
		return nil
	}
	entry := m.state.Redo[len(m.state.Redo)-1]
	m.state.Redo = m.state.Redo[:len(m.state.Redo)-1]
	m.state.Undo = append(m.state.Undo, snapshotOf(m.currentDoc()))
	m.restore(entry)
	return nil
}

// currentDoc is the editable document as it stands, for undo bookkeeping.
func (m *Machine) currentDoc() Buffer {
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		return Buffer{}
	case spaceFields:
		doc, err := m.fieldDoc()
		if err != nil {
			return Buffer{Lines: []string{""}}
		}
		return Buffer{Lines: doc.lines, Cursor: doc.cursor}
	default:
		return m.state.Buffer
	}
}

// restore installs a snapshot as the current document.
func (m *Machine) restore(entry Snapshot) {
	doc := bufferDoc{lines: entry.Lines, cursor: entry.Cursor, anchor: entry.Anchor, single: m.singleLineView()}
	m.storeDoc(doc)
	m.state.Status = ""
	m.ensureVisible()
}

func (m *Machine) singleLineView() bool {
	return cursorSpaceFor(m.spec.View.Mode) == spaceFields
}

// markDirty records that the document changed; the application decides what a
// dirty buffer means for its own commit path.
func (m *Machine) markDirty() { m.state.Dirty = true }

// ---------------------------------------------------------------------------
// Binding application
// ---------------------------------------------------------------------------

// applyBinding runs one approved primitive action.
func (m *Machine) applyBinding(b *binding, inputText string) ([]EventPayload, error) {
	switch b.Action {
	case domain.ActionMoveCursor:
		return nil, m.applyMove(*b.Params.Move)
	case domain.ActionEditBuffer:
		return nil, m.applyEdit(b.Params.Edit.Op, *b.Params.Edit, inputText)
	case domain.ActionSelectRange:
		return nil, m.applySelect(*b.Params.Select)
	case domain.ActionSearchBuffer:
		return nil, m.applySearch(*b.Params.Search)
	case domain.ActionScroll:
		return nil, m.applyScroll(*b.Params.Scroll)
	case domain.ActionUpdateStatus:
		return nil, m.applyStatus(*b.Params.Status)
	case domain.ActionModeSwitch:
		return nil, m.applyModeSwitch(*b.Params.Mode)
	case domain.ActionEmitEvent:
		event, err := m.applyEmit(*b.Params.Event, b.Keys)
		if err != nil {
			return nil, err
		}
		return []EventPayload{event}, nil
	default:
		return nil, specError("action %q is not an approved primitive action", b.Action)
	}
}

func (m *Machine) applyMove(params MoveParams) error {
	count, err := boundCount(params.Count, m.limits, "max_count")
	if err != nil {
		return err
	}
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		m.moveCell(params.Target, count)
		return m.clampViewport()
	case spaceFields:
		m.moveField(params.Target)
		return m.clampViewport()
	default:
		doc := m.editDoc()
		doc.cursor = doc.move(params.Target, count, m.spec.columnPolicy(m.state.Mode), m.pageRows())
		m.storeDoc(doc)
		m.ensureVisible()
		return nil
	}
}

func (m *Machine) applySelect(params SelectParams) error {
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		start, end, ok := m.cellAnchors(params)
		if !ok {
			m.state.Table.Cursor = clampCell(m.state.Table.Cursor, len(m.state.Table.Rows), len(m.spec.View.Columns))
			m.state.Table.Anchor = nil
			return nil
		}
		first, second := start, end
		if first.Line > second.Line || (first.Line == second.Line && first.Column > second.Column) {
			first, second = second, first
		}
		anchor := first
		m.state.Table.Anchor = &anchor
		m.state.Table.Cursor = second
		return nil
	default:
		doc := m.editDoc()
		anchor, ok := m.anchorFor(doc, params.From)
		if !ok {
			doc.anchor = nil
			m.storeDoc(doc)
			return nil
		}
		head, ok := m.anchorFor(doc, params.To)
		if !ok {
			doc.anchor = nil
			m.storeDoc(doc)
			return nil
		}
		if params.To == AnchorCursor {
			head = doc.cursor
		}
		doc.anchor = &anchor
		doc.cursor = doc.clampPosition(head, m.spec.columnPolicy(m.state.Mode))
		m.storeDoc(doc)
		return nil
	}
}

// anchorFor resolves a selection anchor against the current document.
func (m *Machine) anchorFor(doc bufferDoc, source AnchorSource) (Cursor, bool) {
	policy := m.spec.columnPolicy(m.state.Mode)
	switch source {
	case AnchorCursor:
		return doc.cursor, true
	case AnchorDocStart:
		return Cursor{}, len(doc.lines) > 0
	case AnchorDocEnd:
		if len(doc.lines) == 0 {
			return Cursor{}, false
		}
		return Cursor{Line: len(doc.lines) - 1, Column: lineLen(doc.line(len(doc.lines) - 1))}, true
	case AnchorMatchStart, AnchorMatchEnd:
		if !matchAt(doc.lines, m.state.Match, CaseSensitive) {
			return Cursor{}, false
		}
		match := m.state.Match
		if source == AnchorMatchEnd {
			return Cursor{Line: match.Line, Column: match.End}, true
		}
		return Cursor{Line: match.Line, Column: match.Start}, true
	case AnchorSelectionStart, AnchorSelectionEnd:
		start, end, ok := doc.selection()
		if !ok {
			return Cursor{}, false
		}
		if source == AnchorSelectionEnd {
			return end, true
		}
		return start, true
	default:
		return doc.clampPosition(Cursor{}, policy), false
	}
}

// cellAnchors resolves selection endpoints in the cell space of a table view.
func (m *Machine) cellAnchors(params SelectParams) (Cursor, Cursor, bool) {
	if len(m.state.Table.Rows) == 0 || len(m.spec.View.Columns) == 0 {
		return Cursor{}, Cursor{}, false
	}
	cursor := m.state.Table.Cursor
	resolve := func(source AnchorSource) (Cursor, bool) {
		switch source {
		case AnchorCursor:
			return cursor, true
		case AnchorDocStart:
			return Cursor{}, true
		case AnchorDocEnd:
			return Cursor{Line: len(m.state.Table.Rows) - 1, Column: len(m.spec.View.Columns) - 1}, true
		case AnchorSelectionStart, AnchorSelectionEnd:
			if m.state.Table.Anchor == nil {
				return cursor, false
			}
			if source == AnchorSelectionEnd {
				return cursor, true
			}
			return *m.state.Table.Anchor, true
		default:
			return cursor, false
		}
	}
	start, ok := resolve(params.From)
	if !ok {
		return Cursor{}, Cursor{}, false
	}
	end, ok := resolve(params.To)
	if !ok {
		return Cursor{}, Cursor{}, false
	}
	return start, end, true
}

func (m *Machine) applySearch(params SearchParams) error {
	doc := m.editDoc()
	pattern := resolveQuery(params, m.state)
	outcome, err := search(doc.lines, doc.cursor, pattern, params.Direction, params.Case, m.limits)
	if err != nil {
		return err
	}
	if outcome.Status != "" {
		// A failed search clears the recorded hit, so a later selection of
		// "match_start" cannot point at a match that no longer exists.
		m.state.Match = nil
		m.state.Status = outcome.Status
		return nil
	}
	doc.cursor = doc.clampPosition(outcome.Cursor, m.spec.columnPolicy(m.state.Mode))
	m.storeDoc(doc)
	m.state.Match = outcome.Match
	m.state.LastQuery = pattern
	m.state.Status = ""
	m.ensureVisible()
	return nil
}

func (m *Machine) applyScroll(params ScrollParams) error {
	count, err := boundCount(params.Count, m.limits, "max_count")
	if err != nil {
		return err
	}
	viewport := m.state.Viewport
	pageRows := m.pageRows()
	delta := 0
	switch params.Unit {
	case ScrollLineUp:
		delta = -count
	case ScrollLineDown:
		delta = count
	case ScrollHalfPageUp:
		delta = -count * (pageRows / 2)
	case ScrollHalfPageDown:
		delta = count * (pageRows / 2)
	case ScrollPageUp:
		delta = -count * pageRows
	case ScrollPageDown:
		delta = count * pageRows
	case ScrollFirstLine:
		viewport.Top = 0
	case ScrollLastLine:
		viewport.Top = max(0, m.contentLines()-m.pageRows())
	}
	if params.Unit != ScrollFirstLine && params.Unit != ScrollLastLine {
		viewport.Top += delta
	}
	m.state.Viewport = viewport
	// Scrolling deliberately moves the window away from the cursor, so the
	// cursor is not pulled back into view here; the bounds still clamp.
	return m.clampViewport()
}

// applyStatus sets or clears the status line. The text is bounded here, before
// it can reach the renderer.
func (m *Machine) applyStatus(params StatusParams) error {
	m.state.Status = SanitizeData(params.Text)
	return m.checkStateBounds()
}

func (m *Machine) applyModeSwitch(params ModeParams) error {
	if _, ok := m.spec.mode(params.Mode); !ok {
		return specError("mode %q is not declared", params.Mode)
	}
	m.state.Mode = params.Mode
	m.pending = nil
	if err := m.rebuildTable(); err != nil {
		return err
	}
	// The new mode's column convention applies immediately, which is what
	// returning from text entry to command style editing requires.
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		m.state.Table.Cursor = clampCell(m.state.Table.Cursor, len(m.state.Table.Rows), len(m.spec.View.Columns))
	default:
		doc := m.editDoc()
		doc.cursor = doc.clampPosition(doc.cursor, m.spec.columnPolicy(m.state.Mode))
		m.storeDoc(doc)
	}
	return nil
}

// applyEmit builds the semantic event the application's handler receives. The
// runtime neither interprets nor performs it.
func (m *Machine) applyEmit(params EventParams, keys Sequence) (EventPayload, error) {
	payload := EventPayload{
		Name:  params.Name,
		Mode:  m.state.Mode,
		Chord: keys.String(),
	}
	fields := make(map[string]string, len(params.Fields))
	for key, value := range params.Fields {
		fields[key] = value
	}
	switch params.Snapshot {
	case SnapshotBufferText:
		payload.Buffer = m.bufferText()
	case SnapshotPromptText:
		payload.Command = m.state.PromptText
	case SnapshotSelectionText:
		doc := m.editDoc()
		text, ok := doc.selectionText()
		if !ok {
			return EventPayload{}, specError("selection_text requested with no selection")
		}
		payload.Selection = text
	case SnapshotNone:
	}
	payload.Fields = fields
	if exceeded(int64(payload.Size()), int64(m.limits.MaxEventBytes)) {
		return EventPayload{}, limitExceeded("max_event_bytes", int64(payload.Size()), int64(m.limits.MaxEventBytes))
	}
	return payload, nil
}

func (m *Machine) bufferText() string {
	doc := m.editDoc()
	return strings.Join(doc.lines, "\n")
}

// ---------------------------------------------------------------------------
// Viewport
// ---------------------------------------------------------------------------

// contentLines is how many document rows the view can scroll over.
func (m *Machine) contentLines() int {
	switch cursorSpaceFor(m.spec.View.Mode) {
	case spaceCells:
		return len(m.state.Table.Rows)
	case spaceFields:
		return len(m.spec.View.Fields)
	default:
		return len(m.editDoc().lines)
	}
}

// pageRows is the number of content rows the view can show, once the status
// line is reserved.
func (m *Machine) pageRows() int {
	rows := m.screen.contentRows(m.statusVisible())
	if rows < 1 {
		return 1
	}
	return rows
}

func (m *Machine) statusVisible() bool {
	return m.state.Status != "" || m.spec.View.StatusLine != ""
}

// ensureVisible scrolls the viewport so the focus line stays on screen.
func (m *Machine) ensureVisible() {
	viewport := m.state.Viewport
	rows := m.pageRows()
	focus := m.focusCursor().Line
	if focus < viewport.Top {
		viewport.Top = focus
	}
	if focus >= viewport.Top+rows {
		viewport.Top = focus - rows + 1
	}
	m.state.Viewport = viewport
	_ = m.clampViewport()
}

// clampViewport keeps the window inside the document.
func (m *Machine) clampViewport() error {
	viewport := m.state.Viewport
	rows := m.pageRows()
	total := m.contentLines()
	maxTop := max(0, total-rows)
	if viewport.Top > maxTop {
		viewport.Top = maxTop
	}
	if viewport.Top < 0 {
		viewport.Top = 0
	}
	if viewport.Width <= 0 {
		viewport.Width = m.screen.Columns
	}
	if viewport.Height <= 0 {
		viewport.Height = rows
	}
	m.state.Viewport = viewport
	return nil
}

// ---------------------------------------------------------------------------
// Cell and field movement
// ---------------------------------------------------------------------------

func (m *Machine) moveCell(target MoveTarget, count int) {
	rows, columns := len(m.state.Table.Rows), len(m.spec.View.Columns)
	cursor := clampCell(m.state.Table.Cursor, rows, columns)
	if rows == 0 || columns == 0 {
		m.state.Table.Cursor = cursor
		return
	}
	switch target {
	case MoveLeft:
		cursor.Column = max(0, cursor.Column-count)
	case MoveRight:
		cursor.Column = min(columns-1, cursor.Column+count)
	case MoveUp:
		cursor.Line = max(0, cursor.Line-count)
	case MoveDown:
		cursor.Line = min(rows-1, cursor.Line+count)
	case MoveDocStart:
		cursor = Cursor{}
	case MoveDocEnd:
		cursor = Cursor{Line: rows - 1, Column: columns - 1}
	case MoveLineStart:
		cursor.Column = 0
	case MoveLineEnd:
		cursor.Column = columns - 1
	case MovePageUp:
		cursor.Line = max(0, cursor.Line-count*m.pageRows())
	case MovePageDown:
		cursor.Line = min(rows-1, cursor.Line+count*m.pageRows())
	case MoveLineNumber:
		cursor.Line = clampLine(count-1, rows)
	case MoveWordForward:
		cursor = nextFilledCell(m.state.Table.Rows, cursor, 1)
	case MoveWordBackward:
		cursor = nextFilledCell(m.state.Table.Rows, cursor, -1)
	}
	m.state.Table.Cursor = cursor
}

// nextFilledCell moves to the next row that has any content, which is what
// makes navigation usable on sparse tables.
func nextFilledCell(rows [][]string, cursor Cursor, direction int) Cursor {
	for i := cursor.Line + direction; i >= 0 && i < len(rows); i += direction {
		for _, cell := range rows[i] {
			if strings.TrimSpace(cell) != "" {
				return Cursor{Line: i, Column: cursor.Column}
			}
		}
	}
	return cursor
}

func clampCell(cursor Cursor, rows, columns int) Cursor {
	if rows <= 0 {
		return Cursor{}
	}
	if cursor.Line >= rows {
		cursor.Line = rows - 1
	}
	if cursor.Line < 0 {
		cursor.Line = 0
	}
	if columns <= 0 {
		cursor.Column = 0
		return cursor
	}
	if cursor.Column >= columns {
		cursor.Column = columns - 1
	}
	if cursor.Column < 0 {
		cursor.Column = 0
	}
	return cursor
}

func clampLine(line, rows int) int {
	if line < 0 {
		return 0
	}
	if rows > 0 && line >= rows {
		return rows - 1
	}
	return line
}

// moveField moves the focus between form fields. Entering a field puts the
// cursor at the end of its value, which is where text entry continues.
func (m *Machine) moveField(target MoveTarget) {
	count := len(m.spec.View.Fields)
	if count == 0 {
		return
	}
	switch target {
	case MoveFieldNext:
		m.state.Focus = min(count-1, m.state.Focus+1)
	case MoveFieldPrevious:
		m.state.Focus = max(0, m.state.Focus-1)
	case MoveDocStart:
		m.state.Focus = 0
	case MoveDocEnd:
		m.state.Focus = count - 1
	case MoveLineNumber:
		m.state.Focus = clampLine(m.state.Focus, count)
	}
	doc := m.editDoc()
	doc.cursor = Cursor{Column: lineLen(doc.line(0))}
	m.storeDoc(doc)
}

// ---------------------------------------------------------------------------
// Handler-facing events
// ---------------------------------------------------------------------------

// InputEvent builds the application event for keys no binding claimed. The
// caller sends it to the sandboxed handler; when the handler cannot act, the
// same key is what ExtensionRequest describes to the model.
func InputEvent(nowUnixMilli int64, session *domain.SessionID, turn *domain.TurnID, in Input) (domain.AppEvent, error) {
	if len(in.Chords) == 0 {
		return domain.AppEvent{}, domain.NewValidationError(domain.CodeInvalidInput,
			"input carries no chords", nil)
	}
	payload := map[string]any{
		"keys": SanitizeData(in.Chords.String()),
		"text": SanitizeData(in.Text),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.AppEvent{}, domain.WrapError(err, domain.CategoryInternal,
			domain.CodeSerializationFailed, "encoding app input event")
	}
	return domain.AppEvent{
		EventType: domain.AppEventInput,
		Payload:   raw,
		Timestamp: nowUnixMilli,
		SessionID: session,
		TurnID:    turn,
	}, nil
}

// ExtensionRequest describes an unbound key to the model-driven extension path.
// The prompt carries only sanitized, bounded context: what the screen is, which
// mode is active, and what was pressed. It never carries file content unless the
// caller chose to include a bounded excerpt.
func ExtensionRequest(requestID string, in Input, view domain.AppView, status string, limits Limits) (domain.AIRequest, error) {
	if requestID == "" {
		return domain.AIRequest{}, domain.NewValidationError(domain.CodeInvalidAppResult,
			"extension request needs a request id", nil)
	}
	if len(in.Chords) == 0 {
		return domain.AIRequest{}, domain.NewValidationError(domain.CodeInvalidInput,
			"extension request needs the pressed key", nil)
	}
	if !domain.IsValidAppViewMode(view.Mode) {
		return domain.AIRequest{}, domain.NewValidationError(domain.CodeInvalidAppView,
			fmt.Sprintf("extension request needs a valid view, got %q", view.Mode), nil)
	}
	limits = limits.withDefaults()
	prompt := SanitizeData(fmt.Sprintf(
		"view=%s keys=%s bindings=%d status=%q",
		view.Mode, in.Chords.String(), len(view.KeyBindings), status))
	if len(prompt) > limits.MaxExtensionText {
		prompt = truncateRunes(prompt, limits.MaxExtensionText)
	}
	return domain.AIRequest{
		RequestID: requestID,
		Prompt:    prompt,
		Context:   json.RawMessage(`{}`),
		MaxTokens: defaultExtensionTokens,
		TimeoutMs: defaultExtensionTimeoutMs,
	}, nil
}

// Extension budget: a short, bounded ask. The generation loop owns the real
// token budget; this request must never turn one key press into a long call.
const (
	defaultExtensionTokens    = 800
	defaultExtensionTimeoutMs = 30_000
)

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
