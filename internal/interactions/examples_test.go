package interactions_test

import (
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/interactions"
)

// The three acceptance examples below are driven entirely through the public
// primitive API and their declarative fixtures. Nothing here knows which
// program an example imitates: the flows only press keys and assert what the
// interaction did.

func TestEditorExampleAcceptanceFlow(t *testing.T) {
	example := loadExample(t, "editor")
	h := newHost(t, example)

	// Normal mode starts on the first line with no changes saved yet.
	if got := h.machine.State().Mode; got != "normal" {
		t.Fatalf("initial mode = %q, want normal", got)
	}
	if !strings.Contains(h.status(), "notes.txt") {
		t.Fatalf("status line does not name the file: %q", h.status())
	}

	// Text entry, then back to normal mode with the dirty marker showing.
	h.press("i", "")
	if got := h.machine.State().Mode; got != "insert" {
		t.Fatalf("mode after i = %q, want insert", got)
	}
	h.press("text", "A ")
	h.press("text", "bravo")
	h.press("escape", "")
	if got := h.machine.State().Buffer.Lines[0]; got != "A bravoalpha bravo" {
		t.Fatalf("line after typing = %q", got)
	}
	if !strings.Contains(h.status(), "[+]") {
		t.Fatalf("status line lacks the dirty marker: %q", h.status())
	}

	// Movement, including a count-prefixed jump and a word jump.
	h.press("j", "")
	if got := h.machine.State().Buffer.Cursor.Line; got != 1 {
		t.Fatalf("line after j = %d, want 1", got)
	}
	h.press("w", "")
	if got := h.machine.State().Buffer.Cursor; got != (interactions.Cursor{Line: 2, Column: 5}) {
		t.Fatalf("cursor after w = %+v, want the next word (line 2, column 5)", got)
	}
	h.press("3 G", "")
	if got := h.machine.State().Buffer.Cursor.Line; got != 2 {
		t.Fatalf("line after 3G = %d, want 2", got)
	}
	h.press("g g", "")
	if got := h.machine.State().Buffer.Cursor.Line; got != 0 {
		t.Fatalf("line after gg = %d, want 0", got)
	}

	// Delete, then undo and redo.
	h.press("x", "")
	if got := h.machine.State().Buffer.Lines[0]; !strings.HasPrefix(got, " bravo") {
		t.Fatalf("line after x = %q", got)
	}
	h.press("u", "")
	if got := h.machine.State().Buffer.Lines[0]; got != "A bravoalpha bravo" {
		t.Fatalf("line after u = %q", got)
	}
	h.press("ctrl-r", "")
	if got := h.machine.State().Buffer.Lines[0]; !strings.HasPrefix(got, " bravo") {
		t.Fatalf("line after redo = %q", got)
	}
	h.press("u", "")

	// Search: / is a semantic event, the pattern is handler-owned text, and the
	// search itself is the primitive.
	h.press("/", "")
	if got := h.machine.State().Mode; got != "search" {
		t.Fatalf("mode after / = %q, want search", got)
	}
	for _, key := range []string{"c", "h"} {
		if kind := h.press(key, ""); kind != interactions.OutcomeUnknown {
			t.Fatalf("typing %q in the search prompt = %v, want the handler path", key, kind)
		}
	}
	if got := h.machine.State().PromptText; got != "ch" {
		t.Fatalf("collected pattern = %q, want %q", got, "ch")
	}
	h.press("enter", "")
	match := h.machine.State().Match
	if match == nil || match.Line != 1 || h.machine.State().Buffer.Cursor.Line != 1 {
		t.Fatalf("search match = %+v, want line 1", match)
	}
	// The search prompt stays open until the mode is left, which is the spec's
	// choice: only the keys it binds can leave a mode.
	h.press("escape", "")
	if got := h.machine.State().Mode; got != "normal" {
		t.Fatalf("mode after leaving the search prompt = %q, want normal", got)
	}

	// A dirty buffer may not be abandoned without the force command.
	h.press(":", "")
	for _, key := range []string{"q", "enter"} {
		h.press(key, "")
	}
	if h.machine.State().Exited {
		t.Fatal(":q on a dirty buffer must not exit")
	}
	if got := h.machine.State().Status; !strings.Contains(got, "No write since last change") {
		t.Fatalf("status after refusing to quit = %q", got)
	}

	// :w proposes a world mutation; the primitive never writes anything itself.
	h.press(":", "")
	h.press("w", "")
	h.press("enter", "")
	if h.machine.State().Exited {
		t.Fatal(":w must not exit the interaction")
	}
	if h.machine.State().Dirty {
		t.Fatal(":w must clear the dirty marker")
	}
	effects := h.sandbox.Effects
	if len(effects) != 1 {
		t.Fatalf("save produced %d effects, want 1", len(effects))
	}
	if got := effects[0].Mutation.Type; got != domain.MutationUpdate {
		t.Fatalf("save mutation type = %v, want an update", got)
	}
	if effects[0].Mutation.ExpectedRev == 0 {
		t.Fatal("a save must carry the expected revision so a concurrent edit conflicts")
	}
	if !effects[0].Sync {
		t.Fatal("a save must be committed before the interaction continues")
	}

	// :wq writes and leaves.
	h.press(":", "")
	h.press("w", "")
	h.press("q", "")
	h.press("enter", "")
	if !h.machine.State().Exited || h.machine.State().ExitCode != 0 {
		t.Fatalf("state after :wq = %+v, want exited with code 0", h.machine.State())
	}
	if len(h.sandbox.Effects) != 2 {
		t.Fatalf(":wq produced %d effects, want 2 in total", len(h.sandbox.Effects))
	}

	// Keys after the interaction asked to leave are refused, not reinterpreted.
	outcome, err := h.machine.Apply(mustInput(t, "l"))
	if err != nil {
		t.Fatalf("applying a key after exit: %v", err)
	}
	if outcome.Kind != interactions.OutcomeExited {
		t.Fatalf("outcome after exit = %v, want exited", outcome.Kind)
	}
}

func TestEditorExampleForceQuitWithoutSaving(t *testing.T) {
	h := newHost(t, loadExample(t, "editor"))
	h.press("i", "")
	h.press("text", "scratch")
	h.press("escape", "")
	h.press(":", "")
	h.press("q", "")
	h.press("!", "")
	h.press("enter", "")
	if !h.machine.State().Exited {
		t.Fatal(":q! must leave a dirty buffer")
	}
	if len(h.sandbox.Effects) != 0 {
		t.Fatalf(":q! produced %d effects, want none", len(h.sandbox.Effects))
	}
}

func TestEditorExampleSearchThenNextAndPrevious(t *testing.T) {
	h := newHost(t, loadExample(t, "editor"))
	// Start on the second occurrence so that a forward search wraps to the first
	// and the repeat walks on from there.
	h.press("G", "")
	h.press("/", "")
	for _, key := range []string{"l"} {
		h.press(key, "")
	}
	h.press("enter", "")
	first := h.machine.State().Match
	if first == nil || first.Line != 0 {
		t.Fatalf("first match = %+v, want line 0", first)
	}
	h.press("escape", "")
	h.press("n", "")
	if got := h.machine.State().Match; got == nil || got.Line != 1 {
		t.Fatalf("next match = %+v, want line 1", got)
	}
	h.press("N", "")
	if got := h.machine.State().Match; got == nil || got.Line != 0 {
		t.Fatalf("previous match = %+v, want line 0", got)
	}
	if got := h.status(); !strings.Contains(got, "1/3") {
		t.Fatalf("status line does not report the position: %q", got)
	}
}

func TestEditorExampleSelectionAndCopyEvent(t *testing.T) {
	h := newHost(t, loadExample(t, "editor"))
	h.press("v", "")
	h.press("j", "")
	outcome := h.pressOutcome("y", "")
	if len(outcome.Events) != 1 {
		t.Fatalf("y produced %d events, want 1", len(outcome.Events))
	}
	event := outcome.Events[0]
	if event.Name != "copy_selection" {
		t.Fatalf("event name = %q, want copy_selection", event.Name)
	}
	if event.Selection != "alpha bravo\n" {
		t.Fatalf("selection carried to the handler = %q", event.Selection)
	}
	if !strings.Contains(h.status(), "selection copied") {
		t.Fatalf("status after copying = %q", h.status())
	}
}

func TestPagerExampleAcceptanceFlow(t *testing.T) {
	example := loadExample(t, "pager")
	h := newHost(t, example)

	// The pager shows the exact referenced content and nothing else.
	frame := h.frame()
	if len(frame.Rows) == 0 || !strings.HasPrefix(frame.Rows[0], "boot: simulated kernel") {
		t.Fatalf("first frame row = %q, want the referenced log content", frame.Rows[0])
	}
	if frame.Status == "" {
		t.Fatal("the pager must report its position on the status line")
	}

	// Paging and half paging never leave the document.
	h.press("space", "")
	if got := h.machine.State().Viewport.Top; got == 0 {
		t.Fatal("a page down must move the viewport")
	}
	h.press("u", "")
	if got := h.machine.State().Viewport.Top; got == 0 {
		t.Fatal("a half page up must move the viewport back")
	}
	h.press("b", "")
	if got := h.machine.State().Viewport.Top; got != 0 {
		t.Fatalf("page up from the top = %d, want 0", got)
	}

	// Top and bottom clamp at the document bounds.
	h.press("G", "")
	frame = h.frame()
	if !strings.HasPrefix(frame.Rows[len(frame.Rows)-1], "shutdown:") {
		t.Fatalf("last row at the bottom = %q, want the final log line", frame.Rows[len(frame.Rows)-1])
	}
	if got := h.machine.State().Viewport.Top; got != 15-len(frame.Rows) {
		t.Fatalf("viewport top at the bottom = %d, want %d", got, 15-len(frame.Rows))
	}
	h.press("g", "")
	if got := h.machine.State().Viewport.Top; got != 0 {
		t.Fatalf("viewport top after g = %d, want 0", got)
	}

	// A fixed status message is a primitive, not handler logic.
	h.press("=", "")
	if got := h.status(); !strings.Contains(got, "(END)") {
		t.Fatalf("status after = = %q, want the declared message", got)
	}

	// Searching scrolls the match into view from wherever the user was.
	h.press("G", "")
	if got := h.machine.State().Viewport.Top; got == 0 {
		t.Fatal("the pager must start this part at the end of the document")
	}
	h.press("/", "")
	for _, key := range []string{"b", "o", "o", "t"} {
		h.press(key, "")
	}
	h.press("enter", "")
	if got := h.machine.State().Match; got == nil || got.Line != 0 {
		t.Fatalf("match = %+v, want line 0", got)
	}
	if got := h.machine.State().Viewport.Top; got != 0 {
		t.Fatalf("viewport top after searching backwards = %d, want 0", got)
	}
	h.press("escape", "")

	// Resize reprojects the same content.
	before := strings.Join(h.frame().Rows, "\n")
	if err := h.machine.SetScreen(interactions.ScreenMetrics{Rows: 5, Columns: 30}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	after := h.frame()
	if len(after.Rows) > 4 {
		t.Fatalf("resized frame has %d rows, want at most 4", len(after.Rows))
	}
	if !strings.Contains(strings.Join(after.Rows, "\n"), "shell: vsh") {
		t.Fatalf("resized frame lost the referenced content: %q", after.Text())
	}
	_ = before

	// Quit is a semantic event the handler answers with an exit.
	h.press("q", "")
	if !h.machine.State().Exited || h.machine.State().ExitCode != 0 {
		t.Fatalf("state after q = %+v, want exited with code 0", h.machine.State())
	}
}

func TestMonitorExampleAcceptanceFlow(t *testing.T) {
	example := loadExample(t, "monitor")
	h := newHost(t, example)

	frame := h.frame()
	if len(frame.Rows) < 2 || !strings.HasPrefix(frame.Rows[0], "PID") {
		t.Fatalf("first frame row = %q, want the column titles", frame.Rows[0])
	}
	if !strings.Contains(strings.Join(frame.Rows, "\n"), "session-supervisor") {
		t.Fatalf("frame does not show the simulated rows: %q", frame.Text())
	}
	if !strings.Contains(frame.Status, "simulated") && !strings.Contains(h.frame().Text(), "simulated") {
		t.Fatalf("the screen does not say its data is simulated: %q / %q", frame.Status, frame.Text())
	}

	// Row selection is a cursor move in the cell space.
	if got := h.machine.State().Table.Cursor.Line; got != 1 {
		t.Fatalf("selected row = %d, want 1", got)
	}
	h.press("j", "")
	if got := h.machine.State().Table.Cursor.Line; got != 2 {
		t.Fatalf("selected row after j = %d, want 2", got)
	}
	h.press("k", "")
	if got := h.machine.State().Table.Cursor.Line; got != 1 {
		t.Fatalf("selected row after k = %d, want 1", got)
	}
	h.press("l", "")
	if got := h.machine.State().Table.Cursor.Column; got != 3 {
		t.Fatalf("selected column after l = %d, want 3", got)
	}

	// A row selection is a range selection in the same space.
	h.press("v", "")
	if h.machine.State().Table.Anchor == nil {
		t.Fatal("v must start a row range")
	}
	h.press("x", "")
	if h.machine.State().Table.Anchor != nil {
		t.Fatal("x must clear the row range")
	}

	// Sorting simulated data is the handler's decision; the key emits an event.
	h.press("P", "")
	state := h.machine.State()
	if state.Table.Sort.Column != "cpu" || state.Table.Sort.Order != interactions.SortDescending {
		t.Fatalf("sort state = %+v, want cpu descending", state.Table.Sort)
	}
	if got := state.Table.Rows[0][0]; got != "102" {
		t.Fatalf("first row after sorting by cpu = %s, want 102", got)
	}
	if !strings.Contains(h.status(), "cpu descending") {
		t.Fatalf("status line does not report the ordering: %q", h.status())
	}
	h.press("M", "")
	if got := h.machine.State().Table.Rows[0][0]; got != "103" {
		t.Fatalf("first row after sorting by mem = %s, want 103", got)
	}

	// A refresh replaces the sample; it never reads a host counter.
	h.press("r", "")
	if got := h.machine.State().Table.Rows[1][2]; got != "2.1" {
		t.Fatalf("cpu column after refresh = %s, want the refreshed sample 2.1", got)
	}
	if !strings.Contains(h.status(), "refreshed simulated sample") {
		t.Fatalf("status after refresh = %q", h.status())
	}

	// Quit.
	h.press("q", "")
	if !h.machine.State().Exited {
		t.Fatal("q must leave the monitor")
	}
}

func TestExamplesRouteUnknownKeysThroughTheHandlerThenTheExtensionPath(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		key  string
	}{
		{name: "editor", dir: "editor", key: "Z"},
		{name: "pager", dir: "pager", key: "Z"},
		{name: "monitor", dir: "monitor", key: "Z"},
		{name: "monitor lower case", dir: "monitor", key: "z"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHost(t, loadExample(t, testCase.dir))
			if kind := h.press(testCase.key, ""); kind != interactions.OutcomeUnknown {
				t.Fatalf("outcome for %q = %v, want unknown", testCase.key, kind)
			}
			if len(h.extension) != 1 {
				t.Fatalf("extension requests = %d, want exactly 1", len(h.extension))
			}
			request := h.extension[0]
			if !strings.Contains(request.Prompt, "keys="+testCase.key) {
				t.Fatalf("extension prompt = %q, want it to name the key", request.Prompt)
			}
			if request.MaxTokens <= 0 || request.TimeoutMs <= 0 {
				t.Fatalf("extension request is unbounded: %+v", request)
			}
			if h.sandbox.unhandled != 1 {
				t.Fatalf("handler refusals = %d, want the key offered once", h.sandbox.unhandled)
			}
			// Nothing may have changed on screen: an unknown key is not a command.
			if h.machine.State().Dirty {
				t.Fatal("an unknown key must not edit the document")
			}
		})
	}
}

func TestExampleHandlerKeysAreHandledLocallyWithoutAnExtension(t *testing.T) {
	h := newHost(t, loadExample(t, "editor"))
	h.press(":", "")
	h.press("w", "")
	if len(h.extension) != 0 {
		t.Fatalf("a key the handler knows produced %d extension requests, want 0", len(h.extension))
	}
	if got := h.machine.State().PromptText; got != "w" {
		t.Fatalf("prompt text = %q, want the typed command", got)
	}
}

// An unknown command is the application's own judgement, not a missing binding:
// the handler answers it and no model call happens.
func TestUnknownCommandStaysInsideTheArtifact(t *testing.T) {
	h := newHost(t, loadExample(t, "editor"))
	h.press(":", "")
	for _, key := range []string{"x", "enter"} {
		h.press(key, "")
	}
	if len(h.extension) != 0 {
		t.Fatalf("an unknown command produced %d extension requests, want 0", len(h.extension))
	}
	if got := h.status(); !strings.Contains(got, "E492") {
		t.Fatalf("status for an unknown command = %q, want the handler's error", got)
	}
	if h.machine.State().Exited {
		t.Fatal("an unknown command must not leave the interaction")
	}
}

func TestExampleArtifactsAreValidAndHashed(t *testing.T) {
	for _, dir := range []string{"editor", "pager", "monitor"} {
		t.Run(dir, func(t *testing.T) {
			example := loadExample(t, dir)
			artifact := artifactBody(t, example)
			if artifact.SourceHash.IsZero() {
				t.Fatal("the fixture artifact must carry a real source hash")
			}
			if err := domain.ValidateManifest(artifact.Manifest); err != nil {
				t.Fatalf("ValidateManifest: %v", err)
			}
			if !artifact.Validation.Passed {
				t.Fatal("the fixture artifact records a failed validation")
			}
			// Every example must be expressible with the approved actions only.
			h := newHost(t, example)
			view := mustView(t, h.machine)
			if len(view.KeyBindings) == 0 {
				t.Fatal("the view must publish the binding table")
			}
			for _, binding := range view.KeyBindings {
				if !domain.IsValidPrimitiveAction(binding.Action) {
					t.Fatalf("view publishes action %q, which is not approved", binding.Action)
				}
			}
		})
	}
}

func TestTextViewRendersPlainLinesWithoutACursor(t *testing.T) {
	spec := mustSpec(t, `{
      "view": { "mode": "text", "status_line": "output -- {status}" },
      "default_mode": "read",
      "modes": [{ "name": "read", "bindings": [
        { "keys": "down", "action": "scroll", "params": { "scroll": { "unit": "line_down" } } },
        { "keys": "up", "action": "scroll", "params": { "scroll": { "unit": "line_up" } } }
      ]}]
    }`)
	machine, err := interactions.New(spec, interactions.State{
		Buffer: interactions.Buffer{Lines: []string{"first", "second", "third", "fourth"}},
	}, interactions.ScreenMetrics{Rows: 3, Columns: 20}, interactions.Limits{})
	if err != nil {
		t.Fatalf("building text view: %v", err)
	}
	frame, err := machine.Frame()
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if got := strings.Join(frame.Rows, ","); got != "first,second" {
		t.Fatalf("text frame rows = %q, want the two rows that fit above the status line", got)
	}
	if frame.Cursor != nil {
		t.Fatalf("a text view shows no cursor, got %+v", frame.Cursor)
	}
	if len(frame.Spans) != 0 {
		t.Fatalf("a plain text view carries no spans, got %+v", frame.Spans)
	}
	press(t, machine, "down", "")
	if got := machine.State().Viewport.Top; got != 1 {
		t.Fatalf("viewport after one line down = %d, want 1", got)
	}
}

func TestStatusViewAlignsColumnsAndMarksTheSelectedRow(t *testing.T) {
	spec := mustSpec(t, `{
      "view": {
        "mode": "status",
        "columns": [
          { "title": "NAME", "align": "left" },
          { "title": "LOAD", "align": "right" }
        ],
        "footer": "2 entries"
      },
      "default_mode": "browse",
      "modes": [{ "name": "browse", "bindings": [
        { "keys": "down", "action": "move_cursor", "params": { "move": { "target": "down" } } },
        { "keys": "v", "action": "select_range", "params": { "select": { "from": "cursor", "to": "cursor" } } }
      ]}]
    }`)
	machine, err := interactions.New(spec, interactions.State{
		Table: interactions.Table{
			Rows: [][]string{
				{"alpha", "0.4"},
				{"beta-with-a-longer-name", "12.5"},
			},
		},
	}, interactions.ScreenMetrics{Rows: 8, Columns: 40}, interactions.Limits{})
	if err != nil {
		t.Fatalf("building status view: %v", err)
	}
	press(t, machine, "down", "")
	press(t, machine, "v", "")
	frame, err := machine.Frame()
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if len(frame.Rows) != 4 {
		t.Fatalf("status frame has %d rows, want titles, two rows, and a footer", len(frame.Rows))
	}
	if !strings.HasPrefix(frame.Rows[0], "NAME") {
		t.Fatalf("first row = %q, want the column titles", frame.Rows[0])
	}
	// The load column is right aligned against the widest cell, so the numbers
	// line up under the title.
	if !strings.HasSuffix(strings.TrimRight(frame.Rows[1], " "), "0.4") {
		t.Fatalf("first data row = %q, want the load value at the end", frame.Rows[1])
	}
	if !strings.HasSuffix(frame.Rows[3], "2 entries") {
		t.Fatalf("last row = %q, want the footer", frame.Rows[3])
	}
	marked := false
	for _, span := range frame.Spans {
		if span.Style == interactions.StyleSelectedRow && span.Row == 2 {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("selected row is not marked: %+v", frame.Spans)
	}
	if frame.Cursor == nil || frame.Cursor.Line != 2 {
		t.Fatalf("cell cursor = %+v, want the second data row", frame.Cursor)
	}
}

func mustInput(t *testing.T, keys string) interactions.Input {
	t.Helper()
	return interactions.Input{Chords: mustSequence(t, keys)}
}

func mustView(t *testing.T, machine *interactions.Machine) domain.AppView {
	t.Helper()
	view, err := machine.View()
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if err := domain.ValidateView(view); err != nil {
		t.Fatalf("view failed domain validation: %v", err)
	}
	return view
}
