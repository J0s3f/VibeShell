package config

import (
	"encoding/json"
	"sort"
)

// FieldClass says whether a configuration field takes effect on live reload
// or only after a service restart. PLAN 11 requires the operator-facing
// boundary to be documented; ADR 0009 records that this was previously
// unimplemented.
type FieldClass string

const (
	// FieldHotReloadable fields are fully validated and published by
	// Store.Reload and take effect for new turns/sessions immediately.
	FieldHotReloadable FieldClass = "hot-reloadable"
	// FieldRestartRequired fields are validated on reload but their new
	// values do not take effect until the process restarts: they configure
	// the listening socket, the sandbox engine binary, or storage opened and
	// migrated at startup.
	FieldRestartRequired FieldClass = "restart-required"
	// FieldStatic fields are read once at startup and never change at
	// runtime; they are neither hot nor explicitly restart-bound.
	FieldStatic FieldClass = "static"
)

// restartRequiredFields names every leaf field that does not take effect on
// a live reload. The set is deliberately short and matches the PLAN 11
// boundary (bind address, engine binary, storage/migrations); a field this
// build does not list is not promised as restart-required.
var restartRequiredFields = map[string]bool{
	"ssh.listen_address":        true,
	"ssh.listen_port":           true,
	"ssh.host_key_file":         true,
	"apps.engine_abi_version":   true,
	"persistence.database_path": true,
	"persistence.durability":    true,
}

// staticGroups are presentation/authentication groups read once at startup.
// They are not part of the hot subset and are not reported as restart-bound.
var staticGroups = map[string]bool{
	"identity": true,
	"auth":     true,
}

// ClassifyField returns the class of one dot-delimited configuration field
// key such as "tiers" or "persistence.database_path". A restart-required leaf
// wins over its group; a static group is reported static; every other known
// top-level group is hot-reloadable. An unknown key is reported as
// FieldStatic, because a field this build does not classify cannot be
// promised as hot-reloadable. It performs no I/O and is safe for concurrent
// use.
func ClassifyField(key string) FieldClass {
	if restartRequiredFields[key] {
		return FieldRestartRequired
	}
	if dot := indexByte(key, '.'); dot >= 0 && restartRequiredFields[key[:dot]] {
		return FieldRestartRequired
	}
	group := key
	if dot := indexByte(key, '.'); dot >= 0 {
		group = key[:dot]
	}
	if staticGroups[group] {
		return FieldStatic
	}
	if _, ok := knownGroups[group]; ok {
		return FieldHotReloadable
	}
	return FieldStatic
}

// knownGroups lists the top-level JSON field names this build recognises, so
// ClassifyField can distinguish "a known hot group" from "an unknown key".
var knownGroups = map[string]bool{
	"identity":      true,
	"ssh":           true,
	"auth":          true,
	"sharing":       true,
	"world":         true,
	"apps":          true,
	"terminal":      true,
	"prompts":       true,
	"providers":     true,
	"routes":        true,
	"accounts":      true,
	"account_pools": true,
	"tiers":         true,
	"discovery":     true,
	"health":        true,
	"limits":        true,
	"inference":     true,
	"spending":      true,
	"persistence":   true,
	"exports":       true,
	"operations":    true,
}

// indexByte returns the first index of c in s, or -1.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Classification is the resolved classification of one configuration
// instance. Every slice is sorted so callers can compare and report them
// deterministically.
type Classification struct {
	// HotReloadable groups take effect for new turns/sessions after a
	// successful reload.
	HotReloadable []string
	// RestartRequired groups contain at least one restart-required field;
	// their new values do not take effect until the process restarts.
	RestartRequired []string
	// Static groups are read once at startup and never hot-applied.
	Static []string
}

// ClassifyConfig resolves the classification of the groups present in cfg.
// It is the programmatic form of the operator-facing boundary: a caller can
// answer "which of my configured groups take effect on reload?" without
// inspecting the document by hand. A group that mixes hot and
// restart-required fields (currently "ssh" and "persistence") is reported in
// RestartRequired, because the reload cannot promise all of its new values.
// A nil config yields an empty result.
func ClassifyConfig(cfg *Config) Classification {
	var result Classification
	if cfg == nil {
		return result
	}
	for _, key := range presentGroups(cfg) {
		switch groupClass(key) {
		case FieldHotReloadable:
			result.HotReloadable = append(result.HotReloadable, key)
		case FieldRestartRequired:
			result.RestartRequired = append(result.RestartRequired, key)
		default:
			result.Static = append(result.Static, key)
		}
	}
	sort.Strings(result.HotReloadable)
	sort.Strings(result.RestartRequired)
	sort.Strings(result.Static)
	return result
}

// groupClass resolves one top-level group to a single class: restart-required
// when any of its leaves is restart-required, static when it is a static
// group, otherwise hot-reloadable.
func groupClass(group string) FieldClass {
	if staticGroups[group] {
		return FieldStatic
	}
	for field := range restartRequiredFields {
		if dot := indexByte(field, '.'); dot >= 0 && field[:dot] == group {
			return FieldRestartRequired
		}
		if field == group {
			return FieldRestartRequired
		}
	}
	if knownGroups[group] {
		return FieldHotReloadable
	}
	return FieldStatic
}

// presentGroups lists the top-level JSON field names set in cfg, in the
// document's own names. A group is present when its pointer/slice field is
// non-nil, so an operator who omitted an optional group never sees it listed.
func presentGroups(cfg *Config) []string {
	var keys []string
	add := func(name string, present bool) {
		if present {
			keys = append(keys, name)
		}
	}
	add("identity", cfg.Identity != nil)
	add("ssh", cfg.SSH != nil)
	add("auth", cfg.Auth != nil)
	add("sharing", cfg.Sharing != nil)
	add("world", cfg.World != nil)
	add("apps", cfg.Apps != nil)
	add("terminal", cfg.Terminal != nil)
	add("prompts", cfg.Prompts != nil)
	add("providers", len(cfg.Providers) > 0)
	add("routes", len(cfg.Routes) > 0)
	add("accounts", len(cfg.Accounts) > 0)
	add("account_pools", len(cfg.AccountPools) > 0)
	add("tiers", len(cfg.Tiers) > 0)
	add("discovery", cfg.Discovery != nil)
	add("health", cfg.Health != nil)
	add("limits", cfg.Limits != nil)
	add("inference", cfg.Inference != nil)
	add("spending", cfg.Spending != nil)
	add("persistence", cfg.Persistence != nil)
	add("exports", cfg.Exports != nil)
	add("operations", cfg.Operations != nil)
	return keys
}

// ReloadReport records what one reload did to the published configuration,
// classified by the hot-reload boundary. It is attached to the newly
// published Snapshot so an operator can see exactly which fields took effect
// and which are waiting for a restart.
type ReloadReport struct {
	// Version is the published snapshot's version.
	Version uint64
	// HotApplied names groups whose new values were validated and are live
	// for new turns/sessions immediately.
	HotApplied []string
	// PendingRestart names restart-required groups whose values differ from
	// the previous snapshot. Their new values are validated but not applied
	// until the process restarts.
	PendingRestart []string
}

// newReloadReport classifies the difference between the previously published
// config and a candidate. A group is reported when its own JSON representation
// changed; hot groups go to HotApplied and restart-required groups go to
// PendingRestart. A first load (old == nil) reports every present group by its
// class, because the classification is the truthful boundary even when there
// is no previous value to compare. Unchanged groups are omitted.
func newReloadReport(old, candidate *Config, version uint64) *ReloadReport {
	report := &ReloadReport{Version: version}
	if candidate == nil {
		return report
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		oldJSON = nil
	}
	for _, key := range presentGroups(candidate) {
		changed := old == nil
		if !changed {
			changed = groupChanged(oldJSON, candidate, key)
		}
		if !changed {
			continue
		}
		switch groupClass(key) {
		case FieldHotReloadable:
			report.HotApplied = append(report.HotApplied, key)
		case FieldRestartRequired:
			report.PendingRestart = append(report.PendingRestart, key)
		}
	}
	sort.Strings(report.HotApplied)
	sort.Strings(report.PendingRestart)
	return report
}

// groupChanged reports whether the JSON form of one top-level group differs
// between oldJSON (the previous config, possibly nil or unparseable) and
// candidate. It re-marshals only the group, so an unrelated group's change
// never marks this one. An unavailable previous config counts as changed.
func groupChanged(oldJSON []byte, candidate *Config, key string) bool {
	if oldJSON == nil {
		return true
	}
	var oldDoc map[string]json.RawMessage
	if err := json.Unmarshal(oldJSON, &oldDoc); err != nil {
		return true
	}
	newJSON, err := json.Marshal(candidate)
	if err != nil {
		return true
	}
	var newDoc map[string]json.RawMessage
	if err := json.Unmarshal(newJSON, &newDoc); err != nil {
		return true
	}
	oldGroup, ok := oldDoc[key]
	if !ok {
		return true
	}
	newGroup, ok := newDoc[key]
	if !ok {
		return true
	}
	return string(oldGroup) != string(newGroup)
}
