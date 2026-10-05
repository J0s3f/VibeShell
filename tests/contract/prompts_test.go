package contract_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/presentation"
)

const (
	promptsDir      = "../../prompts"
	promptManifest  = promptsDir + "/manifest.json"
	utf8BOM         = "\xef\xbb\xbf"
	placeholderExpr = `\{\{[^{}]*\}\}`
)

// placeholderPattern matches one placeholder in a prompt file. A nested or
// unterminated brace does not match, so a malformed placeholder is reported by
// the balance check below rather than silently accepted.
var placeholderPattern = regexp.MustCompile(placeholderExpr)

// openBraces counts every "{{" in the text so an unbalanced placeholder is
// caught even when the placeholder pattern cannot match it.
func openBraces(text string) int { return strings.Count(text, "{{") }

// promptManifestDoc mirrors prompts/manifest.json. It is the administrator-facing
// inventory of prompt slots, their variables and their hard constraints.
type promptManifestDoc struct {
	Version           int             `json:"version"`
	PromptSet         string          `json:"prompt_set"`
	Description       string          `json:"description"`
	PlaceholderSyntax string          `json:"placeholder_syntax"`
	PlaceholderRules  []string        `json:"placeholder_rules"`
	HardConstraints   []string        `json:"hard_constraints"`
	Prompts           []promptDocSlot `json:"prompts"`
}

type promptDocSlot struct {
	ID            string           `json:"id"`
	File          string           `json:"file"`
	PlanReference string           `json:"plan_reference"`
	MaxLines      *int             `json:"max_lines,omitempty"`
	Purpose       string           `json:"purpose"`
	Variables     []promptDocEntry `json:"variables"`
}

type promptDocEntry struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Source         string `json:"source"`
	EmptyRendering string `json:"empty_rendering"`
}

func readPromptManifest(t *testing.T) promptManifestDoc {
	t.Helper()
	raw, err := os.ReadFile(promptManifest)
	if err != nil {
		t.Fatalf("reading %s: %v", promptManifest, err)
	}
	doc, err := decodeAs[promptManifestDoc](raw)
	if err != nil {
		t.Fatalf("%s does not decode: %v", promptManifest, err)
	}
	return doc.(promptManifestDoc)
}

// TestPromptManifestInventory checks the manifest header and that the set of
// prompt files on disk is exactly the set the manifest registers.
func TestPromptManifestInventory(t *testing.T) {
	doc := readPromptManifest(t)
	if doc.Version != 1 {
		t.Errorf("prompt manifest version = %d, want 1", doc.Version)
	}
	if doc.PromptSet == "" {
		t.Error("prompt manifest has no prompt_set label")
	}
	if doc.PlaceholderSyntax != "{{variable_name}}" {
		t.Errorf("placeholder syntax = %q, want {{variable_name}}", doc.PlaceholderSyntax)
	}
	if len(doc.Prompts) == 0 {
		t.Fatal("prompt manifest registers no prompts")
	}

	registered := map[string]bool{}
	ids := map[string]bool{}
	for _, slot := range doc.Prompts {
		if slot.ID == "" || slot.File == "" {
			t.Errorf("prompt slot %+v has no id or file", slot)
			continue
		}
		if ids[slot.ID] {
			t.Errorf("prompt slot %q is registered twice", slot.ID)
		}
		ids[slot.ID] = true
		if err := checkRelativePath(slot.File); err != nil {
			t.Errorf("prompt slot %q: %v", slot.ID, err)
			continue
		}
		registered[filepath.ToSlash(slot.File)] = true
		if slot.MaxLines != nil && *slot.MaxLines < 1 {
			t.Errorf("prompt slot %q: max_lines = %d, want at least 1", slot.ID, *slot.MaxLines)
		}
		if slot.Purpose == "" {
			t.Errorf("prompt slot %q has no purpose", slot.ID)
		}
		if slot.PlanReference == "" {
			t.Errorf("prompt slot %q has no plan_reference", slot.ID)
		}
	}

	onDisk, err := filepath.Glob(filepath.Join(promptsDir, "*", "*.txt"))
	if err != nil {
		t.Fatalf("globbing prompt files: %v", err)
	}
	if len(onDisk) == 0 {
		t.Fatalf("no prompt files found under %s", promptsDir)
	}
	for _, path := range onDisk {
		rel, err := filepath.Rel(promptsDir, path)
		if err != nil {
			t.Fatalf("relative path of %s: %v", path, err)
		}
		if !registered[filepath.ToSlash(rel)] {
			t.Errorf("prompt file %s is not registered in the manifest", rel)
		}
		delete(registered, filepath.ToSlash(rel))
	}
	for rel := range registered {
		t.Errorf("manifest registers %s, which does not exist", rel)
	}
}

// TestPromptFilesAreCleanPlainText checks the rendering constraints that apply
// to the shipped prompt files themselves: valid UTF-8, no byte-order mark, no
// backticks, and no terminal control sequences.
func TestPromptFilesAreCleanPlainText(t *testing.T) {
	doc := readPromptManifest(t)
	for _, slot := range doc.Prompts {
		if slot.File == "" {
			continue
		}
		if err := checkRelativePath(slot.File); err != nil {
			t.Errorf("prompt slot %q: %v", slot.ID, err)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(promptsDir, slot.File))
		if err != nil {
			t.Errorf("prompt slot %q: reading %s: %v", slot.ID, slot.File, err)
			continue
		}
		checkCleanPromptText(t, slot.ID, raw)
	}
}

func checkCleanPromptText(t *testing.T, id string, raw []byte) {
	t.Helper()
	name := id + " prompt"
	if !utf8.Valid(raw) {
		t.Errorf("%s is not valid UTF-8", name)
		return
	}
	text := string(raw)
	if strings.HasPrefix(text, utf8BOM) {
		t.Errorf("%s starts with a byte-order mark", name)
	}
	if strings.ContainsRune(text, '`') {
		t.Errorf("%s contains a backtick; the no-fence constraint forbids them", name)
	}
	if strings.TrimSpace(text) == "" {
		t.Errorf("%s is empty", name)
	}
	for offset, r := range text {
		switch {
		case r == '\n' || r == '\t':
		case r < 0x20 || r == 0x7f:
			t.Errorf("%s contains control character %U at byte offset %d", name, r, offset)
		case r >= 0x80 && r <= 0x9f:
			t.Errorf("%s contains C1 control character %U at byte offset %d", name, r, offset)
		}
	}
}

// TestPromptPlaceholdersAreDocumented checks that each prompt file uses only
// documented placeholders and that every documented variable is really used,
// so the documentation cannot drift away from the shipped text.
func TestPromptPlaceholdersAreDocumented(t *testing.T) {
	doc := readPromptManifest(t)
	for _, slot := range doc.Prompts {
		if slot.File == "" {
			continue
		}
		if err := checkRelativePath(slot.File); err != nil {
			t.Errorf("prompt slot %q: %v", slot.ID, err)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(promptsDir, slot.File))
		if err != nil {
			t.Errorf("prompt slot %q: reading %s: %v", slot.ID, slot.File, err)
			continue
		}
		checkPromptPlaceholders(t, slot, string(raw))
	}
}

func checkPromptPlaceholders(t *testing.T, slot promptDocSlot, text string) {
	documented := map[string]bool{}
	for _, variable := range slot.Variables {
		if variable.Name == "" {
			t.Errorf("prompt slot %q has a variable with no name", slot.ID)
			continue
		}
		if documented[variable.Name] {
			t.Errorf("prompt slot %q documents %q twice", slot.ID, variable.Name)
		}
		documented[variable.Name] = true
		if variable.Description == "" || variable.Source == "" || variable.EmptyRendering == "" {
			t.Errorf("prompt slot %q: variable %q needs a description, a source and an empty rendering",
				slot.ID, variable.Name)
		}
	}

	// The generic renderer must reproduce every documented empty rendering for
	// this slot, so a prompt never reaches the model with a raw placeholder.
	empty := map[string]string{}
	for _, variable := range slot.Variables {
		empty[variable.Name] = variable.EmptyRendering
	}
	for _, variable := range slot.Variables {
		rendered, err := presentation.RenderPrompt("{{"+variable.Name+"}}", nil, empty)
		if err != nil {
			t.Errorf("prompt slot %q: variable %q is not renderable by the runtime: %v",
				slot.ID, variable.Name, err)
		} else if rendered != variable.EmptyRendering {
			t.Errorf("prompt slot %q: variable %q renders %q, manifest documents %q",
				slot.ID, variable.Name, rendered, variable.EmptyRendering)
		}
	}
	// The wired MOTD slot must carry exactly the manifest's MOTD table.
	if slot.ID == "motd" {
		for _, variable := range slot.Variables {
			rendered, err := presentation.RenderMOTDPrompt("{{"+variable.Name+"}}", nil)
			if err != nil {
				t.Errorf("MOTD variable %q is not renderable by the wired renderer: %v", variable.Name, err)
			} else if rendered != variable.EmptyRendering {
				t.Errorf("MOTD variable %q renders %q, manifest documents %q",
					variable.Name, rendered, variable.EmptyRendering)
			}
		}
	}

	matches := placeholderPattern.FindAllString(text, -1)
	used := map[string]bool{}
	for _, match := range matches {
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(match, "{{"), "}}"))
		used[name] = true
		if !documented[name] {
			t.Errorf("prompt slot %q uses undocumented placeholder {{%s}}", slot.ID, name)
		}
	}
	for name := range documented {
		if !used[name] {
			t.Errorf("prompt slot %q documents %q but never uses it", slot.ID, name)
		}
	}
	if closed := strings.Count(text, "}}"); closed != len(matches) {
		t.Errorf("prompt slot %q has %d closing braces for %d placeholders", slot.ID, closed, len(matches))
	}
	if open, closed := openBraces(text), strings.Count(text, "}}"); open != closed {
		t.Errorf("prompt slot %q has %d opening and %d closing placeholder braces", slot.ID, open, closed)
	}
}

// TestPromptHardConstraintsAreDocumented checks that the manifest still states
// the rendering rules and the rule that an edited prompt never gains authority.
func TestPromptHardConstraintsAreDocumented(t *testing.T) {
	doc := readPromptManifest(t)
	if len(doc.HardConstraints) == 0 {
		t.Fatal("prompt manifest states no hard constraints")
	}
	if len(doc.PlaceholderRules) == 0 {
		t.Error("prompt manifest states no placeholder rules")
	}
	stated := strings.ToLower(strings.Join(doc.HardConstraints, "\n"))
	for _, rule := range []string{"markdown fences", "model commentary", "terminal control sequence", "permission"} {
		if !strings.Contains(stated, rule) {
			t.Errorf("prompt manifest hard constraints do not mention %q", rule)
		}
	}
}

// TestPromptsDirectoryHasNoStrayFiles keeps the prompt set to files the
// administrator can reason about: prompt text, the manifest, and documentation.
func TestPromptsDirectoryHasNoStrayFiles(t *testing.T) {
	allowed := map[string]bool{".txt": true, ".json": true, ".md": true}
	err := filepath.WalkDir(promptsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !allowed[strings.ToLower(filepath.Ext(path))] {
			rel, relErr := filepath.Rel(promptsDir, path)
			if relErr != nil {
				return relErr
			}
			t.Errorf("%s is not a prompt file, manifest, or documentation", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", promptsDir, err)
	}
}

// checkRelativePath rejects a manifest path that escapes the directory it is
// relative to, so a prompt can never be loaded from outside its own tree.
func checkRelativePath(rel string) error {
	switch {
	case rel == "":
		return errors.New("path is empty")
	case filepath.IsAbs(rel):
		return errors.New("path must be relative")
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("path escapes its directory")
	}
	return nil
}

// sortedNames returns the sorted keys of a set, for stable failure output.
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
