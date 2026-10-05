package contract_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// appABIContract is the contract whose representative type carries the
// line-based interactive fields.
const appABIContract = "app-abi"

// appABIFixtures returns the fixture names the fixture manifest registers for
// the app-abi contract. A contract with no fixtures is a manifest bug, so the
// test that reads them fails rather than silently checking nothing.
func appABIFixtures(t *testing.T) []string {
	t.Helper()
	doc := readFixturesManifest(t)
	for _, contract := range doc.Contracts {
		if contract.Contract == appABIContract {
			return contract.Fixtures
		}
	}
	t.Fatalf("the fixture manifest registers no %q contract", appABIContract)
	return nil
}

func readAppExchange(t *testing.T, name string) AppExchange {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixturesDir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	exchange, err := decodeAs[AppExchange](raw)
	if err != nil {
		t.Fatalf("fixture %s does not decode into AppExchange: %v", name, err)
	}
	return exchange.(AppExchange)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return raw
}

// TestAppABIContinuationFieldsAreOptional checks the optional half of the
// line-based interactive shape across the registered app-abi fixtures: a
// one-shot app leaves awaiting_input and prompt out and re-marshals without
// them, and a fixture that declares them keeps exactly what it declared. It also
// requires both cases to exist, so the coverage cannot be lost by editing the
// fixtures.
func TestAppABIContinuationFieldsAreOptional(t *testing.T) {
	var oneShot, interactive int
	for _, name := range appABIFixtures(t) {
		exchange := readAppExchange(t, name)
		result := exchange.Result

		if result.AwaitingInput == nil && result.Prompt == nil {
			oneShot++
		} else {
			interactive++
			if result.AwaitingInput != nil && !*result.AwaitingInput {
				t.Errorf("fixture %s declares awaiting_input false; the absent form is how a one-shot app says so", name)
			}
			if result.Prompt != nil && len(*result.Prompt) > domain.MaxAppPromptBytes {
				t.Errorf("fixture %s prompt is %d bytes, past the %d-byte bound", name, len(*result.Prompt), domain.MaxAppPromptBytes)
			}
		}

		remarshalled, err := json.Marshal(exchange)
		if err != nil {
			t.Fatalf("fixture %s does not remarshal: %v", name, err)
		}
		for _, key := range []struct {
			name     string
			declared bool
		}{
			{`"awaiting_input"`, result.AwaitingInput != nil},
			{`"prompt"`, result.Prompt != nil},
		} {
			if got := bytes.Contains(remarshalled, []byte(key.name)); got != key.declared {
				t.Errorf("fixture %s re-marshals with %s present = %v, want %v (result %s)",
					name, key.name, got, key.declared, remarshalled)
			}
		}
	}
	if oneShot == 0 {
		t.Error("no app-abi fixture covers an app that never awaits input")
	}
	if interactive == 0 {
		t.Errorf("no app-abi fixture exercises awaiting_input or prompt")
	}
}

// TestAppABIFixturesDecodeIntoTheDomainABI checks that every registered app-abi
// fixture is also a document the shipped domain types accept, so the schema, the
// representative structures and domain.AppResult cannot drift apart.
func TestAppABIFixturesDecodeIntoTheDomainABI(t *testing.T) {
	for _, name := range appABIFixtures(t) {
		exchange := readAppExchange(t, name)

		var manifest domain.AppManifest
		if err := json.Unmarshal(mustMarshal(t, exchange.Artifact.Manifest), &manifest); err != nil {
			t.Errorf("fixture %s manifest does not decode into domain.AppManifest: %v", name, err)
			continue
		}
		if err := domain.ValidateManifest(manifest); err != nil {
			t.Errorf("fixture %s manifest is not a valid domain.AppManifest: %v", name, err)
		}

		var result domain.AppResult
		if err := json.Unmarshal(mustMarshal(t, exchange.Result), &result); err != nil {
			t.Errorf("fixture %s result does not decode into domain.AppResult: %v", name, err)
			continue
		}
		if err := domain.ValidateResult(result); err != nil {
			t.Errorf("fixture %s result is not a valid domain.AppResult: %v", name, err)
		}
	}
}

// TestAppABIContinuationFieldsValidation checks the rules the two new fields
// exist to enforce, exercised through the JSON the ABI carries: an app cannot
// ask for another line and for termination at once, and the continuation prompt
// stays within the bound the terminal layer displays.
func TestAppABIContinuationFieldsValidation(t *testing.T) {
	// decode builds a result document from the fixed base a valid result needs
	// plus the fields a case is about, so each case states only its own shape.
	decode := func(t *testing.T, fields string) domain.AppResult {
		t.Helper()
		var result domain.AppResult
		raw := `{"new_state":{},"view":{"mode":"text"},"exited":false` + fields + `}`
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatalf("%s does not decode into domain.AppResult: %v", raw, err)
		}
		return result
	}

	accepted := []struct {
		name   string
		fields string
	}{
		{"one-shot", ``},
		{"awaiting input with a prompt", `,"awaiting_input":true,"prompt":"grove> "`},
		{"prompt at the bound", `,"prompt":"` + strings.Repeat(">", domain.MaxAppPromptBytes) + `"`},
	}
	for _, tc := range accepted {
		if err := domain.ValidateResult(decode(t, tc.fields)); err != nil {
			t.Errorf("ValidateResult(%s) = %v, want success", tc.name, err)
		}
	}

	rejected := []struct {
		name   string
		fields string
	}{
		{"awaiting input with exited", `,"awaiting_input":true,"exited":true`},
		{"prompt past the bound", `,"prompt":"` + strings.Repeat(">", domain.MaxAppPromptBytes+1) + `"`},
	}
	for _, tc := range rejected {
		err := domain.ValidateResult(decode(t, tc.fields))
		if err == nil {
			t.Errorf("ValidateResult(%s) = nil, want error", tc.name)
			continue
		}
		if !domain.IsValidationError(err) {
			t.Errorf("ValidateResult(%s) error %v is not a validation error", tc.name, err)
		}
	}
}

// TestAppABISchemaDeclaresOptionalContinuationFields pins the schema side of the
// two fields: the result object must declare them with the shapes the Go type
// uses, must not require them, and must keep refusing unknown properties. The
// representative structures decode with unknown fields disallowed, so a schema
// that dropped one of these would let a real document and the shipped contract
// disagree without any other test noticing.
func TestAppABISchemaDeclaresOptionalContinuationFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repositoryRoot, "schemas", "app-abi.schema.json"))
	if err != nil {
		t.Fatalf("reading schemas/app-abi.schema.json: %v", err)
	}
	var schema struct {
		Defs map[string]schemaNode `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schemas/app-abi.schema.json is not valid JSON: %v", err)
	}
	result, known := schema.Defs["appResult"]
	if !known {
		t.Fatal("schemas/app-abi.schema.json has no appResult definition")
	}
	if string(result.AdditionalProperties) != "false" {
		t.Errorf("appResult additionalProperties = %s, want false", result.AdditionalProperties)
	}

	fields := []struct {
		name string
		want schemaShape
	}{
		{"awaiting_input", schemaShape{typ: "boolean"}},
		{"prompt", schemaShape{typ: "string", maxLength: domain.MaxAppPromptBytes}},
	}
	for _, field := range fields {
		got, declared := result.Properties[field.name]
		if !declared {
			t.Errorf("appResult does not declare %s; the ABI and the schema would disagree", field.name)
			continue
		}
		if got.Type != field.want.typ || got.MaxLength != field.want.maxLength {
			t.Errorf("appResult %s is type %q maxLength %d, want type %q maxLength %d",
				field.name, got.Type, got.MaxLength, field.want.typ, field.want.maxLength)
		}
		for _, required := range result.Required {
			if required == field.name {
				t.Errorf("appResult requires %s; the continuation fields are optional", field.name)
			}
		}
	}
}

// schemaNode is the part of a JSON Schema node these tests read: enough to
// check a declared property's type and its length bound, and whether the node
// refuses unknown properties.
type schemaNode struct {
	Type                 string                `json:"type"`
	MaxLength            int                   `json:"maxLength"`
	Required             []string              `json:"required"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
	Properties           map[string]schemaNode `json:"properties"`
}

// schemaShape is the comparable subset of a schemaNode one property must match.
// A node carries slices and maps, so only the two facts these tests pin are
// kept here.
type schemaShape struct {
	typ       string
	maxLength int
}
