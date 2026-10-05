package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// hotReloadDoc is a minimal valid document whose hot groups (tiers, sharing)
// and restart-required group (ssh persistence) can be changed independently,
// so a reload test can prove which changes took effect.
const hotReloadDoc = `{
  "version": 1,
  "identity": {"system_name": "VibeOS", "shell_name": "VibeShell", "hostname": "testhost"},
  "ssh": {"listen_port": 2222, "host_key_file": "/etc/vibeshell/host_key"},
  "auth": {"mode": "public"},
  "sharing": {"enabled": false},
  "routes": [
    {"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "m1"}
  ],
  "tiers": [
    {"name": "t1", "routes": ["rte_0123456789ABCDEFGHJKMNPQRS"]}
  ],
  "persistence": {"database_path": "/var/lib/vibeshell/world.db"}
}`

// openReloadStore writes doc to a fresh temp file and opens a store on it.
func openReloadStore(t *testing.T, doc string) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "vibeshell.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(&fakeClock{now: 1000}, NewLoader(dir, Options{}), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store, path
}

func TestClassifyFieldBoundary(t *testing.T) {
	cases := []struct {
		key  string
		want FieldClass
	}{
		{"tiers", FieldHotReloadable},
		{"accounts", FieldHotReloadable},
		{"prompts", FieldHotReloadable},
		{"sharing.enabled", FieldHotReloadable},
		{"health", FieldHotReloadable},
		{"inference", FieldHotReloadable},
		{"limits.turn_deadline_ms", FieldHotReloadable},
		{"ssh.listen_address", FieldRestartRequired},
		{"ssh.listen_port", FieldRestartRequired},
		{"persistence.database_path", FieldRestartRequired},
		{"apps.engine_abi_version", FieldRestartRequired},
		{"identity", FieldStatic},
		{"auth", FieldStatic},
		{"not_a_group", FieldStatic},
	}
	for _, c := range cases {
		if got := ClassifyField(c.key); got != c.want {
			t.Errorf("ClassifyField(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestClassifyConfigReportsGroups(t *testing.T) {
	cfg, err := Parse([]byte(hotReloadDoc), Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := ClassifyConfig(cfg)

	wantHot := map[string]bool{"sharing": true, "tiers": true, "routes": true}
	if !sameSet(got.HotReloadable, wantHot) {
		t.Errorf("hot = %v, want groups %v", got.HotReloadable, wantHot)
	}
	wantRestart := map[string]bool{"ssh": true, "persistence": true}
	if !sameSet(got.RestartRequired, wantRestart) {
		t.Errorf("restart = %v, want %v", got.RestartRequired, wantRestart)
	}
	wantStatic := map[string]bool{"identity": true, "auth": true}
	if !sameSet(got.Static, wantStatic) {
		t.Errorf("static = %v, want %v", got.Static, wantStatic)
	}
}

func TestReloadPublishesValidatedHotFields(t *testing.T) {
	store, path := openReloadStore(t, hotReloadDoc)
	first := store.Pin()
	if first.Version != 1 {
		t.Fatalf("first version = %d", first.Version)
	}

	// Change a hot field (sharing) and a restart-required field (db path).
	updated := strings.Replace(hotReloadDoc, `"sharing": {"enabled": false}`, `"sharing": {"enabled": true}`, 1)
	updated = strings.Replace(updated, `"/var/lib/vibeshell/world.db"`, `"/var/lib/vibeshell/other.db"`, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	pinned := store.Pin()
	if pinned.Version != 2 {
		t.Fatalf("reloaded version = %d", pinned.Version)
	}
	if !pinned.Config.Sharing.Enabled {
		t.Fatal("hot field sharing.enabled was not published")
	}
	if pinned.Reload == nil {
		t.Fatal("reload report missing")
	}
	if !contains(pinned.Reload.HotApplied, "sharing") {
		t.Errorf("sharing not reported hot-applied: %v", pinned.Reload.HotApplied)
	}
	if !contains(pinned.Reload.PendingRestart, "persistence") {
		t.Errorf("persistence not reported pending-restart: %v", pinned.Reload.PendingRestart)
	}
	if contains(pinned.Reload.HotApplied, "persistence") {
		t.Errorf("restart-required persistence reported hot-applied: %v", pinned.Reload.HotApplied)
	}

	// The previously pinned snapshot must be untouched.
	if first.Version != 1 || first.Config.Sharing.Enabled {
		t.Fatalf("pinned snapshot mutated: %+v", first.Config.Sharing)
	}
}

func TestReloadReportsRestartRequiredAsNotHotApplied(t *testing.T) {
	store, path := openReloadStore(t, hotReloadDoc)

	// Change only a restart-required field.
	updated := strings.Replace(hotReloadDoc, `"/var/lib/vibeshell/world.db"`, `"/var/lib/vibeshell/other.db"`, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	report := store.Pin().Reload
	if report == nil {
		t.Fatal("reload report missing")
	}
	if len(report.HotApplied) != 0 {
		t.Errorf("restart-only change reported hot-applied: %v", report.HotApplied)
	}
	if !contains(report.PendingRestart, "persistence") {
		t.Errorf("persistence not reported pending-restart: %v", report.PendingRestart)
	}
}

func TestReloadInvalidCandidateKeepsSnapshot(t *testing.T) {
	store, path := openReloadStore(t, hotReloadDoc)
	before := store.Pin()

	// A value that parses as JSON but violates a semantic rule.
	if err := os.WriteFile(path, []byte(`{"version": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	if got := store.Pin(); got != before || got.Version != 1 {
		t.Fatalf("snapshot replaced after invalid candidate: %v", got)
	}
	if before.Config.Sharing.Enabled {
		t.Fatal("pinned config no longer matches the original document")
	}
}

func TestReloadConcurrentReadersSeeConsistentSnapshot(t *testing.T) {
	store, path := openReloadStore(t, hotReloadDoc)
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	alt := []byte(strings.Replace(string(base), `"enabled": false`, `"enabled": true`, 1))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := store.Pin()
				if snap == nil || snap.Config == nil || snap.Config.Sharing == nil {
					t.Error("reader saw an incomplete snapshot")
					return
				}
				// Version and sharing must come from one publication.
				if snap.Config.Sharing.Enabled && snap.Version == 1 {
					t.Error("reader saw a torn snapshot: v1 with sharing enabled")
					return
				}
			}
		}()
	}
	for i := 0; i < 25; i++ {
		doc := alt
		if i%2 == 0 {
			doc = base
		}
		if err := os.WriteFile(path, doc, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.Reload(); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

func sameSet(got []string, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for _, g := range got {
		if !want[g] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
