package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/sandbox"
	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/observability"
	"j0s.at/vibeshell/internal/routing"
	"j0s.at/vibeshell/internal/system"
)

// The tests in this file prove that the durable adapters the composition root
// wires are the ones the process actually runs with. They drive them through
// the real application lifecycle and the real router, close the database, and
// reopen it, so a restart exercises the same open-migrate-construct sequence as
// startup instead of a second connection to a live handle.

// testQuotaGroup is the shared quota group both durable accounts belong to.
const testQuotaGroup = "team-a"

// openDurableTestDB opens a database file exactly as startup does and builds the
// durable adapters over it. The returned close function checkpoints and closes
// the handle; calling it early is what makes the restart in a test a real close
// and reopen.
func openDurableTestDB(t *testing.T, path string) (*durableState, *system.Clock, func()) {
	t.Helper()
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		t.Fatalf("sqlite.Open(%q): %v", path, err)
	}
	clock := system.NewClock()
	state, err := openDurableState(db, clock)
	if err != nil {
		_ = db.Close()
		t.Fatalf("openDurableState: %v", err)
	}
	closeDB := func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return state, clock, closeDB
}

// testRoutingConfig is the router configuration the composition builds for a
// single route and a single account in a shared quota group. A positive
// estimate marks the route paid and returns the estimate the ledger reserves.
func testRoutingConfig(estimate float64) routing.Config {
	route, account := testRouteID(), testAccountID()
	cfg := routing.Config{
		Policy: domain.RoutePolicy{
			Tiers: []domain.TierConfig{{
				Name:     "named-free",
				RouteIDs: []domain.RouteID{route},
				Enabled:  true,
			}},
			MaxAttempts:    3,
			TurnDeadlineMs: 60_000,
		},
		Routes: []routing.RouteSpec{{ID: route, Product: "console", Paid: estimate > 0}},
		Accounts: []domain.Account{{
			ID:                account,
			QuotaGroup:        testQuotaGroup,
			PermittedProducts: []string{"console"},
			Enabled:           true,
		}},
	}
	if estimate > 0 {
		cfg.EstimateMaxCost = func(domain.RouteID) (float64, bool) { return estimate, true }
	}
	return cfg
}

// testOwnerID is the user who owns the generated application.
func testOwnerID(t *testing.T) domain.UserID {
	t.Helper()
	owner, err := domain.ParseUserID(domain.PrefixUser + "_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	return owner
}

func testSessionID(t *testing.T, value string) domain.SessionID {
	t.Helper()
	session, err := domain.ParseSessionID(domain.PrefixSession + "_" + value)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	return session
}

// acceptTestApp registers and activates a generated application through the real
// application service over the durable registry, using the real sandbox for
// staging validation. It returns the activated version.
func acceptTestApp(t *testing.T, state *durableState, owner domain.UserID) domain.AppVersionID {
	t.Helper()
	ctx := context.Background()
	adapter, err := sandbox.NewAdapter(ctx, sandbox.DefaultMemoryPages, sandbox.Config{})
	if err != nil {
		t.Fatalf("sandbox.NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close(ctx) })

	clock := system.NewClock()
	service := apps.NewService(state.apps, adapter, clock, system.NewRandom(), nil)
	version, err := service.RegisterCandidate(ctx, apps.CandidateRequest{
		Manifest: domain.AppManifest{
			ABIVersion:   domain.AppABIVersion,
			CommandNames: []string{"moon-orchard"},
			Description:  "generated moon-orchard",
			StateSchema:  json.RawMessage(`{}`),
			Capabilities: []string{domain.CapabilityTime},
			Entrypoint:   "handle",
			Version:      "0.1.0",
		},
		Source: constSource,
		Owner:  owner,
		Scope:  domain.ScopeUser,
		Provenance: domain.Provenance{
			Source:        "model",
			Actor:         "generation",
			PromptVersion: "v1",
			ConfigVersion: "1",
		},
		Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	artifact, err := state.apps.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact(%s): %v", version, err)
	}
	if _, err := service.Activate(ctx, apps.ActivateRequest{
		AppID:   artifact.AppID,
		Version: version,
		Actor:   owner,
		Policy:  domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return version
}

// TestDurableAppRegistryAndHealthSurviveRestart proves that an accepted
// application and the route/account health a failed turn produced are still
// there after the database is closed and reopened, and that a router built over
// the reopened store honors them: the exhausted quota group is not selected
// again.
func TestDurableAppRegistryAndHealthSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "world.db")
	route, account := testRouteID(), testAccountID()
	session := testSessionID(t, "0123456789ABCDEFGHJKMNPQRS")

	first, clock, closeFirst := openDurableTestDB(t, dbPath)
	owner := testOwnerID(t)
	version := acceptTestApp(t, first, owner)
	artifact, err := first.apps.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact before restart: %v", err)
	}
	appID := artifact.AppID

	// One turn against a provider that reports an exhausted quota: the router
	// cools the account record and blocks the shared quota group.
	quotaFailure := domain.NewErrorEnvelope(
		domain.FailureQuotaExhausted, "quota exhausted", route, account, clock.NowUnixMilli())
	gateway := &fakeGateway{err: quotaFailure}
	router := buildRouter(gateway, first, clock, system.NewRandom(), testRoutingConfig(0))
	if _, err := router.ExecuteTurn(ctx, routing.TurnRequest{
		Session:  session,
		Turn:     mustTurnID(t),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
	}); err == nil {
		t.Fatal("a turn whose only provider failed reported success")
	}
	closeFirst()

	second, _, _ := openDurableTestDB(t, dbPath)

	// The application registry kept the accepted artifact, its current
	// pointer, and the activation log.
	current, err := second.apps.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current after restart: %v", err)
	}
	if current != version {
		t.Fatalf("current version after restart = %s, want %s", current, version)
	}
	reloaded, err := second.apps.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact after restart: %v", err)
	}
	if reloaded.Source != constSource {
		t.Fatal("the stored artifact's source did not survive the restart")
	}
	activations, err := second.apps.Activations(ctx, appID)
	if err != nil {
		t.Fatalf("Activations after restart: %v", err)
	}
	if len(activations) != 1 || activations[0].To != version {
		t.Fatalf("activation log after restart = %+v, want one entry for %s", activations, version)
	}

	// Route and account health survived, with the failure that caused them.
	rec, ok, err := second.health.Get(ctx, domain.HealthKey{
		RouteID: route, AccountID: account, Scope: routing.ScopeAccount,
	})
	if err != nil {
		t.Fatalf("read account health after restart: %v", err)
	}
	if !ok {
		t.Fatal("the account health record did not survive the restart")
	}
	if rec.State != domain.HealthCoolingDown || rec.FailureClass != domain.FailureQuotaExhausted {
		t.Fatalf("account health after restart = state %v class %v, want cooling_down quota_exhausted",
			rec.State, rec.FailureClass)
	}
	blocked, err := second.health.QuotaGroupBlocked(ctx, testQuotaGroup, clock.NowUnixMilli())
	if err != nil {
		t.Fatalf("QuotaGroupBlocked after restart: %v", err)
	}
	if !blocked {
		t.Fatal("the quota-group block did not survive the restart")
	}

	// A router built over the reopened store therefore selects nothing, rather
	// than retrying an account the provider already refused.
	restarted := buildRouter(gateway, second, clock, system.NewRandom(), testRoutingConfig(0))
	if _, err := restarted.EnsureBinding(ctx, session); !errors.Is(err, routing.ErrNoEligibleRoute) {
		t.Fatalf("EnsureBinding after restart = %v, want ErrNoEligibleRoute", err)
	}
}

// durableTestConfig is a public-mode configuration whose listener, host key,
// and database all live under dir, so a test can build the real component
// twice over one database file.
func durableTestConfig(t *testing.T, dir string, port int) string {
	t.Helper()
	return fmt.Sprintf(`{
  "version": 1,
  "identity": {"system_name": "VibeOS", "shell_name": "VibeShell", "hostname": "vibeshell.test"},
  "ssh": {"listen_address": "127.0.0.1", "listen_port": %d, "host_key_file": %q},
  "auth": {"mode": "public"},
  "sharing": {"enabled": false},
  "providers": [{"name": "opencode", "products": [{"name": "console", "base_url": "https://opencode.example.invalid", "protocols": ["chat"], "default_protocol": "chat"}]}],
  "routes": [{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "durable-test", "protocol": "chat"}],
  "tiers": [{"name": "durable-test", "routes": ["rte_0123456789ABCDEFGHJKMNPQRS"]}],
  "accounts": [{"id": "acc_0123456789ABCDEFGHJKMNPQRS", "quota_group": %q, "permitted_products": ["console"], "secret_ref": "{env:VIBESHELL_TEST_KEY}"}],
  "persistence": {"database_path": %q}
}`, port, filepath.Join(dir, "host_key"), testQuotaGroup, filepath.Join(dir, "world.db"))
}

// buildDurableComponent builds the real component over the configuration in dir
// and shuts it down on cleanup. It is the same construction path startup uses,
// so a test proves the wiring rather than a helper that resembles it.
func buildDurableComponent(t *testing.T, dir string) *component {
	t.Helper()
	t.Setenv("VIBESHELL_TEST_KEY", "test-secret-value")
	configPath := filepath.Join(dir, "vibeshell.json")
	if err := os.WriteFile(configPath, []byte(durableTestConfig(t, dir, freeLoopbackPort(t))), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	snapshot, err := config.NewLoader(dir, config.Options{}).Load(mustReadFile(t, configPath))
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	logger := observability.NewLogger(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := build(ctx, snapshot, dir, observability.NewReadinessReporter(), logger)
	if err != nil {
		t.Fatalf("build component: %v", err)
	}
	t.Cleanup(func() { _ = c.shutdown() })
	return c
}

// TestComponentWiresDurableStateAcrossRestart is the composition-level proof: the
// component the process builds reads and writes its generated-application
// registry, route/account health, quota groups, and spending accounting through
// the SQLite-backed stores, so all of it is still there for the next process.
func TestComponentWiresDurableStateAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	route, account := testRouteID(), testAccountID()
	owner := testOwnerID(t)

	first := buildDurableComponent(t, dir)
	if first.durable == nil || first.appService == nil {
		t.Fatal("build did not construct the durable adapters and the application service")
	}
	if first.health != first.durable.health {
		t.Fatal("the router's health store is not the durable one")
	}
	engine, ok := buildTurnEngine(context.Background(), first).(*generationEngine)
	if !ok {
		t.Fatal("a configured provider account did not select the generation engine")
	}
	if engine.registry != first.durable.apps {
		t.Fatal("the generation engine does not use the durable app registry")
	}

	version := acceptTestApp(t, first.durable, owner)
	artifact, err := first.durable.apps.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact before restart: %v", err)
	}
	// A quota failure cools the account and blocks the shared quota group, the
	// two facts routing must not forget across a restart.
	rec := domain.NewHealthRecord(route, account, routing.ScopeAccount, first.clock.NowUnixMilli())
	rec.RecordFailure(domain.FailureQuotaExhausted, "quota exhausted", nil, first.clock.NowUnixMilli())
	if err := first.health.Save(ctx, rec); err != nil {
		t.Fatalf("save account health: %v", err)
	}
	if err := first.health.BlockQuotaGroup(ctx, testQuotaGroup, first.clock.NowUnixMilli()+3_600_000); err != nil {
		t.Fatalf("BlockQuotaGroup: %v", err)
	}
	if err := first.shutdown(); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}

	second := buildDurableComponent(t, dir)
	current, err := second.durable.apps.Current(ctx, artifact.AppID)
	if err != nil {
		t.Fatalf("Current after restart: %v", err)
	}
	if current != version {
		t.Fatalf("current version after restart = %s, want %s", current, version)
	}
	if _, err := second.durable.apps.GetArtifact(ctx, version); err != nil {
		t.Fatalf("GetArtifact after restart: %v", err)
	}
	restored, ok, err := second.health.Get(ctx, domain.HealthKey{
		RouteID: route, AccountID: account, Scope: routing.ScopeAccount,
	})
	if err != nil || !ok {
		t.Fatalf("read account health after restart: ok=%v err=%v", ok, err)
	}
	if restored.State != domain.HealthCoolingDown || restored.FailureClass != domain.FailureQuotaExhausted {
		t.Fatalf("account health after restart = state %v class %v, want cooling_down quota_exhausted",
			restored.State, restored.FailureClass)
	}
	blocked, err := second.health.QuotaGroupBlocked(ctx, testQuotaGroup, second.clock.NowUnixMilli())
	if err != nil {
		t.Fatalf("QuotaGroupBlocked after restart: %v", err)
	}
	if !blocked {
		t.Fatal("the quota-group block did not survive the restart")
	}
}

// TestDurableSpendingSurvivesRestart proves the spending ledger the composition
// wires is the durable one: a reservation drawn for a paid attempt and settled
// with the reported usage is still accounted after the restart, so a restart
// does not hand the same remaining budget out again.
func TestDurableSpendingSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "world.db")
	route, account := testRouteID(), testAccountID()
	const estimate, actual = 0.5, 0.05

	reported := actual
	gateway := &fakeGateway{content: "ok", usage: domain.Usage{EstimatedCostUSD: &reported}}

	first, clock, closeFirst := openDurableTestDB(t, dbPath)
	router := buildRouter(gateway, first, clock, system.NewRandom(), testRoutingConfig(estimate))
	result, err := router.ExecuteTurn(ctx, routing.TurnRequest{
		Session:  testSessionID(t, "0123456789ABCDEFGHJKMNPQR9"),
		Turn:     mustTurnID(t),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("ExecuteTurn: %v", err)
	}
	if result.Binding.Account != account {
		t.Fatalf("turn ran on account %s, want %s", result.Binding.Account, account)
	}
	closeFirst()

	second, _, _ := openDurableTestDB(t, dbPath)
	usage, ok, err := second.spending.Usage(ctx, account, route)
	if err != nil {
		t.Fatalf("Usage after restart: %v", err)
	}
	if !ok {
		t.Fatal("the settled spending record did not survive the restart")
	}
	if math.Abs(usage.EstimatedUSD-estimate) > 1e-9 {
		t.Fatalf("estimated cost after restart = %v, want %v", usage.EstimatedUSD, estimate)
	}
	if math.Abs(usage.ReportedUSD-actual) > 1e-9 {
		t.Fatalf("reported cost after restart = %v, want %v", usage.ReportedUSD, actual)
	}
	if usage.ReservedUSD != 0 {
		t.Fatalf("held reservation after restart = %v, want 0 once settled", usage.ReservedUSD)
	}
}
