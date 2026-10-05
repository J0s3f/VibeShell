package interactions

import (
	"strconv"

	"j0s.at/vibeshell/internal/domain"
)

// Cursor addresses the focus space of the current view: a buffer position in
// text-like views, a cell in table and status views, a field in forms. Columns
// count runes, not bytes, so multi-byte text and combining sequences stay
// addressable.
type Cursor struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Buffer is the editable text an interaction owns. It is in-memory only:
// persistence happens through the application's effect-proposal path, never
// from a primitive.
type Buffer struct {
	// Ref records the content reference this buffer came from, when the host
	// resolved one before starting the interaction. The primitives never
	// resolve it; they only carry it into the renderer-facing view.
	Ref    *domain.ContentRef `json:"ref,omitempty"`
	Lines  []string           `json:"lines"`
	Cursor Cursor             `json:"cursor"`
	// Anchor is the selection start; nil means no selection.
	Anchor *Cursor `json:"anchor,omitempty"`
}

// Snapshot is one undo or redo entry: the document and its cursor as they were
// before a mutating edit.
type Snapshot struct {
	Lines  []string `json:"lines"`
	Cursor Cursor   `json:"cursor"`
	Anchor *Cursor  `json:"anchor,omitempty"`
}

// Match is the last search hit, which later bindings can select from without
// repeating the pattern.
type Match struct {
	Line  int `json:"line"`
	Start int `json:"start"`
	End   int `json:"end"`
}

// Align mirrors Column.Align for cell rendering.
// Table holds cell data for table and status views. Sorting is not a primitive:
// the handler reorders rows and reports the ordering in Sort.
type Table struct {
	// Rows holds cell text; every row must have exactly one cell per column.
	Rows [][]string `json:"rows"`
	// Cursor is the focused cell.
	Cursor Cursor `json:"cursor"`
	// Anchor is the selected range's start; nil means no selection.
	Anchor *Cursor `json:"anchor,omitempty"`
	// Sort records the last applied ordering the handler reported, so the view
	// can label it.
	Sort SortState `json:"sort"`
}

// SortState names the ordering a handler applied.
type SortState struct {
	Column string    `json:"column,omitempty"`
	Order  SortOrder `json:"order,omitempty"`
}

// SortOrder is the direction of a reported sort.
type SortOrder string

const (
	SortAscending  SortOrder = "ascending"
	SortDescending SortOrder = "descending"
)

// State is the interaction state a generated application's handler owns and the
// primitives mutate. It is pure data: no clock, no randomness, no world
// handles, so a whole key flow is reproducible in a test.
type State struct {
	// Mode is the active mode; empty means the spec's default mode.
	Mode string `json:"mode,omitempty"`
	// Buffer holds the document for text-like views.
	Buffer Buffer `json:"buffer"`
	// Fields holds form values, aligned with the view's declared fields.
	Fields []string `json:"fields,omitempty"`
	// Focus is the focused form field index.
	Focus int `json:"focus,omitempty"`
	// Table holds cell data for table and status views.
	Table Table `json:"table"`
	// PromptText is text the application collected in its own prompt or command
	// line. Prompt-sourced search and command events read it, which keeps text
	// entry in the handler where the application's own prompt belongs.
	PromptText string `json:"prompt_text,omitempty"`
	// LastQuery is the pattern of the most recent successful search, which is
	// what a repeated search repeats.
	LastQuery string `json:"last_query,omitempty"`
	// Match is the last search hit.
	Match *Match `json:"match,omitempty"`
	// Viewport is the visible window, in document coordinates.
	Viewport domain.Viewport `json:"viewport"`
	// Status is the status line.
	Status string `json:"status,omitempty"`
	// Dirty marks unsaved changes; the app decides what that means for commit.
	Dirty bool `json:"dirty,omitempty"`
	// Exited asks the host to return the user to a fresh shell.
	Exited   bool `json:"exited,omitempty"`
	ExitCode int  `json:"exit_code,omitempty"`
	// Undo and Redo are bounded by Limits.MaxUndoDepth.
	Undo []Snapshot `json:"undo,omitempty"`
	Redo []Snapshot `json:"redo,omitempty"`
}

// ScreenMetrics is the terminal size the layout must fit. A resize is delivered
// by the host as a new ScreenMetrics value; nothing here observes a terminal.
type ScreenMetrics struct {
	Rows    int `json:"rows"`
	Columns int `json:"columns"`
}

// Validate checks that the screen can show something at all.
func (s ScreenMetrics) Validate() error {
	if s.Rows <= 0 || s.Columns <= 0 {
		return domain.NewValidationError(domain.CodeInvalidInput,
			"screen metrics must be positive", map[string]string{
				"rows": strconv.Itoa(s.Rows), "columns": strconv.Itoa(s.Columns),
			})
	}
	return nil
}

// contentRows returns how many document rows are available for display once a
// status line is reserved.
func (s ScreenMetrics) contentRows(statusVisible bool) int {
	rows := s.Rows
	if statusVisible {
		rows--
	}
	if rows < 1 {
		return 1
	}
	return rows
}

func (s State) clone() State {
	out := s
	if s.Buffer.Anchor != nil {
		anchor := *s.Buffer.Anchor
		out.Buffer.Anchor = &anchor
	}
	if s.Match != nil {
		match := *s.Match
		out.Match = &match
	}
	out.Buffer.Lines = append([]string(nil), s.Buffer.Lines...)
	out.Fields = append([]string(nil), s.Fields...)
	out.Table.Rows = cloneRows(s.Table.Rows)
	out.Undo = append([]Snapshot(nil), s.Undo...)
	out.Redo = append([]Snapshot(nil), s.Redo...)
	return out
}

func cloneRows(rows [][]string) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = append([]string(nil), row...)
	}
	return out
}

func snapshotOf(b Buffer) Snapshot {
	snap := Snapshot{Lines: append([]string(nil), b.Lines...), Cursor: b.Cursor}
	if b.Anchor != nil {
		anchor := *b.Anchor
		snap.Anchor = &anchor
	}
	return snap
}
