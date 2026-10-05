package contract_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	fixturesDir      = "fixtures"
	fixturesManifest = fixturesDir + "/manifest.json"
	repositoryRoot   = "../.."
)

// plan14EdgeCases is the fixture coverage PLAN 14 requires. Each name must
// appear exactly once in the fixture manifest.
var plan14EdgeCases = []string{
	"empty output",
	"Unicode",
	"binary content reference",
	"unknown command",
	"new folder",
	"shared-scope denial",
	"concurrent conflict",
	"quota error",
	"cancelled generation",
	"sandbox exception",
	"incomplete transcript delivery",
}

type fixturesManifestDoc struct {
	Version     int           `json:"version"`
	Description string        `json:"description"`
	Contracts   []contractDoc `json:"contracts"`
	EdgeCases   []edgeCaseDoc `json:"edge_cases"`
}

type contractDoc struct {
	Contract           string   `json:"contract"`
	PlanReference      string   `json:"plan_reference"`
	SchemaStatus       string   `json:"schema_status"`
	Schema             *string  `json:"schema"`
	RepresentativeType string   `json:"representative_type"`
	Fixtures           []string `json:"fixtures"`
	Note               string   `json:"note,omitempty"`
}

type edgeCaseDoc struct {
	Case             string   `json:"case"`
	OwnedFixtures    []string `json:"owned_fixtures"`
	ExistingFixtures []string `json:"existing_fixtures"`
	Note             string   `json:"note"`
}

func readFixturesManifest(t *testing.T) fixturesManifestDoc {
	t.Helper()
	raw, err := os.ReadFile(fixturesManifest)
	if err != nil {
		t.Fatalf("reading %s: %v", fixturesManifest, err)
	}
	doc, err := decodeAs[fixturesManifestDoc](raw)
	if err != nil {
		t.Fatalf("%s does not decode: %v", fixturesManifest, err)
	}
	return doc.(fixturesManifestDoc)
}

// TestContractFixturesDecodeAndRoundTrip checks that every registered fixture
// is valid JSON and decodes strictly into the representative structure of its
// contract, then round-trips without loss. Strict decoding is the stdlib
// equivalent of the additionalProperties:false the shipped schemas use.
func TestContractFixturesDecodeAndRoundTrip(t *testing.T) {
	doc := readFixturesManifest(t)
	if doc.Version != 1 {
		t.Errorf("fixture manifest version = %d, want 1", doc.Version)
	}
	if len(doc.Contracts) == 0 {
		t.Fatal("fixture manifest registers no contracts")
	}
	for _, contract := range doc.Contracts {
		decode, registered := representativeTypes[contract.RepresentativeType]
		if !registered {
			t.Errorf("contract %q names unregistered representative type %q; register it",
				contract.Contract, contract.RepresentativeType)
			continue
		}
		if len(contract.Fixtures) == 0 {
			t.Errorf("contract %q registers no fixtures", contract.Contract)
			continue
		}
		for _, name := range contract.Fixtures {
			checkFixtureRoundTrip(t, contract.Contract, name, contract.RepresentativeType, decode)
		}
	}
}

func checkFixtureRoundTrip(t *testing.T, contract, name, typeName string, decode func([]byte) (any, error)) {
	t.Helper()
	rel := filepath.Join(fixturesDir, filepath.FromSlash(name))
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Errorf("contract %q: reading %s: %v", contract, name, err)
		return
	}
	typed, err := decode(raw)
	if err != nil {
		t.Errorf("contract %q: %s does not decode into %s: %v", contract, name, typeName, err)
		return
	}
	checkRoundTrip(t, name, raw, typed)
}

func checkRoundTrip(t *testing.T, name string, raw []byte, typed any) {
	t.Helper()
	remarshalled, err := json.Marshal(typed)
	if err != nil {
		t.Errorf("%s does not remarshal: %v", name, err)
		return
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Errorf("%s: reparsing the original failed: %v", name, err)
		return
	}
	if err := json.Unmarshal(remarshalled, &got); err != nil {
		t.Errorf("%s: reparsing the round trip failed: %v", name, err)
		return
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s does not round-trip:\n want %v\n got  %v", name, want, got)
	}
}

// TestEveryContractFixtureIsRegistered keeps the directory and the manifest in
// step: a fixture nobody registers would never be checked.
func TestEveryContractFixtureIsRegistered(t *testing.T) {
	doc := readFixturesManifest(t)
	registered := map[string]bool{}
	for _, contract := range doc.Contracts {
		for _, name := range contract.Fixtures {
			registered[name] = true
		}
	}

	onDisk := map[string]bool{}
	err := filepath.WalkDir(fixturesDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".json") {
			return nil
		}
		rel, err := filepath.Rel(fixturesDir, path)
		if err != nil {
			return err
		}
		onDisk[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", fixturesDir, err)
	}
	delete(onDisk, "manifest.json")
	if len(onDisk) == 0 {
		t.Fatalf("no fixtures found under %s", fixturesDir)
	}
	for _, name := range sortedNames(onDisk) {
		if !registered[name] {
			t.Errorf("fixture %s is not registered in the manifest", name)
		}
	}
	for _, name := range sortedNames(registered) {
		if !onDisk[name] {
			t.Errorf("manifest registers fixture %s, which does not exist", name)
		}
	}
}

// TestContractManifestNamesShippedSchemas checks that a contract claiming a
// shipped schema points at a schema file that exists in the repository.
func TestContractManifestNamesShippedSchemas(t *testing.T) {
	doc := readFixturesManifest(t)
	for _, contract := range doc.Contracts {
		switch contract.SchemaStatus {
		case "shipped":
			if contract.Schema == nil || *contract.Schema == "" {
				t.Errorf("contract %q claims a shipped schema but names none", contract.Contract)
				continue
			}
			if _, err := os.Stat(filepath.Join(repositoryRoot, filepath.FromSlash(*contract.Schema))); err != nil {
				t.Errorf("contract %q names schema %s, which does not exist: %v", contract.Contract, *contract.Schema, err)
			}
		case "proposed":
			if contract.PlanReference == "" {
				t.Errorf("proposed contract %q has no plan_reference", contract.Contract)
			}
			if contract.Note == "" {
				t.Errorf("proposed contract %q has no note explaining why it is a proposal", contract.Contract)
			}
		default:
			t.Errorf("contract %q has unknown schema status %q", contract.Contract, contract.SchemaStatus)
		}
	}
}

// TestPlan14EdgeCasesAreCovered checks the coverage PLAN 14 lists, and that
// every edge case points at a registered fixture here and at an existing
// fragment under schemas/fixtures.
func TestPlan14EdgeCasesAreCovered(t *testing.T) {
	doc := readFixturesManifest(t)
	registered := map[string]bool{}
	for _, contract := range doc.Contracts {
		for _, name := range contract.Fixtures {
			registered[name] = true
		}
	}

	seen := map[string]bool{}
	for _, edgeCase := range doc.EdgeCases {
		if seen[edgeCase.Case] {
			t.Errorf("edge case %q is listed twice", edgeCase.Case)
		}
		seen[edgeCase.Case] = true
		if edgeCase.Note == "" {
			t.Errorf("edge case %q has no note explaining what it adds", edgeCase.Case)
		}
		if len(edgeCase.OwnedFixtures) == 0 {
			t.Errorf("edge case %q has no fixture here", edgeCase.Case)
		}
		if len(edgeCase.ExistingFixtures) == 0 {
			t.Errorf("edge case %q does not reference the earlier fragment under schemas/fixtures", edgeCase.Case)
		}
		for _, name := range edgeCase.OwnedFixtures {
			if !registered[name] {
				t.Errorf("edge case %q references unregistered fixture %s", edgeCase.Case, name)
			}
		}
		for _, name := range edgeCase.ExistingFixtures {
			if _, err := os.Stat(filepath.Join(repositoryRoot, filepath.FromSlash(name))); err != nil {
				t.Errorf("edge case %q references %s, which does not exist: %v", edgeCase.Case, name, err)
			}
		}
	}
	for _, want := range plan14EdgeCases {
		if !seen[want] {
			t.Errorf("PLAN 14 edge case %q has no fixture entry", want)
		}
		delete(seen, want)
	}
	for _, extra := range sortedNames(seen) {
		t.Errorf("edge case %q is not one of the PLAN 14 edge cases", extra)
	}
}

// TestFixturesContainNoCredentials keeps credentials out of fixtures, as the
// project rules require for fixtures, diagnostics and exports.
func TestFixturesContainNoCredentials(t *testing.T) {
	forbidden := []string{"password_hash", "api_key", "secret_value", "authorization", "bearer "}
	err := filepath.WalkDir(fixturesDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(raw))
		for _, needle := range forbidden {
			if strings.Contains(lower, needle) {
				rel, relErr := filepath.Rel(fixturesDir, path)
				if relErr != nil {
					return relErr
				}
				t.Errorf("fixture %s mentions %q", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", fixturesDir, err)
	}
}
