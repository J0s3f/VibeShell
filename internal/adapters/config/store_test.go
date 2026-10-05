package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeClock struct{ now int64 }

func (c *fakeClock) NowUnixMilli() int64   { return c.now }
func (c *fakeClock) MonotonicNanos() int64 { return c.now * 1_000_000 }

func TestReloadAtomicity(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vibeshell.json")
	if err := os.WriteFile(cfgPath, []byte(minimalDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: 1000}
	loader := NewLoader(dir, Options{})
	store, err := OpenStore(clock, loader, cfgPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pin1 := store.Pin()
	if pin1.Version != 1 {
		t.Fatalf("first version = %d", pin1.Version)
	}

	// A broken reload must keep the previous snapshot and version.
	if err := os.WriteFile(cfgPath, []byte(`{"version": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err == nil {
		t.Fatal("invalid document accepted on reload")
	}
	if got := store.Pin(); got != pin1 || got.Version != 1 {
		t.Fatalf("pin changed after failed reload: %v vs %v", got, pin1)
	}
	if pin1.Config.Auth.Mode != AuthModePublic {
		t.Fatalf("pinned config mutated: %v", pin1.Config.Auth.Mode)
	}

	// A valid reload publishes a new snapshot; the pinned one stays intact.
	updated := strings.Replace(minimalDoc, `"hostname": "testhost"`, `"hostname": "secondhost"`, 1)
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	clock.now = 2000
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	pin2 := store.Pin()
	if pin2.Version != 2 || pin2.Config.Identity.Hostname != "secondhost" {
		t.Fatalf("reload published %+v", pin2)
	}
	if pin1.Version != 1 || pin1.Config.Identity.Hostname != "testhost" {
		t.Fatalf("pinned snapshot was rewritten: %+v", pin1.Config.Identity)
	}
	if pin2.LoadedAtUnixMilli != 2000 {
		t.Fatalf("loaded-at = %d", pin2.LoadedAtUnixMilli)
	}
}

func TestSnapshotFormatIsRedacted(t *testing.T) {
	s := Snapshot{
		Version: 3,
		Config:  &Config{Auth: &Auth{Mode: AuthModeSecure}, Sharing: &Sharing{Enabled: true}, Tiers: []Tier{{Name: "t"}}, Accounts: []Account{{ID: "acc_x"}}},
		secrets: map[string]Secret{"{env:K}": Secret([]byte("FAKE-PRIMARY-KEY-0001"))},
	}
	for _, fmtLiteral := range []string{fmt.Sprintf("%v", s), fmt.Sprintf("%v", &s), fmt.Sprintf("%+v", s), fmt.Sprintf("%s", s)} {
		if strings.Contains(fmtLiteral, "FAKE") {
			t.Fatalf("snapshot formatting leaks: %s", fmtLiteral)
		}
	}
	if got := fmt.Sprintf("%v", s); !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("snapshot formatting lost the redaction marker: %s", got)
	}
}
