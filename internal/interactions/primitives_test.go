package interactions_test

import (
	"encoding/json"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/interactions"
)

// editorSpecJSON is a minimal, valid editor spec used by the primitive tests. It
// is declared inline rather than loaded from a fixture so a test can change one
// aspect of a spec without touching a checked-in example.
const editorSpecJSON = `{
  "view": {
    "mode": "editor",
    "gutter": true,
    "status_line": "file {dirty} -- {mode} -- {line}/{lines}"
  },
  "mode": "normal",
  "default_mode": "normal",
  "modes": [
    {
      "name": "normal",
      "column_policy": "within_line",
      "mode_targets": ["insert"],
      "bindings": [
        { "keys": "h", "action": "move_cursor", "params": { "move": { "target": "left" } } },
        { "keys": "j", "action": "move_cursor", "params": { "move": { "target": "down" } } },
        { "keys": "l", "action": "move_cursor", "params": { "move": { "target": "right" } } },
        { "keys": "G", "action": "move_cursor", "params": { "move": { "target": "doc_end" } } },
        { "keys": "3 G", "action": "move_cursor", "params": { "move": { "target": "line_number", "count": 3 } } },
        { "keys": "w", "action": "move_cursor", "params": { "move": { "target": "word_forward" } } },
        { "keys": "x", "action": "edit_buffer", "params": { "edit": { "op": "delete_after" } } },
        { "keys": "d d", "action": "edit_buffer", "params": { "edit": { "op": "delete_line" } } },
        { "keys": "u", "action": "edit_buffer", "params": { "edit": { "op": "undo" } } },
        { "keys": "ctrl-r", "action": "edit_buffer", "params": { "edit": { "op": "redo" } } },
        { "keys": "v", "action": "select_range", "params": { "select": { "from": "cursor", "to": "cursor" } } },
        { "keys": "escape", "action": "select_range", "params": { "select": { "from": "none", "to": "none" } } },
        { "keys": "i", "action": "mode_switch", "params": { "mode": { "mode": "insert" } } },
        { "keys": "n", "action": "search_buffer", "params": { "search": { "query": "prompt", "direction": "forward_wrap", "case": "insensitive" } } },
        { "keys": "y", "action": "emit_event", "params": { "event": { "name": "copy", "snapshot": "selection_text" } } }
      ]
    },
    {
      "name": "insert",
      "inherit": false,
      "column_policy": "allow_end",
      "mode_targets": ["normal"],
      "bindings": [
        { "keys": "text", "action": "edit_buffer", "params": { "edit": { "op": "insert", "text": "input_text" } } },
        { "keys": "left", "action": "move_cursor", "params": { "move": { "target": "left" } } },
        { "keys": "right", "action": "move_cursor", "params": { "move": { "target": "right" } } },
        { "keys": "escape", "action": "mode_switch", "params": { "mode": { "mode": "normal" } } }
      ]
    }
  ]
}`

func mustSpec(t *testing.T, raw string) interactions.Spec {
	t.Helper()
	spec, err := interactions.ParseSpec([]byte(raw))
	if err != nil {
		t.Fatalf("parsing spec: %v", err)
	}
	return spec
}

func threeLineState() interactions.State {
	return interactions.State{
		Buffer: interactions.Buffer{
			Lines:  []string{"alpha bravo", "charlie", "delta echo"},
			Cursor: interactions.Cursor{},
		},
	}
}

func newEditor(t *testing.T, state interactions.State) *interactions.Machine {
	t.Helper()
	machine, err := interactions.New(mustSpec(t, editorSpecJSON), state,
		interactions.ScreenMetrics{Rows: 10, Columns: 40}, interactions.Limits{})
	if err != nil {
		t.Fatalf("building machine: %v", err)
	}
	return machine
}

func press(t *testing.T, machine *interactions.Machine, keys, text string) interactions.Outcome {
	t.Helper()
	seq, err := interactions.ParseSequence(keys)
	if err != nil {
		t.Fatalf("test key %q: %v", keys, err)
	}
	outcome, err := machine.Apply(interactions.Input{Chords: seq, Text: text})
	if err != nil {
		t.Fatalf("applying %q: %v", keys, err)
	}
	return outcome
}

func TestParseSpecRejectsInvalidDeclarations(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{
			name:    "unknown view mode",
			spec:    strings.Replace(editorSpecJSON, `"mode": "editor"`, `"mode": "hologram"`, 1),
			wantErr: "unknown view mode",
		},
		{
			name:    "action outside the approved set",
			spec:    strings.Replace(editorSpecJSON, `"action": "move_cursor"`, `"action": "spawn_process"`, 1),
			wantErr: "not an approved primitive action",
		},
		{
			name:    "unpopulated parameter union",
			spec:    strings.Replace(editorSpecJSON, `"params": { "move": { "target": "left" } }`, `"params": {}`, 1),
			wantErr: "exactly one parameter member",
		},
		{
			name: "parameters for a different action",
			spec: strings.Replace(editorSpecJSON,
				`{ "keys": "w", "action": "move_cursor", "params": { "move": { "target": "word_forward" } } }`,
				`{ "keys": "w", "action": "move_cursor", "params": { "edit": { "op": "undo" } } }`, 1),
			wantErr: "no matching parameters",
		},
		{
			name:    "duplicate key in one mode",
			spec:    strings.Replace(editorSpecJSON, `{ "keys": "l", "action": "move_cursor", "params": { "move": { "target": "right" } } },`, `{ "keys": "h", "action": "move_cursor", "params": { "move": { "target": "right" } } },`, 1),
			wantErr: "twice",
		},
		{
			name:    "mode switch to an undeclared mode",
			spec:    strings.Replace(editorSpecJSON, `{ "mode": "insert" } }`, `{ "mode": "replace" } }`, 1),
			wantErr: "undeclared mode",
		},
		{
			name:    "mode switch outside the declared targets",
			spec:    strings.Replace(editorSpecJSON, `"mode_targets": ["insert"]`, `"mode_targets": []`, 1),
			wantErr: "may not switch",
		},
		{
			name:    "unknown status placeholder",
			spec:    strings.Replace(editorSpecJSON, `"file {dirty} -- {mode} -- {line}/{lines}"`, `"file {hostname}"`, 1),
			wantErr: "not one of the declared fields",
		},
		{
			name:    "unknown JSON field",
			spec:    strings.Replace(editorSpecJSON, `"gutter": true,`, `"guttr": true,`, 1),
			wantErr: "not valid JSON",
		},
		{
			name:    "table view requires columns",
			spec:    strings.Replace(editorSpecJSON, `"mode": "editor"`, `"mode": "table"`, 1),
			wantErr: "must not declare a gutter",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := interactions.ParseSpec([]byte(testCase.spec))
			if err == nil {
				t.Fatalf("ParseSpec accepted an invalid spec")
			}
			if !strings.Contains(errorText(err), testCase.wantErr) {
				t.Fatalf("ParseSpec error = %q, want it to mention %q", errorText(err), testCase.wantErr)
			}
			if !domain.IsValidationError(err) {
				t.Fatalf("ParseSpec error category = %v, want validation", domain.GetErrorCategory(err))
			}
		})
	}
}

func TestViewModeDeclaresItsOwnData(t *testing.T) {
	tableSpec := `{
      "view": { "mode": "table", "columns": [{ "title": "PID" }], "fields": [{ "name": "x" }] },
      "default_mode": "browse",
      "modes": [{ "name": "browse", "bindings": [] }]
    }`
	if _, err := interactions.ParseSpec([]byte(tableSpec)); err == nil {
		t.Fatal("a table view with form fields must be rejected")
	}
	formSpec := `{
      "view": { "mode": "form", "fields": [{ "name": "user", "editable": true }] },
      "default_mode": "entry",
      "modes": [{ "name": "entry", "bindings": [] }]
    }`
	if _, err := interactions.ParseSpec([]byte(formSpec)); err != nil {
		t.Fatalf("a valid form spec was rejected: %v", err)
	}
}

func TestMachineRejectsBindingsThatCannotApplyToTheView(t *testing.T) {
	spec := `{
      "view": { "mode": "table", "columns": [{ "title": "PID" }] },
      "default_mode": "browse",
      "modes": [{ "name": "browse", "bindings": [
        { "keys": "x", "action": "edit_buffer", "params": { "edit": { "op": "delete_after" } } }
      ]}]
    }`
	parsed := mustSpec(t, spec)
	_, err := interactions.New(parsed, interactions.State{},
		interactions.ScreenMetrics{Rows: 6, Columns: 20}, interactions.Limits{})
	if err == nil || !strings.Contains(errorText(err), "no editable target") {
		t.Fatalf("New error = %v, want a rejected table-cell edit", err)
	}
}

func TestBindingsDispatchToApprovedActions(t *testing.T) {
	machine := newEditor(t, threeLineState())

	if outcome := press(t, machine, "l", ""); outcome.Kind != interactions.OutcomeApplied {
		t.Fatalf("move outcome = %v, want applied", outcome.Kind)
	}
	if got := machine.State().Buffer.Cursor; got != (interactions.Cursor{Line: 0, Column: 1}) {
		t.Fatalf("cursor after move right = %+v, want column 1", got)
	}

	press(t, machine, "x", "")
	if got := machine.State().Buffer.Lines[0]; got != "apha bravo" {
		t.Fatalf("line after delete = %q, want %q", got, "apha bravo")
	}
	if !machine.State().Dirty {
		t.Fatal("an edit must mark the buffer dirty")
	}

	press(t, machine, "u", "")
	if got := machine.State().Buffer.Lines[0]; got != "alpha bravo" {
		t.Fatalf("line after undo = %q, want %q", got, "alpha bravo")
	}

	press(t, machine, "ctrl-r", "")
	if got := machine.State().Buffer.Lines[0]; got != "apha bravo" {
		t.Fatalf("line after redo = %q, want %q", got, "apha bravo")
	}
}

func TestUndoAndRedoReportWhenTheStackIsEmpty(t *testing.T) {
	machine := newEditor(t, threeLineState())
	press(t, machine, "u", "")
	if got := machine.State().Status; got != "nothing to undo" {
		t.Fatalf("status after empty undo = %q, want the bounded primitive note", got)
	}
	press(t, machine, "ctrl-r", "")
	if got := machine.State().Status; got != "nothing to redo" {
		t.Fatalf("status after empty redo = %q, want the bounded primitive note", got)
	}
}

func TestMultiKeyCommandWaitsForTheRest(t *testing.T) {
	machine := newEditor(t, threeLineState())
	if outcome := press(t, machine, "d", ""); outcome.Kind != interactions.OutcomePending {
		t.Fatalf("first chord outcome = %v, want pending", outcome.Kind)
	}
	if len(machine.State().Buffer.Lines) != 3 {
		t.Fatal("a pending command must not change the document")
	}
	if outcome := press(t, machine, "d", ""); outcome.Kind != interactions.OutcomeApplied {
		t.Fatalf("second chord outcome = %v, want applied", outcome.Kind)
	}
	if got := machine.State().Buffer.Lines; len(got) != 2 || got[0] != "charlie" {
		t.Fatalf("lines after delete_line = %v, want the first line removed", got)
	}
}

func TestUnboundKeyIsReportedForTheHandlerAndExtensionPath(t *testing.T) {
	machine := newEditor(t, threeLineState())
	outcome := press(t, machine, "Z", "")
	if outcome.Kind != interactions.OutcomeUnknown {
		t.Fatalf("outcome = %v, want unknown", outcome.Kind)
	}
	if outcome.Chord != "Z" {
		t.Fatalf("unbound chord = %q, want %q", outcome.Chord, "Z")
	}
	view, err := machine.View()
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	request, err := interactions.ExtensionRequest("req-1",
		interactions.Input{Chords: mustSequence(t, "Z")}, view, machine.State().Status,
		interactions.DefaultLimits())
	if err != nil {
		t.Fatalf("ExtensionRequest: %v", err)
	}
	if request.RequestID != "req-1" {
		t.Fatalf("request id = %q", request.RequestID)
	}
	if request.MaxTokens <= 0 || request.TimeoutMs <= 0 {
		t.Fatalf("extension request is not bounded: %+v", request)
	}
	if !strings.Contains(request.Prompt, "keys=Z") {
		t.Fatalf("extension prompt does not name the key: %q", request.Prompt)
	}
	if strings.ContainsAny(request.Prompt, "\x1b\x07") {
		t.Fatalf("extension prompt carries control bytes: %q", request.Prompt)
	}
}

func mustSequence(t *testing.T, keys string) interactions.Sequence {
	t.Helper()
	seq, err := interactions.ParseSequence(keys)
	if err != nil {
		t.Fatalf("parsing %q: %v", keys, err)
	}
	return seq
}

func TestTextBindingInsertsTypedTextWithoutPerCharacterBindings(t *testing.T) {
	machine := newEditor(t, threeLineState())
	press(t, machine, "i", "")
	if machine.State().Mode != "insert" {
		t.Fatalf("mode after i = %q, want insert", machine.State().Mode)
	}
	press(t, machine, "text", "hello")
	if got := machine.State().Buffer.Lines[0]; got != "helloalpha bravo" {
		t.Fatalf("line after insert = %q, want the typed run inserted", got)
	}
	press(t, machine, "escape", "")
	if machine.State().Mode != "normal" {
		t.Fatalf("mode after escape = %q, want normal", machine.State().Mode)
	}
	// Returning to the normal mode clamps the cursor onto an existing character.
	if got := machine.State().Buffer.Cursor.Column; got != 5 {
		t.Fatalf("cursor column after escape = %d, want 5 clamped under the normal policy", got)
	}
}

func TestSelectionAnchorsAndClearing(t *testing.T) {
	machine := newEditor(t, threeLineState())
	press(t, machine, "v", "")
	if machine.State().Buffer.Anchor == nil {
		t.Fatal("select_range must set an anchor")
	}
	press(t, machine, "j", "")
	outcome := press(t, machine, "y", "")
	if outcome.Kind != interactions.OutcomeApplied {
		t.Fatalf("copy outcome = %v", outcome.Kind)
	}
	if len(outcome.Events) != 1 {
		t.Fatalf("copy produced %d events, want 1", len(outcome.Events))
	}
	if got := outcome.Events[0].Selection; got != "alpha bravo\n" {
		t.Fatalf("selection snapshot = %q, want the selected lines", got)
	}
	press(t, machine, "escape", "")
	if machine.State().Buffer.Anchor != nil {
		t.Fatal("escape must clear the selection")
	}
}

func TestPromptSourcedSearchAndMissingPattern(t *testing.T) {
	machine := newEditor(t, threeLineState())
	machine = machineWithPrompt(t, machine, "bravo")
	press(t, machine, "n", "")
	match := machine.State().Match
	if match == nil || match.Line != 0 {
		t.Fatalf("search match = %+v, want line 0", match)
	}
	machine = machineWithPrompt(t, machine, "nothing-matches-this")
	press(t, machine, "n", "")
	if got := machine.State().Status; got != "pattern not found" {
		t.Fatalf("status after a missing pattern = %q", got)
	}
	if machine.State().Match != nil {
		t.Fatal("a failed search must not record a match")
	}
}

// machineWithPrompt installs state whose prompt line holds the given text, the
// way an application handler would after collecting a search pattern.
func machineWithPrompt(t *testing.T, machine *interactions.Machine, prompt string) *interactions.Machine {
	t.Helper()
	state := machine.State()
	state.PromptText = prompt
	if err := machine.Replace(machine.Spec(), state); err != nil {
		t.Fatalf("installing prompt text: %v", err)
	}
	return machine
}

func TestResourceLimitsRefuseOversizedInput(t *testing.T) {
	spec := mustSpec(t, editorSpecJSON)
	limits := interactions.Limits{MaxBufferBytes: 24, MaxLineBytes: 16, MaxUndoDepth: 2, MaxCount: 5}
	machine, err := interactions.New(spec, interactions.State{
		Buffer: interactions.Buffer{Lines: []string{"alpha bravo"}},
	}, interactions.ScreenMetrics{Rows: 8, Columns: 30}, limits)
	if err != nil {
		t.Fatalf("building machine within limits: %v", err)
	}
	press(t, machine, "i", "")
	_, err = machine.Apply(interactions.Input{
		Chords: mustSequence(t, "text"),
		Text:   strings.Repeat("z", 32),
	})
	if err == nil {
		t.Fatal("inserting past max_buffer_bytes must fail")
	}
	if !domain.IsLimitError(err) {
		t.Fatalf("error category = %v, want limit", domain.GetErrorCategory(err))
	}
	if got := domain.GetErrorCode(err); got != domain.CodeOutputTooLarge {
		t.Fatalf("error code = %q, want %q", got, domain.CodeOutputTooLarge)
	}
	// The refused edit must leave the document untouched.
	if got := machine.State().Buffer.Lines[0]; got != "alpha bravo" {
		t.Fatalf("document after a refused edit = %q, want it unchanged", got)
	}
}

func TestCountAboveTheConfiguredBoundIsRefused(t *testing.T) {
	spec := mustSpec(t, editorSpecJSON)
	machine, err := interactions.New(spec, threeLineState(),
		interactions.ScreenMetrics{Rows: 8, Columns: 30}, interactions.Limits{MaxCount: 2})
	if err != nil {
		t.Fatalf("building machine: %v", err)
	}
	_, err = machine.Apply(interactions.Input{Chords: mustSequence(t, "3 G")})
	if err == nil || !domain.IsLimitError(err) {
		t.Fatalf("jumping past max_count error = %v, want a limit error", err)
	}
}

func TestSearchStepBoundIsEnforced(t *testing.T) {
	spec := mustSpec(t, editorSpecJSON)
	lines := make([]string, 64)
	for i := range lines {
		lines[i] = "line"
	}
	machine, err := interactions.New(spec, interactions.State{
		Buffer: interactions.Buffer{Lines: lines},
	}, interactions.ScreenMetrics{Rows: 8, Columns: 30}, interactions.Limits{MaxSearchSteps: 4})
	if err != nil {
		t.Fatalf("building machine: %v", err)
	}
	state := machine.State()
	state.PromptText = "absent"
	if err := machine.Replace(machine.Spec(), state); err != nil {
		t.Fatalf("installing prompt: %v", err)
	}
	_, err = machine.Apply(interactions.Input{Chords: mustSequence(t, "n")})
	if err == nil || !domain.IsLimitError(err) {
		t.Fatalf("search past max_search_steps error = %v, want a limit error", err)
	}
}

func TestViewPassesDomainValidationAndCarriesTheBindingTable(t *testing.T) {
	machine := newEditor(t, threeLineState())
	view, err := machine.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if err := domain.ValidateView(view); err != nil {
		t.Fatalf("ValidateView: %v", err)
	}
	if view.Mode != domain.AppViewModeEditor {
		t.Fatalf("view mode = %q, want editor", view.Mode)
	}
	if len(view.KeyBindings) == 0 {
		t.Fatal("the renderer-facing view must publish the binding table")
	}
	var params interactions.Params
	if err := json.Unmarshal(view.KeyBindings[0].Params, &params); err != nil {
		t.Fatalf("binding params are not a valid Params union: %v", err)
	}
	if len(view.Metadata) == 0 {
		t.Fatal("the view must carry its declarative metadata")
	}
}

func TestResizeReprojectsWithoutLosingState(t *testing.T) {
	machine := newEditor(t, threeLineState())
	press(t, machine, "G", "")
	if err := machine.SetScreen(interactions.ScreenMetrics{Rows: 5, Columns: 20}); err != nil {
		t.Fatalf("SetScreen: %v", err)
	}
	frame, err := machine.Frame()
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if len(frame.Rows) > 4 {
		t.Fatalf("frame has %d rows, want at most 4 content rows on a 5-row screen", len(frame.Rows))
	}
	if got := machine.State().Buffer.Cursor.Line; got != 2 {
		t.Fatalf("cursor line after resize = %d, want the document position kept", got)
	}
	if frame.Cursor == nil {
		t.Fatal("the resized frame must still locate the cursor")
	}
}

func TestFrameNeverEmitsControlSequences(t *testing.T) {
	hostile := interactions.State{
		Buffer: interactions.Buffer{
			Lines: []string{
				"\x1b[31mred label\x07",
				"\x1b]8;;https://evil.invalid\x1b\\click\x1b]8;;\x1b\\",
			},
		},
		Status: "\x1b]0;window title\x07",
	}
	machine := newEditor(t, hostile)
	frame, err := machine.Frame()
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	for _, row := range append(frame.Rows, frame.Status) {
		for _, r := range row {
			if r < 0x20 && r != '\t' || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("frame row %q carries control byte U+%04X", row, r)
			}
		}
	}
	if !strings.Contains(frame.Text(), "�") {
		t.Fatalf("sanitized frame does not show the replacement marker: %q", frame.Text())
	}
	if err := frame.Validate(interactions.DefaultLimits()); err != nil {
		t.Fatalf("Frame.Validate: %v", err)
	}
}

func TestSanitizeDataKeepsTabsAndNeutralizesControls(t *testing.T) {
	got := interactions.SanitizeData("a\tb\x1b]0;x\x07c\u009bd")
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) || strings.ContainsRune(got, 0x9b) {
		t.Fatalf("SanitizeData left a control byte: %q", got)
	}
	if !strings.Contains(got, "a\tb") {
		t.Fatalf("SanitizeData must keep tabs for the renderer to expand: %q", got)
	}
}

func TestFormViewFocusAndEditing(t *testing.T) {
	spec := mustSpec(t, `{
      "view": {
        "mode": "form",
        "status_line": "login {dirty}",
        "fields": [
          { "name": "user", "label": "User", "editable": true },
          { "name": "host", "label": "Host", "editable": true },
          { "name": "hint", "label": "Hint" }
        ]
      },
      "default_mode": "entry",
      "modes": [{ "name": "entry", "bindings": [
        { "keys": "down", "action": "move_cursor", "params": { "move": { "target": "field_next" } } },
        { "keys": "up", "action": "move_cursor", "params": { "move": { "target": "field_previous" } } },
        { "keys": "text", "action": "edit_buffer", "params": { "edit": { "op": "insert", "text": "input_text" } } },
        { "keys": "enter", "action": "emit_event", "params": { "event": { "name": "submit" } } }
      ]}]
    }`)
	machine, err := interactions.New(spec, interactions.State{Fields: []string{"micro", ""}},
		interactions.ScreenMetrics{Rows: 8, Columns: 40}, interactions.Limits{})
	if err != nil {
		t.Fatalf("building form machine: %v", err)
	}
	press(t, machine, "text", "root")
	press(t, machine, "down", "")
	press(t, machine, "text", "vibes.internal")
	state := machine.State()
	if state.Fields[0] != "rootmicro" || state.Fields[1] != "vibes.internal" {
		t.Fatalf("form values = %v, want both fields filled", state.Fields)
	}
	if state.Focus != 1 {
		t.Fatalf("focus = %d, want the second field", state.Focus)
	}
	frame, err := machine.Frame()
	if err != nil {
		t.Fatalf("form frame: %v", err)
	}
	joined := strings.Join(frame.Rows, "|")
	if !strings.Contains(joined, "User") || !strings.Contains(joined, "Hint") {
		t.Fatalf("form frame does not show every field: %q", joined)
	}
	if frame.Cursor == nil {
		t.Fatal("an editable focused field must place the cursor")
	}
}

func TestExitedInteractionRejectsFurtherInput(t *testing.T) {
	machine := newEditor(t, threeLineState())
	state := machine.State()
	state.Exited = true
	if err := machine.Replace(machine.Spec(), state); err != nil {
		t.Fatalf("marking exited: %v", err)
	}
	outcome := press(t, machine, "l", "")
	if outcome.Kind != interactions.OutcomeExited {
		t.Fatalf("outcome after exit = %v, want exited", outcome.Kind)
	}
}
