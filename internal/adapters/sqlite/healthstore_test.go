package sqlite

import (
	"context"
	"reflect"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/routing"
)

func mustRouteID(t *testing.T) domain.RouteID {
	t.Helper()
	raw, err := newTestPrefixID(domain.PrefixRoute)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseRouteID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustAccountID(t *testing.T) domain.AccountID {
	t.Helper()
	raw, err := newTestPrefixID(domain.PrefixAccount)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseAccountID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// coolingRecord is a record that failed with an exhausted quota: it must still
// be cooling down after a restart, with its reset time intact.
func coolingRecord(route domain.RouteID, account domain.AccountID, now int64) domain.HealthRecord {
	rec := domain.NewHealthRecord(route, account, routing.ScopeAccount, now)
	rec.RecordFailure(domain.FailureQuotaExhausted, "quota exhausted", nil, now)
	return rec
}

func TestRouteHealthCooldownSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	db, path := durableDB(t)
	const now = 1_700_000_500_000
	health1, err := NewRouteHealth(db.SQL(), fixedTestClock{now: now})
	if err != nil {
		t.Fatalf("NewRouteHealth: %v", err)
	}

	route, account := mustRouteID(t), mustAccountID(t)
	cooling := coolingRecord(route, account, now)
	if cooling.State != domain.HealthCoolingDown || cooling.CooldownUntil == 0 {
		t.Fatalf("fixture is not cooling down: %+v", cooling)
	}
	if err := health1.Save(ctx, cooling); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A probe-eligible route record with a due probe time: the probe state must
	// also outlive the process, or a restarted router would never re-probe.
	routeRecord := domain.NewHealthRecord(route, domain.AccountID{}, routing.ScopeRoute, now)
	routeRecord.RecordFailure(domain.FailureProviderOutage, "provider down", nil, now)
	routeRecord.State = domain.HealthProbeEligible
	routeRecord.NextProbeAt = now + 1_000
	routeRecord.ProbeCount = 2
	if err := health1.Save(ctx, routeRecord); err != nil {
		t.Fatalf("Save route record: %v", err)
	}

	db = restart(t, db, path)
	health2, err := NewRouteHealth(db.SQL(), fixedTestClock{now: now + 60_000})
	if err != nil {
		t.Fatalf("NewRouteHealth after restart: %v", err)
	}

	key := domain.HealthKey{RouteID: route, AccountID: account, Scope: routing.ScopeAccount}
	got, ok, err := health2.Get(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Get after restart: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, cooling) {
		t.Errorf("account record after restart:\n got %+v\nwant %+v", got, cooling)
	}
	// The cooldown must still be in force, not silently treated as healthy.
	if got.IsHealthy(now + 1) {
		t.Errorf("account record is healthy during its cooldown: %+v", got)
	}
	if !got.IsHealthy(got.CooldownUntil) {
		t.Errorf("account record is still unhealthy after its cooldown: %+v", got)
	}

	gotRoute, ok, err := health2.Get(ctx, domain.HealthKey{RouteID: route, Scope: routing.ScopeRoute})
	if err != nil || !ok {
		t.Fatalf("Get route record after restart: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(gotRoute, routeRecord) {
		t.Errorf("route record after restart:\n got %+v\nwant %+v", gotRoute, routeRecord)
	}

	all, err := health2.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List returned %d records, want 2", len(all))
	}
	// Deterministic key order keeps due-probe scanning and status output stable.
	// Ordering is by key, so the route-scope record (no account) comes first.
	if all[0].Scope != routing.ScopeRoute || all[1].Scope != routing.ScopeAccount {
		t.Errorf("List order = %q, %q; want the account-less route scope first", all[0].Scope, all[1].Scope)
	}
}

func TestRouteHealthMissingRecordMeansHealthy(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	health, err := NewRouteHealth(db.SQL(), fixedTestClock{now: 1})
	if err != nil {
		t.Fatalf("NewRouteHealth: %v", err)
	}
	_, ok, err := health.Get(ctx, domain.HealthKey{RouteID: mustRouteID(t), Scope: routing.ScopeRoute})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("a scope with no stored problem must report ok=false")
	}
	all, err := health.List(ctx)
	if err != nil || len(all) != 0 {
		t.Errorf("List on empty store = %d records, err %v; want none", len(all), err)
	}
}

func TestRouteHealthSaveUpdatesInPlace(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	const now = 1_700_000_600_000
	health, err := NewRouteHealth(db.SQL(), fixedTestClock{now: now})
	if err != nil {
		t.Fatalf("NewRouteHealth: %v", err)
	}
	route, account := mustRouteID(t), mustAccountID(t)
	key := domain.HealthKey{RouteID: route, AccountID: account, Scope: routing.ScopeCredential}

	cooling := coolingRecord(route, account, now)
	cooling.Scope = routing.ScopeCredential
	if err := health.Save(ctx, cooling); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A later success clears the failure history in place instead of adding a
	// second row for the same scope.
	recovered := cooling
	recovered.RecordSuccess(cooling.CooldownUntil)
	if err := health.Save(ctx, recovered); err != nil {
		t.Fatalf("Save recovered: %v", err)
	}
	got, ok, err := health.Get(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, recovered) {
		t.Errorf("recovered record:\n got %+v\nwant %+v", got, recovered)
	}
	all, err := health.List(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("List returned %d records, want 1: %v", len(all), err)
	}
}

func TestRouteHealthRejectsUnusableRecords(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	health, err := NewRouteHealth(db.SQL(), fixedTestClock{now: 1})
	if err != nil {
		t.Fatalf("NewRouteHealth: %v", err)
	}
	route, account := mustRouteID(t), mustAccountID(t)

	unknownState := domain.NewHealthRecord(route, account, routing.ScopeAccount, 1)
	unknownState.State = "vibes"
	if err := health.Save(ctx, unknownState); domain.GetErrorCode(err) != domain.CodeInvalidInput {
		t.Errorf("Save with an unknown state = %v, want %s", err, domain.CodeInvalidInput)
	}
	missingRoute := domain.NewHealthRecord(domain.RouteID{}, account, routing.ScopeAccount, 1)
	if err := health.Save(ctx, missingRoute); domain.GetErrorCode(err) != domain.CodeInvalidInput {
		t.Errorf("Save without a route = %v, want %s", err, domain.CodeInvalidInput)
	}
	missingScope := domain.NewHealthRecord(route, account, "", 1)
	if err := health.Save(ctx, missingScope); domain.GetErrorCode(err) != domain.CodeInvalidInput {
		t.Errorf("Save without a scope = %v, want %s", err, domain.CodeInvalidInput)
	}
}

func TestQuotaGroupBlockSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	db, path := durableDB(t)
	const now = 1_700_000_700_000
	const until = now + domain.DefaultQuotaCooldownMs
	health1, err := NewRouteHealth(db.SQL(), fixedTestClock{now: now})
	if err != nil {
		t.Fatalf("NewRouteHealth: %v", err)
	}
	if err := health1.BlockQuotaGroup(ctx, "team-a", until); err != nil {
		t.Fatalf("BlockQuotaGroup: %v", err)
	}

	db = restart(t, db, path)
	health2, err := NewRouteHealth(db.SQL(), fixedTestClock{now: now})
	if err != nil {
		t.Fatalf("NewRouteHealth after restart: %v", err)
	}
	blocked, err := health2.QuotaGroupBlocked(ctx, "team-a", now+1)
	if err != nil {
		t.Fatalf("QuotaGroupBlocked: %v", err)
	}
	if !blocked {
		t.Error("quota group must stay blocked across a restart")
	}
	if blocked, err := health2.QuotaGroupBlocked(ctx, "team-a", until); err != nil || blocked {
		t.Errorf("quota group after its reset = blocked:%v err:%v, want unblocked", blocked, err)
	}
	if blocked, err := health2.QuotaGroupBlocked(ctx, "payg", now); err != nil || blocked {
		t.Errorf("unknown quota group = blocked:%v err:%v, want unblocked", blocked, err)
	}
	blocks, err := health2.QuotaGroupBlocks(ctx)
	if err != nil {
		t.Fatalf("QuotaGroupBlocks: %v", err)
	}
	if len(blocks) != 1 || blocks["team-a"] != until {
		t.Errorf("QuotaGroupBlocks = %v, want team-a blocked until %d", blocks, until)
	}
	if err := health2.BlockQuotaGroup(ctx, "", until); domain.GetErrorCode(err) != domain.CodeInvalidInput {
		t.Errorf("BlockQuotaGroup without a group = %v, want %s", err, domain.CodeInvalidInput)
	}
}
