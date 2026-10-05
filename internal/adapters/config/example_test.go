package config

import (
	"os"
	"strings"
	"testing"
)

// TestShippedExampleValidates runs the 4-tier shipped example through the
// same parse and semantic validation the service applies at startup.
func TestShippedExampleValidates(t *testing.T) {
	raw, err := os.ReadFile("../../../examples/vibeshell.json")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{SecretDirs: []string{"/etc/vibeshell/secrets"}}
	cfg, err := Parse(raw, opts)
	if err != nil {
		t.Fatalf("example configuration rejected: %v", err)
	}
	if len(cfg.Tiers) != 4 {
		t.Fatalf("tiers = %d, want 4", len(cfg.Tiers))
	}
	want := []string{"named-free", "free-discovery", "go-native", "cheap-payg"}
	for i, name := range want {
		if cfg.Tiers[i].Name != name {
			t.Fatalf("tier %d = %q, want %q", i, cfg.Tiers[i].Name, name)
		}
	}

	// The same example must fail cleanly when a secret file is missing:
	// no file exists in the test container, so Load reports and no partial
	// snapshot escapes.
	loader := NewLoader("../../../examples", opts)
	if _, err := loader.Load(raw); err == nil {
		t.Fatal("Load accepted a configuration whose secret files do not exist")
	} else if strings.Contains(err.Error(), "FAKE") {
		t.Fatalf("Load error leaks a secret value: %v", err)
	}
	if err := loader.Validate(raw); err == nil {
		t.Fatal("Validate accepted a configuration whose password file does not exist")
	}
}
