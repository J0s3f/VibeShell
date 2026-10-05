package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const examplePath = "examples/vibeshell.conf.json"

// TestShippedExampleLoads loads the documented example
// configuration end to end: strict parsing, semantic
// validation, prompt-file existence, and secret
// resolution. It also verifies the four example tiers
// resolve to exactly the routes PLAN 11 asks the
// example to demonstrate.
func TestShippedExampleLoads(t *testing.T) {
	secretsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(secretsDir, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "accounts", "opencode-primary.key"), []byte("FAKE-OPENCODE-KEY-0001"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIBESHELL_ZEN_API_KEY_FAKE", "FAKE-ZEN-KEY-0002")

	loader := Loader{SecretsDir: secretsDir, Catalogue: ExampleCatalogue()}
	store := NewStore(loader)
	snap, err := store.LoadFile(examplePath)
	if err != nil {
		t.Fatalf("shipped example failed to load: %v", err)
	}

	if snap.Version() != 1 {
		t.Fatalf("version = %d, want 1", snap.Version())
	}
	if snap.AuthMode() != AuthModeSecure {
		t.Fatalf("auth mode = %q", snap.AuthMode())
	}
	wantTiers := []string{"named-free", "free-discovery", "go-native", "cheap-payg"}
	if got := snap.TierNames(); !slices.Equal(got, wantTiers) {
		t.Fatalf("tier names = %v, want %v", got, wantTiers)
	}

	freeRoutes := []string{"opencode/ling-3.1-flash-free", "opencode/deepseek-v4.1-flash-free", "opencode/qwen3.6-flash-free"}
	checkTier := func(name string, want []string) {
		t.Helper()
		got, ok := snap.TierRoutes(name)
		if !ok {
			t.Fatalf("tier %q is missing", name)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("tier %q routes = %v, want %v", name, got, want)
		}
	}

	// Tier 1: named free routes, exactly as listed.
	checkTier("named-free", freeRoutes)
	// Tier 2: suitable-free discovery of every
	// opencode route whose ID ends in -free, in
	// catalogue order.
	checkTier("free-discovery", freeRoutes)
	// Tier 3: Go-native routes.
	checkTier("go-native", []string{"opencode-go/ling-3.1-flash", "opencode-go/deepseek-v4.1-flash"})
	// Tier 4: explicitly allowed cheap pay-as-you-go
	// routes.
	checkTier("cheap-payg", []string{"zen/gpt-5.1-nano", "zen/minimax-m3-turbo"})

	wantAccounts := []string{"opencode-primary", "zen-payg"}
	if got := snap.AccountIDs(); !slices.Equal(got, wantAccounts) {
		t.Fatalf("account IDs = %v, want %v", got, wantAccounts)
	}
	secret, ok := snap.Secret("opencode-primary")
	if !ok || secret.Value() != "FAKE-OPENCODE-KEY-0001" {
		t.Fatal("file-backed account secret did not resolve")
	}
	if secret, ok := snap.Secret("zen-payg"); !ok || secret.Value() != "FAKE-ZEN-KEY-0002" {
		t.Fatal("env-backed account secret did not resolve")
	}
}

// TestShippedExampleRejectsTampering copies the shipped
// example and proves a single added unknown field fails
// strict parsing, so the example cannot silently
// accumulate undocumented fields.
func TestShippedExampleRejectsTampering(t *testing.T) {
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.TrimSuffix(string(data), "\n")
	tampered = strings.TrimSuffix(tampered, "}") + `,  "surprise": true` + "\n}\n"
	path := filepath.Join(t.TempDir(), "tampered.conf.json")
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := Loader{SecretsDir: t.TempDir(), Catalogue: ExampleCatalogue()}
	if _, err := loader.Load(path); err == nil {
		t.Fatal("tampered example loaded")
	} else if want := `unknown field "surprise"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// TestShippedExampleRejectsMissingPromptFile copies the
// example and removes one prompt file, proving missing
// prompt paths fail the load with an actionable error.
func TestShippedExampleRejectsMissingPromptFile(t *testing.T) {
	src := "examples"
	dst := t.TempDir()
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dst, "prompts", "repair.md")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	secretsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(secretsDir, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "accounts", "opencode-primary.key"), []byte("FAKE-OPENCODE-KEY-0001"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIBESHELL_ZEN_API_KEY_FAKE", "FAKE-ZEN-KEY-0002")

	loader := Loader{SecretsDir: secretsDir, Catalogue: ExampleCatalogue()}
	_, err := loader.Load(filepath.Join(dst, "vibeshell.conf.json"))
	if err == nil {
		t.Fatal("example with a missing prompt file loaded")
	}
	if want := "prompts.repair"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name the missing prompt path", err)
	}
}

// TestShippedExampleRejectsPromptOutsideConfigDir copies
// the example and points one prompt path outside the
// configuration directory.
func TestShippedExampleRejectsPromptOutsideConfigDir(t *testing.T) {
	src := "examples"
	dst := t.TempDir()
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "vibeshell.conf.json"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "escape.md")
	if err := os.WriteFile(outside, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"repair": "prompts/repair.md"`, `"repair": "`+outside+`"`, 1)
	if tampered == string(data) {
		t.Fatal("fixture: repair path not found in the example")
	}
	if err := os.WriteFile(filepath.Join(dst, "vibeshell.conf.json"), []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	loader := Loader{SecretsDir: t.TempDir(), Catalogue: ExampleCatalogue()}
	if _, err := loader.Load(filepath.Join(dst, "vibeshell.conf.json")); err == nil {
		t.Fatal("example with an escaping prompt path loaded")
	}
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		s := filepath.Join(src, entry.Name())
		d := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return err
			}
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
