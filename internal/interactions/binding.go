package interactions

import (
	"encoding/json"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
)

// MoveTarget names a cursor destination. The same target set serves every view
// mode; the machine resolves it against the focus space of the current view
// (buffer runes for text-like views, table cells for table and status views,
// fields for forms), so a spec never has to name a program.
type MoveTarget string

const (
	MoveLeft          MoveTarget = "left"
	MoveRight         MoveTarget = "right"
	MoveUp            MoveTarget = "up"
	MoveDown          MoveTarget = "down"
	MoveWordForward   MoveTarget = "word_forward"
	MoveWordBackward  MoveTarget = "word_backward"
	MoveLineStart     MoveTarget = "line_start"
	MoveLineEnd       MoveTarget = "line_end"
	MoveDocStart      MoveTarget = "doc_start"
	MoveDocEnd        MoveTarget = "doc_end"
	MovePageUp        MoveTarget = "page_up"
	MovePageDown      MoveTarget = "page_down"
	MoveLineNumber    MoveTarget = "line_number"
	MoveFieldPrevious MoveTarget = "field_previous"
	MoveFieldNext     MoveTarget = "field_next"
)

// Count is read per target: column steps for left and right, words for
// word_forward and word_backward, pages for page_up and page_down, a 1-based
// line number for line_number, and ignored for absolute targets.
type MoveParams struct {
	Target MoveTarget `json:"target"`
	Count  int        `json:"count,omitempty"`
}

// EditOp names a buffer edit. Editing is an in-memory operation on the current
// edit target; persistence is a separate, application-driven commit path.
type EditOp string

const (
	// EditInsert inserts the bound or input text at the cursor.
	EditInsert EditOp = "insert"
	// EditReplace replaces the selection with the bound or input text.
	EditReplace EditOp = "replace_selection"
	// EditDeleteBefore deletes the character before the cursor.
	EditDeleteBefore EditOp = "delete_before"
	// EditDeleteAfter deletes the character at the cursor.
	EditDeleteAfter EditOp = "delete_after"
	// EditDeleteSelection deletes the selected range.
	EditDeleteSelection EditOp = "delete_selection"
	// EditDeleteLine deletes the cursor's whole line.
	EditDeleteLine EditOp = "delete_line"
	// EditDeleteToLineEnd deletes from the cursor to the end of the line.
	EditDeleteToLineEnd EditOp = "delete_to_line_end"
	// EditDeleteWordBefore deletes the word before the cursor.
	EditDeleteWordBefore EditOp = "delete_word_before"
	// EditNewline splits the cursor's line.
	EditNewline EditOp = "newline"
	// EditJoinLine joins the cursor's line with the next one.
	EditJoinLine EditOp = "join_line"
	EditUndo     EditOp = "undo"
	EditRedo     EditOp = "redo"
)

// TextSource says where edited text comes from: a literal in the binding, or
// the printable run carried by the current input event. The input source is
// what lets one binding serve every character typed in insert mode.
type TextSource string

const (
	TextLiteral   TextSource = "literal"
	TextFromInput TextSource = "input_text"
)

type EditParams struct {
	Op      EditOp     `json:"op"`
	Text    TextSource `json:"text,omitempty"`
	Literal string     `json:"literal,omitempty"`
	Count   int        `json:"count,omitempty"`
}

// AnchorSource names a selection endpoint. Selection therefore composes from
// the cursor, the document bounds, and the last search match without the
// binding needing to know coordinates.
type AnchorSource string

const (
	AnchorNone           AnchorSource = "none"
	AnchorCursor         AnchorSource = "cursor"
	AnchorDocStart       AnchorSource = "doc_start"
	AnchorDocEnd         AnchorSource = "doc_end"
	AnchorMatchStart     AnchorSource = "match_start"
	AnchorMatchEnd       AnchorSource = "match_end"
	AnchorSelectionStart AnchorSource = "selection_start"
	AnchorSelectionEnd   AnchorSource = "selection_end"
)

type SelectParams struct {
	From AnchorSource `json:"from"`
	To   AnchorSource `json:"to"`
}

// QuerySource says where a search pattern comes from.
type QuerySource string

const (
	// QueryPrompt reads the text the application collected in its own prompt
	// line, which is how a spec searches for a pattern typed after "/".
	QueryPrompt QuerySource = "prompt"
	// QueryLast repeats the pattern of the previous successful search, so a
	// repeated search survives leaving the prompt it was typed into.
	QueryLast QuerySource = "last"
	// QueryLiteral uses the pattern written into the binding.
	QueryLiteral QuerySource = "literal"
)

// SearchDirection covers the four ordinary search gestures; wrap variants
// continue from the other end instead of reporting "not found".
type SearchDirection string

const (
	SearchForward      SearchDirection = "forward"
	SearchBackward     SearchDirection = "backward"
	SearchForwardWrap  SearchDirection = "forward_wrap"
	SearchBackwardWrap SearchDirection = "backward_wrap"
)

// CaseMode selects literal case sensitivity. Matching is always a literal
// substring search: no regular expressions, so a pattern cannot cost more than
// the bounded scan and cannot be mistaken for code.
type CaseMode string

const (
	CaseSensitive   CaseMode = "sensitive"
	CaseInsensitive CaseMode = "insensitive"
)

type SearchParams struct {
	Query     QuerySource     `json:"query"`
	Literal   string          `json:"literal,omitempty"`
	Direction SearchDirection `json:"direction"`
	Case      CaseMode        `json:"case,omitempty"`
}

// ScrollUnit names a viewport step. Units are verb-named so a spec never needs
// a direction flag alongside a unit flag.
type ScrollUnit string

const (
	ScrollLineUp       ScrollUnit = "line_up"
	ScrollLineDown     ScrollUnit = "line_down"
	ScrollHalfPageUp   ScrollUnit = "half_page_up"
	ScrollHalfPageDown ScrollUnit = "half_page_down"
	ScrollPageUp       ScrollUnit = "page_up"
	ScrollPageDown     ScrollUnit = "page_down"
	// ScrollFirstLine and ScrollLastLine put the first or the last document row
	// on screen. They are named after the document position rather than after a
	// key, so no view has to be described by the name of a program.
	ScrollFirstLine ScrollUnit = "first_line"
	ScrollLastLine  ScrollUnit = "last_line"
)

type ScrollParams struct {
	Unit  ScrollUnit `json:"unit"`
	Count int        `json:"count,omitempty"`
}

// StatusParams sets the status line. An empty text clears it, which is how a
// spec dismisses a message without a separate action.
type StatusParams struct {
	Text string `json:"text,omitempty"`
}

// ModeParams switches to a mode the spec declares.
type ModeParams struct {
	Mode string `json:"mode"`
}

// SnapshotSource says which application-owned text an emitted semantic event
// carries back to the sandboxed handler. Events stay small by default; a save
// or a command run explicitly asks for the buffer or the collected prompt
// line.
type SnapshotSource string

const (
	SnapshotNone          SnapshotSource = "none"
	SnapshotBufferText    SnapshotSource = "buffer_text"
	SnapshotPromptText    SnapshotSource = "prompt_text"
	SnapshotSelectionText SnapshotSource = "selection_text"
)

// EventParams emits a semantic event. The name and fields are application
// data; the runtime neither interprets nor persists them.
type EventParams struct {
	Name     string            `json:"name"`
	Snapshot SnapshotSource    `json:"snapshot,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
}

// Params is the action-parameter union: exactly one member is populated, and
// the populated member is the one matching the binding's action. Keeping the
// parameters typed rather than raw JSON is what lets validation reject a
// binding that would give a primitive an argument it cannot honour.
type Params struct {
	Move   *MoveParams   `json:"move,omitempty"`
	Edit   *EditParams   `json:"edit,omitempty"`
	Select *SelectParams `json:"select,omitempty"`
	Search *SearchParams `json:"search,omitempty"`
	Scroll *ScrollParams `json:"scroll,omitempty"`
	Status *StatusParams `json:"status,omitempty"`
	Mode   *ModeParams   `json:"mode,omitempty"`
	Event  *EventParams  `json:"event,omitempty"`
}

// Binding maps one key sequence to one approved primitive action.
type Binding struct {
	Keys   Sequence
	Action string
	Params Params
}

// actionParams returns the populated parameter member for an action, or nil
// when the union does not carry it.
func (p Params) actionParams(action string) any {
	switch action {
	case domain.ActionMoveCursor:
		if p.Move != nil {
			return p.Move
		}
	case domain.ActionEditBuffer:
		if p.Edit != nil {
			return p.Edit
		}
	case domain.ActionSelectRange:
		if p.Select != nil {
			return p.Select
		}
	case domain.ActionSearchBuffer:
		if p.Search != nil {
			return p.Search
		}
	case domain.ActionScroll:
		if p.Scroll != nil {
			return p.Scroll
		}
	case domain.ActionUpdateStatus:
		if p.Status != nil {
			return p.Status
		}
	case domain.ActionModeSwitch:
		if p.Mode != nil {
			return p.Mode
		}
	case domain.ActionEmitEvent:
		if p.Event != nil {
			return p.Event
		}
	}
	return nil
}

// keyString returns the canonical spelling used in the renderer-facing view.
func (b Binding) keyString() string { return b.Keys.String() }

// validate checks the action, the parameter union, and the per-action
// argument rules, normalizing the defaulted members on the way. Spec-level
// questions (is the target mode declared?) are checked by Spec.Validate, which
// knows all modes at once.
func (b *Binding) validate() error {
	if len(b.Keys) == 0 {
		return specError("binding has no keys")
	}
	if !domain.IsValidPrimitiveAction(b.Action) {
		return specError("action %q is not an approved primitive action", b.Action)
	}
	populated := 0
	for _, member := range []bool{b.Params.Move != nil, b.Params.Edit != nil, b.Params.Select != nil,
		b.Params.Search != nil, b.Params.Scroll != nil, b.Params.Status != nil, b.Params.Mode != nil,
		b.Params.Event != nil} {
		if member {
			populated++
		}
	}
	if populated != 1 {
		return specError("binding %q must populate exactly one parameter member, got %d", b.keyString(), populated)
	}
	// A populated member that does not belong to the action is the common
	// mistake, so it gets its own message instead of a nil dereference later.
	if b.Params.actionParams(b.Action) == nil {
		return specError("binding %q action %q has no matching parameters", b.keyString(), b.Action)
	}
	if err := b.validateArgs(); err != nil {
		return err
	}
	return nil
}

func (b *Binding) validateArgs() error {
	switch b.Action {
	case domain.ActionMoveCursor:
		p := b.Params.Move
		if !isMoveTarget(p.Target) {
			return specError("binding %q: unknown move target %q", b.keyString(), p.Target)
		}
		if p.Count < 0 {
			return specError("binding %q: move count must not be negative", b.keyString())
		}
	case domain.ActionEditBuffer:
		p := b.Params.Edit
		if !isEditOp(p.Op) {
			return specError("binding %q: unknown edit op %q", b.keyString(), p.Op)
		}
		switch p.Text {
		case "":
			p.Text = TextLiteral
		case TextLiteral:
			if opUsesText(p.Op) && p.Literal == "" {
				return specError("binding %q: edit op %q needs literal text", b.keyString(), p.Op)
			}
		case TextFromInput:
			p.Literal = ""
		default:
			return specError("binding %q: unknown text source %q", b.keyString(), p.Text)
		}
		if p.Count < 0 {
			return specError("binding %q: edit count must not be negative", b.keyString())
		}
	case domain.ActionSelectRange:
		p := b.Params.Select
		if !isAnchorSource(p.From) || !isAnchorSource(p.To) {
			return specError("binding %q: unknown selection anchor", b.keyString())
		}
	case domain.ActionSearchBuffer:
		p := b.Params.Search
		switch p.Query {
		case QueryPrompt, QueryLast:
			p.Literal = ""
		case QueryLiteral:
			if p.Literal == "" {
				return specError("binding %q: literal search needs a pattern", b.keyString())
			}
		default:
			return specError("binding %q: unknown query source %q", b.keyString(), p.Query)
		}
		switch p.Direction {
		case SearchForward, SearchBackward, SearchForwardWrap, SearchBackwardWrap:
		default:
			return specError("binding %q: unknown search direction %q", b.keyString(), p.Direction)
		}
		switch p.Case {
		case "":
			p.Case = CaseSensitive
		case CaseSensitive, CaseInsensitive:
		default:
			return specError("binding %q: unknown case mode %q", b.keyString(), p.Case)
		}
	case domain.ActionScroll:
		p := b.Params.Scroll
		switch p.Unit {
		case ScrollLineUp, ScrollLineDown, ScrollHalfPageUp, ScrollHalfPageDown,
			ScrollPageUp, ScrollPageDown, ScrollFirstLine, ScrollLastLine:
		default:
			return specError("binding %q: unknown scroll unit %q", b.keyString(), p.Unit)
		}
		if p.Count < 0 {
			return specError("binding %q: scroll count must not be negative", b.keyString())
		}
	case domain.ActionUpdateStatus:
		// Empty text is meaningful (it clears the status line).
	case domain.ActionModeSwitch:
		if b.Params.Mode.Mode == "" {
			return specError("binding %q: mode switch needs a target mode", b.keyString())
		}
	case domain.ActionEmitEvent:
		p := b.Params.Event
		if p.Name == "" {
			return specError("binding %q: emitted event needs a name", b.keyString())
		}
		switch p.Snapshot {
		case "", SnapshotNone:
			p.Snapshot = SnapshotNone
		case SnapshotBufferText, SnapshotPromptText, SnapshotSelectionText:
		default:
			return specError("binding %q: unknown snapshot source %q", b.keyString(), p.Snapshot)
		}
	}
	return nil
}

func opUsesText(op EditOp) bool {
	return op == EditInsert || op == EditReplace
}

func isMoveTarget(t MoveTarget) bool {
	switch t {
	case MoveLeft, MoveRight, MoveUp, MoveDown, MoveWordForward, MoveWordBackward,
		MoveLineStart, MoveLineEnd, MoveDocStart, MoveDocEnd, MovePageUp, MovePageDown,
		MoveLineNumber, MoveFieldPrevious, MoveFieldNext:
		return true
	default:
		return false
	}
}

func isEditOp(op EditOp) bool {
	switch op {
	case EditInsert, EditReplace, EditDeleteBefore, EditDeleteAfter, EditDeleteSelection,
		EditDeleteLine, EditDeleteToLineEnd, EditDeleteWordBefore, EditNewline, EditJoinLine,
		EditUndo, EditRedo:
		return true
	default:
		return false
	}
}

func isAnchorSource(a AnchorSource) bool {
	switch a {
	case AnchorNone, AnchorCursor, AnchorDocStart, AnchorDocEnd, AnchorMatchStart,
		AnchorMatchEnd, AnchorSelectionStart, AnchorSelectionEnd:
		return true
	default:
		return false
	}
}

// boundCount resolves a declared count to at least 1 and reports it over the
// configured maximum so callers can reject it before touching the buffer.
func boundCount(count int, limits Limits, bound string) (int, error) {
	if count == 0 {
		count = 1
	}
	if exceeded(int64(count), int64(limits.MaxCount)) {
		return 0, limitExceeded(bound, int64(count), int64(limits.MaxCount))
	}
	return count, nil
}

func specError(format string, args ...any) error {
	return domain.NewValidationError(domain.CodeInvalidAppView,
		fmt.Sprintf(format, args...), nil)
}

// jsonRaw marshals a value or reports the error; used where the renderer-facing
// view carries opaque-but-validated data.
func jsonRaw(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, domain.WrapError(err, domain.CategoryInternal, domain.CodeSerializationFailed,
			"encoding interaction parameters")
	}
	return raw, nil
}

// eventFieldsSize is the bounded footprint of an event's application fields.
func eventFieldsSize(fields map[string]string) int {
	total := 0
	for key, value := range fields {
		total += len(key) + len(value)
	}
	return total
}
