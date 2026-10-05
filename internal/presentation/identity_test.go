package presentation

import (
	"strings"
	"testing"
)

func TestDefaultIdentityReportsSimulatedValues(t *testing.T) {
	id := DefaultSystemIdentity()
	if err := id.Validate(); err != nil {
		t.Fatalf("default identity invalid: %v", err)
	}
	uname := id.Uname()
	for _, want := range []string{DefaultSystemName, DefaultHostname, DefaultRelease, DefaultMachine} {
		if !strings.Contains(uname, want) {
			t.Fatalf("uname %q missing simulated value %q", uname, want)
		}
	}
	osrel := id.OSRelease()
	if !strings.Contains(osrel, "VibeOS") || !strings.Contains(osrel, "simulated") {
		t.Fatalf("os-release %q must brand VibeOS as simulated", osrel)
	}
	if strings.Contains(osrel, "ID_LIKE") {
		t.Fatalf("os-release %q must not claim distribution lineage", osrel)
	}
	if id.ShellIdentification() != DefaultShellName {
		t.Fatalf("shell = %q, want %q", id.ShellIdentification(), DefaultShellName)
	}
	if !strings.Contains(id.ShellVersionString(), DefaultShellName) {
		t.Fatalf("shell version %q missing shell name", id.ShellVersionString())
	}
}

func TestIdentityOverrideRendersOverrideOnly(t *testing.T) {
	id := DefaultSystemIdentity()
	id.Hostname = "hurdbox"
	id.Release = "2.0.0"
	if err := id.Validate(); err != nil {
		t.Fatalf("override invalid: %v", err)
	}
	if got := id.Nodename(); got != "hurdbox" {
		t.Fatalf("nodename = %q, want hurdbox", got)
	}
	uname := id.Uname()
	if !strings.Contains(uname, "hurdbox") || !strings.Contains(uname, "2.0.0") {
		t.Fatalf("uname %q missing override values", uname)
	}
	// The override renders exactly the configured fields; there is
	// no path in this package that substitutes real host data.
	if strings.Contains(uname, DefaultHostname) {
		t.Fatalf("uname %q still carries the old hostname after override", uname)
	}
}

func TestIdentityValidationRejectsBadOverrides(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SystemIdentity)
	}{
		{"empty system name", func(i *SystemIdentity) { i.SystemName = "" }},
		{"control characters", func(i *SystemIdentity) { i.SystemName = "Vibe\x00OS" }},
		{"empty hostname", func(i *SystemIdentity) { i.Hostname = "" }},
		{"hostname with spaces", func(i *SystemIdentity) { i.Hostname = "not a host!" }},
		{"hostname with slash", func(i *SystemIdentity) { i.Hostname = "a/b" }},
		{"oversize release", func(i *SystemIdentity) { i.Release = strings.Repeat("x", MaxIdentityFieldLen+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := DefaultSystemIdentity()
			tc.mutate(&id)
			if err := id.Validate(); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}
