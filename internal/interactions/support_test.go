package interactions_test

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/interactions"
)

// fixtureDir is the checked-in example set these tests drive.
const fixtureDir = "../../tests/fixtures/interactions"

// errorText returns the human-readable message of a domain error. Its Error()
// string carries only the category and code, which is what operators match on;
// the message is the detail a test asserts.
func errorText(err error) string {
	var domainErr *domain.DomainError
	if errors.As(err, &domainErr) && domainErr.Message != "" {
		return domainErr.Message
	}
	return err.Error()
}

// fixtureTimestamp is a fixed instant, so a fixture artifact is byte-identical
// on every run.
const fixtureTimestamp = int64(1_757_000_000_000)

// artifactFixture is the static description of an example artifact. The handler
// lives in its own file because it is the artifact's JavaScript source, not a
// JSON field.
type artifactFixture struct {
	AppID        string   `json:"app_id"`
	VersionID    string   `json:"version_id"`
	CommandNames []string `json:"command_names"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Entrypoint   string   `json:"entrypoint"`
	Version      string   `json:"version"`
	Handler      string   `json:"handler"`
}

// handlerResult is what the artifact's handler decides for one event. Every
// field is optional; a rule that only sets a status is a complete rule.
type handlerResult struct {
	Mode            string                  `json:"mode,omitempty"`
	Status          string                  `json:"status,omitempty"`
	StatusPrefix    string                  `json:"status_prefix,omitempty"`
	ClearStatus     bool                    `json:"clear_status,omitempty"`
	ClearPrompt     bool                    `json:"clear_prompt,omitempty"`
	AppendPrompt    string                  `json:"append_prompt,omitempty"`
	BackspacePrompt bool                    `json:"backspace_prompt,omitempty"`
	Dirty           *bool                   `json:"dirty,omitempty"`
	Cursor          *interactions.Cursor    `json:"cursor,omitempty"`
	BufferLines     []string                `json:"buffer_lines,omitempty"`
	TableRows       [][]string              `json:"table_rows,omitempty"`
	TableCursor     *interactions.Cursor    `json:"table_cursor,omitempty"`
	Sort            *interactions.SortState `json:"sort,omitempty"`
	Focus           *int                    `json:"focus,omitempty"`
	Fields          []string                `json:"fields,omitempty"`
	Exited          bool                    `json:"exited,omitempty"`
	ExitCode        int                     `json:"exit_code,omitempty"`
	Effects         []domain.AppEffect      `json:"effects,omitempty"`
}

// handlerRule matches an event, or a raw key, and answers with a result. The
// command, dirty flag, and field conditions are what let one table express the
// application's own conditional decisions. A text rule may match one key
// spelling or, with printable set, any printable text the terminal delivered.
type handlerRule struct {
	Event     string            `json:"event,omitempty"`
	Command   string            `json:"command,omitempty"`
	Dirty     *bool             `json:"dirty,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	Keys      string            `json:"keys,omitempty"`
	Printable bool              `json:"printable,omitempty"`
	// Modes restricts a raw-input rule to the interaction modes whose prompt
	// line it owns. Without it the rule would swallow every printable key in
	// every mode, including the ones where the application expects nothing.
	Modes     []string      `json:"modes,omitempty"`
	Unhandled bool          `json:"unhandled,omitempty"`
	Result    handlerResult `json:"result"`
}

// appendPromptText is the substitution the handler table uses to append whatever
// text the terminal delivered, which is what a prompt line does.
const appendPromptText = "$text"

type handlerFixture struct {
	Rules []handlerRule `json:"rules"`
	// TextRules answer raw key input, which is how an application owns its own
	// prompt line.
	TextRules []handlerRule `json:"text_rules"`
	// EchoPrompts maps a mode to the prefix its prompt line is echoed with, so
	// the table states the handler's behaviour once instead of per key.
	EchoPrompts map[string]string `json:"echo_prompts"`
}

// exampleFixture is one declarative acceptance example.
type exampleFixture struct {
	Example     string                     `json:"example"`
	Artifact    artifactFixture            `json:"artifact"`
	Interaction interactions.Spec          `json:"interaction"`
	State       interactions.State         `json:"state"`
	Screen      interactions.ScreenMetrics `json:"screen"`
	Handler     handlerFixture             `json:"handler"`

	Source   string             `json:"-"`
	Manifest domain.AppManifest `json:"-"`
}

// loadExample reads one example fixture and builds the artifact, spec, and state
// the machine consumes.
func loadExample(t *testing.T, name string) exampleFixture {
	t.Helper()
	path := filepath.Join(fixtureDir, name, "app.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	var example exampleFixture
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&example); err != nil {
		t.Fatalf("decoding fixture %s: %v", path, err)
	}
	source, err := os.ReadFile(filepath.Join(fixtureDir, name, example.Artifact.Handler))
	if err != nil {
		t.Fatalf("reading handler for %s: %v", name, err)
	}
	example.Source = string(source)
	if strings.TrimSpace(example.Source) == "" {
		t.Fatalf("fixture %s has an empty handler source", name)
	}

	if _, err := domain.ParseAppID(example.Artifact.AppID); err != nil {
		t.Fatalf("fixture %s app_id: %v", name, err)
	}
	if _, err := domain.ParseAppVersionID(example.Artifact.VersionID); err != nil {
		t.Fatalf("fixture %s version_id: %v", name, err)
	}
	example.Manifest = domain.AppManifest{
		ABIVersion:   domain.AppABIVersion,
		CommandNames: example.Artifact.CommandNames,
		Description:  example.Artifact.Description,
		Capabilities: example.Artifact.Capabilities,
		Entrypoint:   example.Artifact.Entrypoint,
		Version:      example.Artifact.Version,
	}
	if err := domain.ValidateManifest(example.Manifest); err != nil {
		t.Fatalf("fixture %s manifest: %v", name, err)
	}
	if err := example.Interaction.Validate(); err != nil {
		t.Fatalf("fixture %s interaction spec: %s", name, errorText(err))
	}
	if err := example.Screen.Validate(); err != nil {
		t.Fatalf("fixture %s screen: %v", name, err)
	}
	return example
}

// artifactBody builds the immutable artifact the fixture describes, including
// the real content hash of the handler source.
func artifactBody(t *testing.T, example exampleFixture) domain.AppArtifact {
	t.Helper()
	appID, err := domain.ParseAppID(example.Artifact.AppID)
	if err != nil {
		t.Fatalf("app id: %v", err)
	}
	versionID, err := domain.ParseAppVersionID(example.Artifact.VersionID)
	if err != nil {
		t.Fatalf("version id: %v", err)
	}
	return domain.AppArtifact{
		AppID:      appID,
		VersionID:  versionID,
		Manifest:   example.Manifest,
		Source:     example.Source,
		SourceHash: contentIDOf(t, example.Source),
		Scope:      domain.ScopeUser,
		Validation: domain.ValidationResult{
			Passed:           true,
			ValidatedAt:      fixtureTimestamp,
			ValidatorVersion: "c05-fixtures",
		},
	}
}

// crockford is the base32 alphabet domain identities use.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// contentIDOf hashes bytes into a domain ContentID the way the content store
// addresses them, so a fixture carries a real reference instead of a
// placeholder.
func contentIDOf(t *testing.T, data string) domain.ContentID {
	t.Helper()
	sum := sha256.Sum256([]byte(data))
	encoded := crockford.EncodeToString(sum[:16])
	if len(encoded) < 26 {
		t.Fatalf("content hash encoded to %d characters, want 26", len(encoded))
	}
	id, err := domain.ParseContentID(domain.PrefixContent + "_" + encoded[:26])
	if err != nil {
		t.Fatalf("parsing derived content id: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Sandbox double
// ---------------------------------------------------------------------------

// handlerCall records one delivery to the artifact's handler.
type handlerCall struct {
	Kind    string // "input" or "semantic"
	Keys    string
	Text    string
	Event   interactions.EventPayload
	Matched string
}

// fakeSandbox stands in for the AppSandbox port. It does not execute
// JavaScript: it matches the artifact's declared rules, which express the same
// decisions as data, and returns the resulting state and app result. Everything
// the primitives do stays the real implementation.
type fakeSandbox struct {
	t         *testing.T
	example   exampleFixture
	calls     []handlerCall
	last      interactions.State
	Effects   []domain.AppEffect
	unhandled int
}

func newFakeSandbox(t *testing.T, example exampleFixture) *fakeSandbox {
	return &fakeSandbox{t: t, example: example}
}

// sync takes the interaction state the machine reached, so a rule is answered
// from what is actually on screen rather than from the last state the handler
// produced.
func (f *fakeSandbox) sync(state interactions.State) { f.last = state }

// handle answers one delivery. The returned state is what the machine should
// continue with; ok is false when no declared rule applies, which is the signal
// to route the key to the model-driven extension path.
func (f *fakeSandbox) handle(kind, keys, text string, event interactions.EventPayload) (interactions.State, bool) {
	f.t.Helper()
	call := handlerCall{Kind: kind, Keys: keys, Text: text, Event: event}
	f.calls = append(f.calls, call)
	rule, ok := f.match(kind, keys, event, f.last)
	if !ok {
		f.unhandled++
		return interactions.State{}, false
	}
	next := f.apply(rule.Result, text)
	result := domain.AppResult{
		View:    domain.AppView{Mode: f.example.Interaction.View.Mode},
		Effects: rule.Result.Effects,
	}
	if err := domain.ValidateResult(result); err != nil {
		f.t.Fatalf("fixture handler produced an invalid app result: %s", errorText(err))
	}
	f.Effects = append(f.Effects, rule.Result.Effects...)
	f.calls[len(f.calls)-1].Matched = f.name(kind, rule)
	return next, true
}

// state is what the machine reported before this delivery. The double keeps the
// last state it produced so a rule can be answered without the machine in hand.
// match finds the declared rule that answers one delivery: an event name plus
// its conditions for semantic events, a key spelling for raw input.
func (f *fakeSandbox) match(kind, keys string, event interactions.EventPayload, current interactions.State) (handlerRule, bool) {
	if kind == "input" {
		for _, rule := range f.example.Handler.TextRules {
			if rule.Unhandled || !ruleAppliesToMode(rule, current.Mode) {
				continue
			}
			if rule.Keys != "" && rule.Keys != keys {
				continue
			}
			if rule.Keys == "" && !rule.Printable {
				continue
			}
			return rule, true
		}
		return handlerRule{}, false
	}
	for _, rule := range f.example.Handler.Rules {
		if rule.Unhandled || rule.Event != event.Name {
			continue
		}
		if rule.Command != "" && rule.Command != strings.TrimSpace(event.Command) {
			continue
		}
		if rule.Dirty != nil && *rule.Dirty != current.Dirty {
			continue
		}
		if len(rule.Fields) > 0 && !fieldsMatch(rule.Fields, event.Fields) {
			continue
		}
		return rule, true
	}
	return handlerRule{}, false
}

// ruleAppliesToMode reports whether a raw-input rule owns the key in the current
// mode. A rule with no modes owns it everywhere.
func ruleAppliesToMode(rule handlerRule, mode string) bool {
	if len(rule.Modes) == 0 {
		return true
	}
	for _, candidate := range rule.Modes {
		if candidate == mode {
			return true
		}
	}
	return false
}

func (f *fakeSandbox) name(kind string, rule handlerRule) string {
	if kind == "input" {
		return "input:" + rule.Keys
	}
	if rule.Command != "" {
		return rule.Event + ":" + rule.Command
	}
	return rule.Event
}

func fieldsMatch(want, got map[string]string) bool {
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

// apply turns a rule result into the next interaction state.
func (f *fakeSandbox) apply(result handlerResult, text string) interactions.State {
	next := f.last
	if result.Mode != "" {
		next.Mode = result.Mode
	}
	if result.ClearPrompt {
		next.PromptText = ""
	}
	if result.AppendPrompt == appendPromptText {
		next.PromptText += text
	} else if result.AppendPrompt != "" {
		next.PromptText += result.AppendPrompt
	}
	if result.BackspacePrompt && next.PromptText != "" {
		next.PromptText = next.PromptText[:len(next.PromptText)-1]
	}
	if result.ClearStatus {
		next.Status = ""
	}
	if result.Status != "" {
		next.Status = result.Status
	}
	if result.StatusPrefix != "" {
		next.Status = result.StatusPrefix + next.PromptText
	}
	if result.Status == "" && result.StatusPrefix == "" {
		// The handler's own prompt line is echoed with the prefix its mode
		// declares, the way a real prompt is.
		if prefix, ok := f.example.Handler.EchoPrompts[next.Mode]; ok && next.PromptText != "" {
			next.Status = prefix + next.PromptText
		}
	}
	if result.Dirty != nil {
		next.Dirty = *result.Dirty
	}
	if result.Cursor != nil {
		next.Buffer.Cursor = *result.Cursor
	}
	if result.BufferLines != nil {
		next.Buffer.Lines = append([]string(nil), result.BufferLines...)
	}
	if result.TableRows != nil {
		next.Table.Rows = cloneRows(result.TableRows)
	}
	if result.TableCursor != nil {
		next.Table.Cursor = *result.TableCursor
	}
	if result.Sort != nil {
		next.Table.Sort = *result.Sort
	}
	if result.Focus != nil {
		next.Focus = *result.Focus
	}
	if result.Fields != nil {
		next.Fields = append([]string(nil), result.Fields...)
	}
	if result.Exited {
		next.Exited = true
		next.ExitCode = result.ExitCode
	}
	f.last = next
	return next
}

func cloneRows(rows [][]string) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = append([]string(nil), row...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Host
// ---------------------------------------------------------------------------

// host is the interaction host these tests exercise. It owns a machine, offers
// unbound keys to the artifact's handler, and asks the model for an extension
// when the handler cannot act. That is the flow a session coordinator follows,
// expressed without depending on any port.
type host struct {
	t         *testing.T
	machine   *interactions.Machine
	sandbox   *fakeSandbox
	spec      interactions.Spec
	extension []domain.AIRequest
}

// newHost builds a host around one example fixture.
func newHost(t *testing.T, example exampleFixture) *host {
	t.Helper()
	machine, err := interactions.New(example.Interaction, example.State, example.Screen, interactions.Limits{})
	if err != nil {
		t.Fatalf("building machine for %s: %v", example.Example, err)
	}
	sandbox := newFakeSandbox(t, example)
	sandbox.last = machine.State()
	return &host{t: t, machine: machine, sandbox: sandbox, spec: example.Interaction}
}

// press delivers one key run and follows the resulting path: a primitive action,
// a pending multi-key command, the artifact's handler, or the extension path.
func (h *host) press(keys, text string) interactions.OutcomeKind {
	return h.pressOutcome(keys, text).Kind
}

// pressOutcome is press with the full outcome, for a test that needs to inspect
// the semantic events a key emitted.
func (h *host) pressOutcome(keys, text string) interactions.Outcome {
	h.t.Helper()
	if h.machine.State().Exited {
		h.t.Fatalf("pressing %q after the interaction asked to exit", keys)
	}
	seq, err := interactions.ParseSequence(keys)
	if err != nil {
		h.t.Fatalf("test key %q: %v", keys, err)
	}
	outcome, err := h.machine.Apply(interactions.Input{Chords: seq, Text: text})
	if err != nil {
		h.t.Fatalf("applying %q: %v", keys, err)
	}
	switch outcome.Kind {
	case interactions.OutcomeUnknown:
		h.routeToHandler(outcome, seq, text)
	case interactions.OutcomeExited:
		return outcome
	}
	for _, event := range outcome.Events {
		h.deliverSemantic(event)
	}
	return outcome
}

// routeToHandler offers an unbound key to the artifact's own handler first and
// only asks the model for an extension when the handler cannot act. A printable
// chord is delivered as its text, which is what a terminal layer does, so an
// application prompt line can accumulate it.
func (h *host) routeToHandler(outcome interactions.Outcome, seq interactions.Sequence, text string) {
	h.t.Helper()
	if text == "" {
		if printable, ok := seq[0].PrintableText(); ok {
			text = printable
		}
	}
	event, err := interactions.InputEvent(fixtureTimestamp, nil, nil, interactions.Input{Chords: seq, Text: text})
	if err != nil {
		h.t.Fatalf("building input event: %v", err)
	}
	var keys struct {
		Keys string `json:"keys"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(event.Payload, &keys); err != nil {
		h.t.Fatalf("decoding input event payload: %v", err)
	}
	if h.deliverInput(keys.Keys, keys.Text) {
		return
	}
	view, err := h.machine.View()
	if err != nil {
		h.t.Fatalf("view for extension request: %v", err)
	}
	request, err := interactions.ExtensionRequest("req-1",
		interactions.Input{Chords: seq, Text: text}, view, h.machine.State().Status,
		interactions.DefaultLimits())
	if err != nil {
		h.t.Fatalf("building extension request: %v", err)
	}
	h.extension = append(h.extension, request)
}

func (h *host) deliverInput(keys, text string) bool {
	h.t.Helper()
	h.sandbox.sync(h.machine.State())
	next, ok := h.sandbox.handle("input", keys, text, interactions.EventPayload{})
	if !ok {
		return false
	}
	h.install(next)
	return true
}

func (h *host) deliverSemantic(event interactions.EventPayload) {
	h.t.Helper()
	h.sandbox.sync(h.machine.State())
	next, ok := h.sandbox.handle("semantic", event.Chord, "", event)
	if !ok {
		h.t.Fatalf("emitted event %q matched no handler rule", event.Name)
	}
	h.install(next)
}

// install replaces the machine with what the handler returned. A handler that
// switches modes returns the same spec in the new mode, which is what Replace
// validates.
func (h *host) install(next interactions.State) {
	h.t.Helper()
	spec := h.spec
	if next.Mode != "" {
		spec.Mode = next.Mode
	}
	if err := h.machine.Replace(spec, next); err != nil {
		h.t.Fatalf("handler result failed validation: %v", err)
	}
}

// frame builds the plain-data frame the trusted renderer would draw.
func (h *host) frame() interactions.Frame {
	h.t.Helper()
	frame, err := h.machine.Frame()
	if err != nil {
		h.t.Fatalf("building frame: %v", err)
	}
	return frame
}

// status returns the status line the renderer would show.
func (h *host) status() string { return h.frame().Status }
