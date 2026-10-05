package interactions

import (
	"encoding/json"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// ColumnPolicy states where a mode allows the cursor to rest within a line.
// "within_line" keeps the cursor on an existing character, which is what
// command-mode editing needs; "allow_end" additionally allows the insertion
// position at the end of the line, which is what text entry needs. It is a
// declared policy rather than a rule keyed on program names, so a new
// application picks its own convention.
type ColumnPolicy string

const (
	// ColumnWithinLine forbids the position past the last character.
	ColumnWithinLine ColumnPolicy = "within_line"
	// ColumnAllowEnd permits the position after the last character.
	ColumnAllowEnd ColumnPolicy = "allow_end"
)

// Align is the horizontal alignment of a table column.
type Align string

const (
	AlignLeft   Align = "left"
	AlignRight  Align = "right"
	AlignCenter Align = "center"
)

// Column describes one table column. Sortable is a declaration for the
// application: sorting simulated data is handler logic, and the column's SortKey
// is what the handler receives in the semantic event a sort key emits.
type Column struct {
	Title   string `json:"title"`
	Align   Align  `json:"align,omitempty"`
	SortKey string `json:"sort_key,omitempty"`
}

// Field describes one form field. The value is application state; the label and
// the editable flag are presentation the view declares.
type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Editable bool   `json:"editable,omitempty"`
}

// ViewSpec is the declarative screen description. Fields that do not belong to
// the view mode are rejected, so a table cannot smuggle in form fields and the
// renderer never has to guess which parts of the description are live.
type ViewSpec struct {
	Mode       string `json:"mode"`
	StatusLine string `json:"status_line,omitempty"`
	// Gutter renders line numbers to the left of buffer lines (editor only).
	Gutter bool `json:"gutter,omitempty"`
	// Columns and Footer describe table and status screens.
	Columns []Column `json:"columns,omitempty"`
	Footer  string   `json:"footer,omitempty"`
	// Fields describes a form screen.
	Fields []Field `json:"fields,omitempty"`
	// CursorVisible draws a cursor in views that have none by default: a text
	// output screen, or a pager that follows its position in the document.
	CursorVisible bool `json:"cursor_visible,omitempty"`
}

// ModeDef declares one interaction mode: its key bindings, the modes it may
// switch to, and its cursor convention.
type ModeDef struct {
	Name string `json:"name"`
	// Column is the cursor convention of this mode.
	Column ColumnPolicy `json:"column_policy,omitempty"`
	// Inherit defaults to true: the mode starts from the default mode's
	// bindings and overrides what it declares. A mode that claims printable
	// keys for text entry must set it to false, or the inherited letter keys
	// would swallow the text instead of reaching the application.
	Inherit *bool `json:"inherit,omitempty"`
	// Targets lists the modes this mode may switch to.
	Targets  []string      `json:"mode_targets,omitempty"`
	Bindings []BindingSpec `json:"bindings"`
}

// BindingSpec is the serialized form of a binding: the key sequence is written
// in canonical chord syntax, and parameters appear under the key matching the
// action.
type BindingSpec struct {
	Keys   string `json:"keys"`
	Action string `json:"action"`
	Params Params `json:"params"`
}

// Spec is a complete declarative interaction description: what the screen is,
// which modes exist, and which key sequences drive which approved primitive
// action. It is data an application produces, not code, so a new application
// needs no change in this package.
type Spec struct {
	View ViewSpec `json:"view"`
	// Mode is the mode the interaction starts in.
	Mode string `json:"mode"`
	// DefaultMode supplies the bindings every mode inherits, so modes that
	// differ in only a few keys stay short.
	DefaultMode string    `json:"default_mode"`
	Modes       []ModeDef `json:"modes"`
}

// binding is the decoded form used at run time.
type binding struct {
	Keys   Sequence
	Action string
	Params Params
}

// ParseSpec decodes and validates a Spec from its serialized form. Unknown
// fields are rejected so a typo in a generated artifact fails loudly instead of
// silently disabling a binding.
func ParseSpec(raw []byte) (Spec, error) {
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, domain.NewValidationError(domain.CodeInvalidAppView,
			"interaction spec is not valid JSON: "+err.Error(), nil)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Validate checks the spec structurally: view mode, declared modes, mode
// targets, bindings, and duplicate keys. Resource bounds are enforced by New,
// which is the first point that knows the configured limits.
func (s Spec) Validate() error {
	if !domain.IsValidAppViewMode(s.View.Mode) {
		return specError("unknown view mode %q", s.View.Mode)
	}
	if len(s.Modes) == 0 {
		return specError("spec declares no modes")
	}
	byName := make(map[string]ModeDef, len(s.Modes))
	for _, mode := range s.Modes {
		if mode.Name == "" {
			return specError("mode name must not be empty")
		}
		if _, exists := byName[mode.Name]; exists {
			return specError("mode %q declared twice", mode.Name)
		}
		byName[mode.Name] = mode
	}
	if s.DefaultMode == "" {
		return specError("spec declares no default mode")
	}
	if _, ok := byName[s.DefaultMode]; !ok {
		return specError("default mode %q is not declared", s.DefaultMode)
	}
	if s.Mode != "" {
		if _, ok := byName[s.Mode]; !ok {
			return specError("mode %q is not declared", s.Mode)
		}
	}
	for _, mode := range s.Modes {
		if err := validateMode(mode, byName); err != nil {
			return err
		}
	}
	return s.View.validate()
}

func validateMode(mode ModeDef, byName map[string]ModeDef) error {
	switch mode.Column {
	case "":
		mode.Column = ColumnWithinLine
	case ColumnWithinLine, ColumnAllowEnd:
	default:
		return specError("mode %q has unknown column policy %q", mode.Name, mode.Column)
	}
	for _, target := range mode.Targets {
		if _, ok := byName[target]; !ok {
			return specError("mode %q targets undeclared mode %q", mode.Name, target)
		}
	}
	seen := make(map[string]struct{}, len(mode.Bindings))
	for i := range mode.Bindings {
		declared := mode.Bindings[i]
		seq, err := ParseSequence(declared.Keys)
		if err != nil {
			return specError("mode %q binding %q: %v", mode.Name, declared.Keys, err)
		}
		converted := &Binding{Keys: seq, Action: declared.Action, Params: declared.Params}
		if err := converted.validate(); err != nil {
			return err
		}
		key := seq.String()
		if _, dup := seen[key]; dup {
			return specError("mode %q binds %q twice", mode.Name, key)
		}
		seen[key] = struct{}{}
		if declared.Action == domain.ActionModeSwitch {
			target := converted.Params.Mode.Mode
			if target != mode.Name {
				if _, ok := byName[target]; !ok {
					return specError("mode %q switches to undeclared mode %q", mode.Name, target)
				}
				if !modeSwitchesAllowed(mode, target) {
					return specError("mode %q may not switch to %q", mode.Name, target)
				}
			}
		}
	}
	return nil
}

// modeSwitchesAllowed enforces the declared mode_targets list. A mode that
// declares no targets may still switch to itself.
func modeSwitchesAllowed(mode ModeDef, target string) bool {
	for _, allowed := range mode.Targets {
		if allowed == target {
			return true
		}
	}
	return false
}

func (v ViewSpec) validate() error {
	if err := validateStatusLine(v.StatusLine); err != nil {
		return err
	}
	switch v.Mode {
	case domain.AppViewModeText, domain.AppViewModeEditor, domain.AppViewModePager:
		if len(v.Columns) > 0 {
			return specError("view mode %q must not declare columns", v.Mode)
		}
		if len(v.Fields) > 0 {
			return specError("view mode %q must not declare fields", v.Mode)
		}
		if v.Footer != "" {
			return specError("view mode %q must not declare a footer", v.Mode)
		}
	case domain.AppViewModeTable, domain.AppViewModeStatus:
		if len(v.Fields) > 0 {
			return specError("view mode %q must not declare fields", v.Mode)
		}
		if v.Gutter {
			return specError("view mode %q must not declare a gutter", v.Mode)
		}
		if len(v.Columns) == 0 {
			return specError("view mode %q requires columns", v.Mode)
		}
	case domain.AppViewModeForm:
		if len(v.Columns) > 0 {
			return specError("view mode %q must not declare columns", v.Mode)
		}
		if v.Footer != "" {
			return specError("view mode %q must not declare a footer", v.Mode)
		}
		if len(v.Fields) == 0 {
			return specError("view mode %q requires fields", v.Mode)
		}
	default:
		return specError("unknown view mode %q", v.Mode)
	}
	for i, column := range v.Columns {
		if column.Title == "" {
			return specError("column %d has no title", i)
		}
		switch column.Align {
		case "", AlignLeft:
		case AlignRight, AlignCenter:
		default:
			return specError("column %q has unknown alignment %q", column.Title, column.Align)
		}
	}
	names := make(map[string]struct{}, len(v.Fields))
	for i, field := range v.Fields {
		if field.Name == "" {
			return specError("field %d has no name", i)
		}
		if _, dup := names[field.Name]; dup {
			return specError("field %q declared twice", field.Name)
		}
		names[field.Name] = struct{}{}
	}
	return nil
}

// mode returns the declaration for a mode name.
func (s Spec) mode(name string) (ModeDef, bool) {
	for _, mode := range s.Modes {
		if mode.Name == name {
			return mode, true
		}
	}
	return ModeDef{}, false
}

// columnPolicy is the cursor convention of a mode, defaulting to
// ColumnWithinLine.
func (s Spec) columnPolicy(name string) ColumnPolicy {
	mode, ok := s.mode(name)
	if !ok || mode.Column == "" {
		return ColumnWithinLine
	}
	return mode.Column
}

// effectiveBindings returns the binding table of one mode. A mode inherits the
// default mode's bindings and overrides what it declares, which keeps a pair of
// similar modes short; a mode that sets inherit to false stands alone.
func (s Spec) effectiveBindings(modeName string) ([]binding, error) {
	def, ok := s.mode(s.DefaultMode)
	if !ok {
		return nil, specError("default mode %q is not declared", s.DefaultMode)
	}
	own, ok := s.mode(modeName)
	if !ok {
		return nil, specError("mode %q is not declared", modeName)
	}
	var inherited []BindingSpec
	if modeName == s.DefaultMode || inheritsBindings(own) {
		inherited = def.Bindings
	}
	merged := make(map[string]binding, len(inherited)+len(own.Bindings))
	order := make([]string, 0, len(inherited)+len(own.Bindings))
	add := func(declared BindingSpec) error {
		seq, err := ParseSequence(declared.Keys)
		if err != nil {
			return specError("binding %q: %v", declared.Keys, err)
		}
		key := seq.String()
		if _, exists := merged[key]; !exists {
			order = append(order, key)
		}
		merged[key] = binding{Keys: seq, Action: declared.Action, Params: declared.Params}
		return nil
	}
	for _, declared := range inherited {
		if err := add(declared); err != nil {
			return nil, err
		}
	}
	for _, declared := range own.Bindings {
		if err := add(declared); err != nil {
			return nil, err
		}
	}
	table := make([]binding, 0, len(order))
	for _, key := range order {
		table = append(table, merged[key])
	}
	return table, nil
}

// inheritsBindings reports whether a mode starts from the default mode's
// bindings. A mode that owns the printable keys for text entry must not.
func inheritsBindings(mode ModeDef) bool {
	return mode.Inherit == nil || *mode.Inherit
}

// viewMetadata is the mode-specific payload the trusted renderer receives. It
// is the validated declarative data, not free-form application JSON.
type viewMetadata struct {
	Gutter        bool     `json:"gutter,omitempty"`
	CursorVisible bool     `json:"cursor_visible,omitempty"`
	Columns       []Column `json:"columns,omitempty"`
	Footer        string   `json:"footer,omitempty"`
	Fields        []Field  `json:"fields,omitempty"`
}

// metadata renders the view's declarative data for the renderer-facing view.
func (v ViewSpec) metadata() (json.RawMessage, error) {
	payload := viewMetadata{
		Gutter:        v.Gutter,
		CursorVisible: v.CursorVisible,
		Columns:       v.Columns,
		Footer:        v.Footer,
		Fields:        v.Fields,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, domain.WrapError(err, domain.CategoryInternal, domain.CodeSerializationFailed,
			"encoding interaction view metadata")
	}
	return raw, nil
}
