package worldpaths

import (
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func TestUnixPermissionTable(t *testing.T) {
	meta := domain.NewNodeMetadata(0o640, 1000, 2000, 1798732800000)
	cases := []struct {
		name  string
		id    Identity
		want  Access
		allow bool
	}{
		{"owner read", Identity{EUID: 1000, EGID: 999}, AccessRead, true},
		{"owner write", Identity{EUID: 1000, EGID: 999}, AccessWrite, true},
		{"owner exec denied", Identity{EUID: 1000, EGID: 999}, AccessExec, false},
		{"group read", Identity{EUID: 3000, EGID: 2000}, AccessRead, true},
		{"group write denied", Identity{EUID: 3000, EGID: 2000}, AccessWrite, false},
		{"group via supplementary", Identity{EUID: 3000, EGID: 3000, Groups: []uint32{2000}}, AccessRead, true},
		{"other read denied", Identity{EUID: 4000, EGID: 4000}, AccessRead, false},
		{"other write denied", Identity{EUID: 4000, EGID: 4000}, AccessWrite, false},
		{"root read bypass", Identity{EUID: 0, IsRootUser: true}, AccessRead, true},
		{"root write bypass", Identity{EUID: 0, IsRootUser: true}, AccessWrite, true},
		{"root exec needs any x", Identity{EUID: 0, IsRootUser: true}, AccessExec, false},
	}
	for _, tc := range cases {
		if got := CheckUnix(meta, tc.id, tc.want); got != tc.allow {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.allow)
		}
	}
	execMeta := domain.NewNodeMetadata(0o755, 1000, 2000, 1798732800000)
	if !CheckUnix(execMeta, Identity{EUID: 0, IsRootUser: true}, AccessExec) {
		t.Error("root exec on 0755 must be allowed")
	}
	other := domain.NewNodeMetadata(0o007, 1000, 2000, 1798732800000)
	if !CheckUnix(other, Identity{EUID: 4000, EGID: 4000}, AccessRead) {
		t.Error("other read on 0007 must be allowed")
	}
}

func TestSharingOffRejectsRegardlessOfBits(t *testing.T) {
	closed := domain.RestrictedScopePolicy()
	open := domain.DefaultScopePolicy()
	// World-readable bits would allow access, but sharing-off denies first.
	meta := domain.NewNodeMetadata(0o777, 1000, 1000, 1798732800000)
	other := Identity{EUID: 4000, EGID: 4000}
	if err := Enforce(open, domain.ScopeShared, false, meta, other, AccessRead, false); err != nil {
		t.Errorf("sharing-on shared read with 0777: %v", err)
	}
	if err := Enforce(closed, domain.ScopeShared, false, meta, other, AccessRead, false); err == nil {
		t.Error("sharing-off shared read succeeded despite 0777, want denial")
	}
	if err := Enforce(closed, domain.ScopeUser, false, meta, other, AccessRead, false); err == nil {
		t.Error("sharing-off cross-user read succeeded, want denial")
	}
	if err := Enforce(closed, domain.ScopeShared, false, meta, other, AccessWrite, true); err == nil {
		t.Error("sharing-off shared write succeeded, want denial")
	}
	// Simulated root still cannot cross the sharing boundary.
	root := Identity{EUID: 0, IsRootUser: true, Simulated: true}
	if err := Enforce(closed, domain.ScopeShared, false, meta, root, AccessRead, false); err == nil {
		t.Error("simulated root crossed sharing-off boundary, want denial")
	}
	// Own user scope and baseline stay usable when sharing is off.
	own := Identity{EUID: 1000, EGID: 1000}
	if err := Enforce(closed, domain.ScopeUser, true, meta, own, AccessRead, false); err != nil {
		t.Errorf("own user read while sharing off: %v", err)
	}
	if err := Enforce(closed, domain.ScopeBaseline, false, meta, own, AccessRead, false); err != nil {
		t.Errorf("baseline read while sharing off: %v", err)
	}
	// Baseline is immutable even when sharing is on.
	if err := Enforce(open, domain.ScopeBaseline, true, meta, root, AccessWrite, true); err == nil {
		t.Error("baseline write succeeded, want denial")
	}
}
