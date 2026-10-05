package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreActiveIsNilBeforeFirstPublication(t *testing.T) {
	loader := Loader{SecretsDir: t.TempDir(), Catalogue: ExampleCatalogue()}
	if store := NewStore(loader); store.Active() != nil {
		t.Fatal("Active() must be nil before the first publication")
	}
}

func TestStorePublishesAValidCandidate(t *testing.T) {
	store, _ := newTestStore(t)
	snap := store.Active()
	if snap == nil {
		t.Fatal("Active() is nil after a successful load")
	}
	if snap.Version() != 1 {
		t.Fatalf("version = %d, want 1", snap.Version())
	}
	if snap.AuthMode() != AuthModeSecure {
		t.Fatalf("auth mode = %q, want %q", snap.AuthMode(), AuthModeSecure)
	}
	if !snap.SharingEnabled() {
		t.Fatal("sharing should be enabled")
	}
	wantTiers := []string{"named-free", "free-discovery", "go-native", "cheap-payg"}
	if got := snap.TierNames(); !slices.Equal(got, wantTiers) {
		t.Fatalf("tier names = %v, want %v", got, wantTiers)
	}
	wantFree := []string{"opencode/ling-3.1-flash-free", "opencode/deepseek-v4.1-flash-free", "opencode/qwen3.6-flash-free"}
	if got, ok := snap.TierRoutes("free-discovery"); !ok || !slices.Equal(got, wantFree) {
		t.Fatalf("discovery tier routes = %v (ok=%t), want %v", got, ok, wantFree)
	}
	if got, ok := snap.TierRoutes("go-native"); !ok || !slices.Equal(got, []string{"opencode-go/ling-3.1-flash", "opencode-go/deepseek-v4.1-flash"}) {
		t.Fatalf("go-native routes = %v (ok=%t)", got, ok)
	}
	if _, ok := snap.TierRoutes("no-such-tier"); ok {
		t.Fatal("TierRoutes reported an unknown tier")
	}
	wantAccounts := []string{"opencode-primary", "zen-payg"}
	if got := snap.AccountIDs(); !slices.Equal(got, wantAccounts) {
		t.Fatalf("account IDs = %v, want %v", got, wantAccounts)
	}
	secret, ok := snap.Secret("opencode-primary")
	if !ok {
		t.Fatal("account secret missing")
	}
	if secret.Value() != "FAKE-PRIMARY-KEY-0001" {
		t.Fatal("resolved secret value mismatch")
	}
	if secret.String() != "[redacted]" {
		t.Fatalf("snapshot secret redaction broken: %q", secret.String())
	}
}

// TestStoreRejectsInvalidCandidateWithoutReplacingActive
// proves a candidate replaces the active snapshot only
// when fully valid: a parse error, a semantic error,
// and an unresolvable secret all leave the published
// snapshot untouched.
func TestStoreRejectsInvalidCandidateWithoutReplacingActive(t *testing.T) {
	store, dir := newTestStore(t)
	before := store.Active()

	// 1. Unknown field: strict decoding fails.
	unknownField := filepath.Join(dir, "unknown-field.json")
	data := []byte(`{"version":1,"identity":{"system_name":"VibeOS","shell_name":"VibeShell","hostname":"h"},"ssh":{"listen_port":2222,"host_key_file":"k"},"auth":{"mode":"secure","password_file":"p"},"sharing":{"enabled":true,"policy_revision":1},"tiers":[{"name":"t","routes":["opencode/ling-3.1-flash-free"]}],"persistence":{"database_path":"db","backup_dir":"bak"},"surprise":true}`)
	if err := os.WriteFile(unknownField, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadFile(unknownField); err == nil {
		t.Fatal("document with an unknown field published")
	} else if want := `unknown field "surprise"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
	assertSameSnapshot(t, store, before)

	// 2. Semantic violation: duplicate account IDs.
	duplicate := validConfig()
	duplicate.Accounts = append(slices.Clone(duplicate.Accounts), duplicate.Accounts[0])
	dupPath := filepath.Join(dir, "duplicate.json")
	writeConfigFile(t, dupPath, duplicate)
	if _, err := store.LoadFile(dupPath); err == nil {
		t.Fatal("document with duplicate account IDs published")
	} else if want := `duplicate account ID "opencode-primary"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
	assertSameSnapshot(t, store, before)

	// 3. Unresolvable secret reference.
	unresolved := validConfig()
	unresolved.Accounts[0].SecretRef = "{env:VIBESHELL_NOT_SET_ANYWHERE}"
	unresolvedPath := filepath.Join(dir, "unresolved.json")
	writeConfigFile(t, unresolvedPath, unresolved)
	if _, err := store.LoadFile(unresolvedPath); err == nil {
		t.Fatal("document with an unresolvable secret published")
	} else if want := `environment variable "VIBESHELL_NOT_SET_ANYWHERE" is not set`; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
	assertSameSnapshot(t, store, before)
}

// TestTurnPinsItsSnapshot proves a turn that pinned a
// snapshot keeps seeing that view after a later reload
// published a new one.
func TestTurnPinsItsSnapshot(t *testing.T) {
	store, dir := newTestStore(t)

	pin := store.Active()
	if pin.Version() != 1 {
		t.Fatalf("pinned version = %d, want 1", pin.Version())
	}
	if pin.AuthMode() != AuthModeSecure {
		t.Fatalf("pinned auth mode = %q", pin.AuthMode())
	}

	// A later, different configuration: public auth,
	// a renamed first tier, a rotated secret file.
	v2 := validConfig()
	v2.Auth.Mode = AuthModePublic
	v2.Auth.PasswordFile = ""
	v2.Tiers[0].Name = "named-free-v2"
	v2.Tiers[0].Routes = []string{"opencode/deepseek-v4.1-flash-free"}
	v2.Accounts[0].SecretRef = "{file:accounts/rotated.key}"
	v2Path := filepath.Join(dir, "v2.json")
	writeConfigFile(t, v2Path, v2)
	published, err := store.LoadFile(v2Path)
	if err != nil {
		t.Fatalf("valid v2 rejected: %v", err)
	}
	if published.Version() != 2 {
		t.Fatalf("v2 version = %d, want 2", published.Version())
	}

	// The pinned view is unchanged.
	if pin.Version() != 1 {
		t.Fatalf("pinned version changed to %d", pin.Version())
	}
	if pin.AuthMode() != AuthModeSecure {
		t.Fatalf("pinned auth mode changed to %q", pin.AuthMode())
	}
	if names := pin.TierNames(); !slices.Equal(names, []string{"named-free", "free-discovery", "go-native", "cheap-payg"}) {
		t.Fatalf("pinned tier names changed to %v", names)
	}
	if routes, ok := pin.TierRoutes("named-free"); !ok || !slices.Equal(routes, []string{"opencode/ling-3.1-flash-free", "opencode/deepseek-v4.1-flash-free", "opencode/qwen3.6-flash-free"}) {
		t.Fatalf("pinned named-free routes changed to %v (ok=%t)", routes, ok)
	}
	if secret, ok := pin.Secret("opencode-primary"); !ok || secret.Value() != "FAKE-PRIMARY-KEY-0001" {
		t.Fatal("pinned secret changed")
	}

	// The active view is the new one.
	active := store.Active()
	if active.Version() != 2 {
		t.Fatalf("active version = %d, want 2", active.Version())
	}
	if active.AuthMode() != AuthModePublic {
		t.Fatalf("active auth mode = %q, want public", active.AuthMode())
	}
	if routes, ok := active.TierRoutes("named-free-v2"); !ok || !slices.Equal(routes, []string{"opencode/deepseek-v4.1-flash-free"}) {
		t.Fatalf("active named-free-v2 routes = %v (ok=%t)", routes, ok)
	}
	if secret, ok := active.Secret("opencode-primary"); !ok || secret.Value() != "FAKE-PRIMARY-KEY-0002" {
		t.Fatal("active secret should be the rotated value")
	}
}

// TestStoreConcurrentReadersDuringReload exercises the
// store under the race detector: readers pin and read
// snapshots while publications swap the active one.
func TestStoreConcurrentReadersDuringReload(t *testing.T) {
	store, dir := newTestStore(t)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := store.Active()
				if snap == nil {
					continue
				}
				_ = snap.Version()
				_ = snap.AuthMode()
				_ = snap.SharingEnabled()
				_ = snap.TierNames()
				_, _ = snap.TierRoutes("named-free")
				_, _ = snap.Secret("opencode-primary")
				time.Sleep(50 * time.Microsecond)
			}
		}()
	}

	const reloads = 25
	for i := 0; i < reloads; i++ {
		cfg := validConfig()
		cfg.Auth.MaxAttemptsPerConn = 1 + i%10
		path := filepath.Join(dir, fmt.Sprintf("reload-%d.json", i+2))
		writeConfigFile(t, path, cfg)
		if _, err := store.LoadFile(path); err != nil {
			t.Fatalf("reload %d failed: %v", i+2, err)
		}
	}
	close(stop)
	readers.Wait()

	if got := store.Active().Version(); got != reloads+1 {
		t.Fatalf("active version = %d, want %d", got, reloads+1)
	}
}

// TestSnapshotRedactsSecretsInFormatting proves that
// formatting a snapshot cannot leak secret values,
// even though the snapshot holds them. fmt calls
// Snapshot.String for every verb at the top level,
// for pointer and value forms alike.
func TestSnapshotRedactsSecretsInFormatting(t *testing.T) {
	store, _ := newTestStore(t)
	snap := store.Active()

	for _, rendered := range []string{
		fmt.Sprintf("%v", snap),
		fmt.Sprintf("%v", *snap),
		fmt.Sprintf("%+v", snap),
		fmt.Sprintf("%s", snap),
	} {
		if want := "[redacted]"; !strings.Contains(rendered, want) {
			t.Fatalf("snapshot formatting does not redact secrets: %s", rendered)
		}
		for _, marker := range []string{"FAKE-PRIMARY-KEY-0001", "FAKE-ZEN-KEY-0002"} {
			if strings.Contains(rendered, marker) {
				t.Fatalf("snapshot formatting leaked %q: %s", marker, rendered)
			}
		}
	}

	// A secret map printed at the top level also
	// redacts, through Secret.String.
	topLevel := map[string]Secret{"account": {value: "FAKE-PRIMARY-KEY-0001"}}
	if rendered := fmt.Sprintf("%v", topLevel); strings.Contains(rendered, "FAKE-PRIMARY-KEY-0001") {
		t.Fatalf("secret map formatting leaked: %s", rendered)
	}
}

func assertSameSnapshot(t *testing.T, store *Store, before *Snapshot) {
	t.Helper()
	after := store.Active()
	if after != before {
		t.Fatalf("active snapshot changed from version %d to %d after a rejected candidate", before.Version(), after.Version())
	}
}
