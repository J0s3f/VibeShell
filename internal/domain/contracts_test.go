package domain_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// schemaVersionExpectations maps each schema file to the domain constant it
// must agree with. Bumping a contract means changing both together.
var schemaVersionExpectations = map[string]int{
	"event-envelope.schema.json":       domain.EventContractVersion,
	"world-changeset.schema.json":      domain.ChangeSetContractVersion,
	"app-abi.schema.json":              domain.AppContractVersion,
	"model-request-result.schema.json": domain.ModelContractVersion,
	"configuration.schema.json":        domain.ConfigContractVersion,
	"route-policy.schema.json":         domain.RoutePolicyContractVersion,
}

func TestSchemaFilesAreValidJSONWithDraft202012(t *testing.T) {
	files, err := filepath.Glob("../../schemas/*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(schemaVersionExpectations) {
		t.Fatalf("expected %d schema files, found %d: %v", len(schemaVersionExpectations), len(files), files)
	}
	for _, f := range files {
		base := filepath.Base(f)
		wantVersion, known := schemaVersionExpectations[base]
		if !known {
			t.Errorf("schema file %s has no version expectation; register it", base)
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("reading %s: %v", base, err)
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s is not valid JSON: %v", base, err)
			continue
		}
		schema, _ := doc["$schema"].(string)
		if !strings.Contains(schema, "draft/2020-12") {
			t.Errorf("%s: $schema %q is not draft 2020-12", base, schema)
		}
		version, ok := doc["version"].(float64)
		if !ok || int(version) != wantVersion {
			t.Errorf("%s: version %v does not match domain constant %d", base, doc["version"], wantVersion)
		}
	}
}

// fixtureCases maps every representative fixture (PLAN 14) to the Go type it
// must unmarshal into. Coverage: empty output, Unicode, binary content
// reference, unknown command, newly created folder, shared-scope denial,
// concurrent conflict, quota error, cancelled generation, sandbox
// exception, incomplete transcript delivery.
var fixtureCases = map[string]func([]byte) (any, error){
	"empty-output.json":         unmarshalAs[domain.ModelResponse],
	"unicode.json":              unmarshalAs[domain.InputAcceptedPayload],
	"binary-content-ref.json":   unmarshalAs[domain.ContentRef],
	"unknown-command.json":      unmarshalAs[domain.AppEvent],
	"new-folder.json":           unmarshalAs[domain.ChangeSet],
	"shared-scope-denial.json":  unmarshalAs[domain.DomainError],
	"concurrent-conflict.json":  unmarshalAs[domain.WorldConflictPayload],
	"quota-error.json":          unmarshalAs[domain.ErrorEnvelope],
	"cancelled-generation.json": unmarshalAs[domain.ErrorEnvelope],
	"sandbox-exception.json":    unmarshalAs[domain.AppSandboxRunPayload],
	"incomplete-delivery.json":  unmarshalAs[domain.RetrievalResult],
}

func unmarshalAs[T any](raw []byte) (any, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func TestFixturesUnmarshalAndRoundTrip(t *testing.T) {
	files, err := filepath.Glob("../../schemas/fixtures/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(fixtureCases) {
		t.Fatalf("expected %d fixtures, found %d: %v", len(fixtureCases), len(files), files)
	}
	for _, f := range files {
		base := filepath.Base(f)
		unmarshal, known := fixtureCases[base]
		if !known {
			t.Errorf("fixture %s has no Go type mapping; register it", base)
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("reading %s: %v", base, err)
			continue
		}
		typed, err := unmarshal(raw)
		if err != nil {
			t.Errorf("%s does not unmarshal into its Go type: %v", base, err)
			continue
		}
		remarshalled, err := json.Marshal(typed)
		if err != nil {
			t.Errorf("%s does not remarshal: %v", base, err)
			continue
		}
		var want, got any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Errorf("%s: reparse of original failed: %v", base, err)
			continue
		}
		if err := json.Unmarshal(remarshalled, &got); err != nil {
			t.Errorf("%s: reparse of round-trip failed: %v", base, err)
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s does not round-trip:\n want %v\n got  %v", base, want, got)
		}
	}
}
