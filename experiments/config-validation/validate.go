package config

import (
	"errors"
	"fmt"
)

// Validate applies the semantic rules that strict decoding
// cannot express: fixed identity constants, enum and bounds
// checks, duplicate identifiers, tier ordering, model-route
// references against a catalogue, protocol support, policy
// combinations, and secret reference grammar. It is pure: no
// I/O, no environment reads, no network, and no secret
// resolution. The caller performs those at the loader
// boundary.
func Validate(cfg Config, cat Catalogue) error {
	var errs []error

	// identity: the schema fixes both names as constants.
	if cfg.Identity.SystemName != SystemNameConst {
		errs = append(errs, fmt.Errorf("identity.system_name must be %q, got %q", SystemNameConst, cfg.Identity.SystemName))
	}
	if cfg.Identity.ShellName != ShellNameConst {
		errs = append(errs, fmt.Errorf("identity.shell_name must be %q, got %q", ShellNameConst, cfg.Identity.ShellName))
	}
	if n := len(cfg.Identity.Hostname); n > MaxHostnameLen {
		errs = append(errs, fmt.Errorf("identity.hostname must be at most %d characters, got %d", MaxHostnameLen, n))
	}

	// ssh
	if cfg.SSH.ListenPort < MinListenPort || cfg.SSH.ListenPort > MaxListenPort {
		errs = append(errs, fmt.Errorf("ssh.listen_port must be in [%d, %d], got %d", MinListenPort, MaxListenPort, cfg.SSH.ListenPort))
	}
	if cfg.SSH.MaxConnections > MaxSSHConnections {
		errs = append(errs, fmt.Errorf("ssh.max_connections must be at most %d, got %d", MaxSSHConnections, cfg.SSH.MaxConnections))
	}

	// auth: an explicit mode, with pairing rules between the
	// mode and the password file.
	switch cfg.Auth.Mode {
	case AuthModePublic, AuthModeSecure:
	default:
		errs = append(errs, fmt.Errorf("auth.mode must be %q or %q, got %q", AuthModePublic, AuthModeSecure, cfg.Auth.Mode))
	}
	switch {
	case cfg.Auth.Mode == AuthModePublic && cfg.Auth.PasswordFile != "":
		errs = append(errs, fmt.Errorf("auth.mode %q must not set auth.password_file; use %q mode for password authentication", AuthModePublic, AuthModeSecure))
	case cfg.Auth.Mode == AuthModeSecure && cfg.Auth.PasswordFile == "":
		errs = append(errs, fmt.Errorf("auth.mode %q requires auth.password_file", AuthModeSecure))
	}
	if cfg.Auth.MaxAttemptsPerConn > MaxAuthAttemptsPerConn {
		errs = append(errs, fmt.Errorf("auth.max_attempts_per_connection must be at most %d, got %d", MaxAuthAttemptsPerConn, cfg.Auth.MaxAttemptsPerConn))
	}

	// sharing: enabling cross-user access requires a policy
	// revision so revocation can be ordered.
	if cfg.Sharing.Enabled == nil {
		errs = append(errs, fmt.Errorf("required field $.sharing.enabled is missing"))
	} else if *cfg.Sharing.Enabled && cfg.Sharing.PolicyRevision < 1 {
		errs = append(errs, fmt.Errorf("sharing.enabled requires sharing.policy_revision >= 1"))
	}

	errs = append(errs, validateTiers(cfg.Tiers, cat)...)
	errs = append(errs, validateAccounts(cfg.Accounts)...)
	errs = append(errs, validateLimits(cfg.Limits)...)
	errs = append(errs, validatePersistence(cfg.Persistence)...)

	if cfg.Providers != nil {
		errs = append(errs, validateProviders(*cfg.Providers)...)
	}
	if cfg.Discovery != nil {
		errs = append(errs, validateDiscovery(*cfg.Discovery)...)
	}
	if cfg.Inference != nil {
		errs = append(errs, validateInference(*cfg.Inference)...)
	}
	if cfg.Operations != nil {
		errs = append(errs, validateOperations(*cfg.Operations)...)
	}

	// Route-to-protocol support is checked last so that unknown
	// routes are reported separately from unsupported protocol
	// combinations.
	errs = append(errs, validateRouteProtocols(cfg.Tiers, cat, cfg.Providers)...)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func validateTiers(tiers []TierConfig, cat Catalogue) []error {
	var errs []error
	if len(tiers) > MaxTiers {
		errs = append(errs, fmt.Errorf("tiers must list at most %d entries, got %d", MaxTiers, len(tiers)))
	}
	names := map[string]bool{}
	discoveryTiers := 0
	for i, tier := range tiers {
		where := fmt.Sprintf("tiers[%d]", i)
		if tier.Name != "" {
			where = fmt.Sprintf("%s (%q)", where, tier.Name)
		}
		if len(tier.Name) > MaxTierNameLen {
			errs = append(errs, fmt.Errorf("%s: name must be at most %d characters, got %d", where, MaxTierNameLen, len(tier.Name)))
		}
		if names[tier.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate tier name %q", where, tier.Name))
		}
		names[tier.Name] = true

		if len(tier.Routes) > MaxRoutesPerTier {
			errs = append(errs, fmt.Errorf("%s: at most %d routes, got %d", where, MaxRoutesPerTier, len(tier.Routes)))
		}

		if tier.AutoFree {
			discoveryTiers++
			if i == 0 {
				errs = append(errs, fmt.Errorf("%s: the suitable-free discovery tier (auto_free) must not be the first tier; list explicit named free routes first so they keep precedence", where))
			}
			if tier.AccountPool != "" {
				errs = append(errs, fmt.Errorf("%s: the discovery tier (auto_free) must not set account_pool; discovery expands across all eligible accounts", where))
			}
		}

		for _, route := range tier.Routes {
			if IsDiscoveryPattern(route) {
				if !tier.AutoFree {
					errs = append(errs, fmt.Errorf("%s: route %q is a discovery pattern; only an auto_free tier may expand patterns", where, route))
					continue
				}
				if matches := cat.Discover(discoveryProduct(route)); len(matches) == 0 {
					errs = append(errs, fmt.Errorf("%s: discovery pattern %q matches no routes in the catalogue", where, route))
				}
				continue
			}
			if tier.AutoFree {
				errs = append(errs, fmt.Errorf("%s: explicit route %q in an auto_free discovery tier; discovery tiers list patterns only", where, route))
			}
			if _, known := cat.Route(route); !known {
				errs = append(errs, fmt.Errorf("%s: unknown model route %q; it is not in the catalogue", where, route))
			}
		}
	}
	if discoveryTiers > 1 {
		errs = append(errs, fmt.Errorf("at most one tier may use auto_free discovery, found %d", discoveryTiers))
	}
	return errs
}

func validateAccounts(accounts []AccountConfig) []error {
	var errs []error
	ids := map[string]bool{}
	for i, account := range accounts {
		where := fmt.Sprintf("accounts[%d]", i)
		if account.ID != "" {
			where = fmt.Sprintf("%s (%q)", where, account.ID)
		}
		if ids[account.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate account ID %q", where, account.ID))
		}
		ids[account.ID] = true
		if _, err := ParseSecretRef(account.SecretRef); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		for _, product := range account.PermittedProducts {
			if product == "" {
				errs = append(errs, fmt.Errorf("%s: permitted_products must not contain empty entries", where))
			}
		}
	}
	return errs
}

func validateLimits(limits LimitsConfig) []error {
	var errs []error
	if limits.TurnDeadlineMs < 0 {
		errs = append(errs, fmt.Errorf("limits.turn_deadline_ms must be at least 1 ms, got %d", limits.TurnDeadlineMs))
	}
	if limits.MaxAttempts > MaxInferenceAttempts {
		errs = append(errs, fmt.Errorf("limits.max_attempts must be at most %d, got %d", MaxInferenceAttempts, limits.MaxAttempts))
	}
	if limits.MaxOutputBytes < 0 {
		errs = append(errs, fmt.Errorf("limits.max_output_bytes must be at least 1, got %d", limits.MaxOutputBytes))
	}
	if limits.MaxContentBytes < 0 {
		errs = append(errs, fmt.Errorf("limits.max_content_bytes must be at least 1, got %d", limits.MaxContentBytes))
	}
	return errs
}

func validatePersistence(p PersistenceConfig) []error {
	var errs []error
	durability := p.Durability
	if durability == "" {
		durability = DurabilityFull // the schema default
	}
	switch durability {
	case DurabilityFull:
		if p.BackupDir == "" {
			errs = append(errs, fmt.Errorf("persistence.durability %s (the default) requires persistence.backup_dir; set persistence.durability to %s to run without backups", DurabilityFull, DurabilityNormal))
		}
	case DurabilityNormal:
	default:
		errs = append(errs, fmt.Errorf("persistence.durability must be %q or %q, got %q", DurabilityFull, DurabilityNormal, p.Durability))
	}
	return errs
}

func validateProviders(p ProvidersConfig) []error {
	var errs []error
	for _, provider := range []struct {
		name string
		cfg  ProviderConfig
	}{
		{"opencode", p.OpenCode},
		{"opencode-go", p.OpenCodeGo},
		{"zen", p.Zen},
	} {
		where := "$.providers." + provider.name
		if provider.cfg.Enabled == nil {
			errs = append(errs, fmt.Errorf("required field %s.enabled is missing", where))
			continue
		}
		if *provider.cfg.Enabled && provider.cfg.Endpoint == "" {
			errs = append(errs, fmt.Errorf("%s: an enabled provider requires %s.endpoint", where, where))
		}
		if len(provider.cfg.Protocols) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one protocol is required in %s.protocols", where, where))
		}
		for _, protocol := range provider.cfg.Protocols {
			if protocol == "" {
				errs = append(errs, fmt.Errorf("%s: protocols must not contain empty entries", where))
			}
		}
	}
	return errs
}

func validateDiscovery(d DiscoveryConfig) []error {
	var errs []error
	if d.RefreshMs < MinDiscoveryRefreshMs {
		errs = append(errs, fmt.Errorf("discovery.refresh_ms must be at least %d ms to avoid busy-looping the metadata source, got %d", MinDiscoveryRefreshMs, d.RefreshMs))
	}
	if d.StaleAfterMs < d.RefreshMs {
		errs = append(errs, fmt.Errorf("discovery.stale_after_ms (%d) must be at least discovery.refresh_ms (%d)", d.StaleAfterMs, d.RefreshMs))
	}
	return errs
}

func validateInference(inf InferenceConfig) []error {
	var errs []error
	if inf.RequestTimeoutMs < 1 || inf.RequestTimeoutMs > MaxInferenceTimeoutMs {
		errs = append(errs, fmt.Errorf("inference.request_timeout_ms must be in [1, %d], got %d", MaxInferenceTimeoutMs, inf.RequestTimeoutMs))
	}
	if inf.MaxTokensPerTurn < 1 || inf.MaxTokensPerTurn > MaxTokensPerTurn {
		errs = append(errs, fmt.Errorf("inference.max_tokens_per_turn must be in [1, %d], got %d", MaxTokensPerTurn, inf.MaxTokensPerTurn))
	}
	if inf.MaxConcurrent < 1 || inf.MaxConcurrent > MaxInferenceConcurrent {
		errs = append(errs, fmt.Errorf("inference.max_concurrent must be in [1, %d], got %d", MaxInferenceConcurrent, inf.MaxConcurrent))
	}
	if inf.QueueDepth < 0 || inf.QueueDepth > MaxInferenceQueueDepth {
		errs = append(errs, fmt.Errorf("inference.queue_depth must be in [0, %d], got %d", MaxInferenceQueueDepth, inf.QueueDepth))
	}
	return errs
}

func validateOperations(ops OperationsConfig) []error {
	var errs []error
	switch ops.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("operations.log_level must be one of debug, info, warn, error; got %q", ops.LogLevel))
	}
	if ops.ShutdownGraceMs < 0 || ops.ShutdownGraceMs > MaxShutdownGraceMs {
		errs = append(errs, fmt.Errorf("operations.shutdown_grace_ms must be in [0, %d], got %d", MaxShutdownGraceMs, ops.ShutdownGraceMs))
	}
	return errs
}

// validateRouteProtocols rejects model-route references whose
// protocol no enabled provider supports. It expands discovery
// patterns first, so a tier that silently expands to nothing
// useful is caught here rather than at first use. A config
// without a providers group (contract version 1) skips this
// check: nothing declares protocols to check against.
func validateRouteProtocols(tiers []TierConfig, cat Catalogue, providers *ProvidersConfig) []error {
	if providers == nil {
		return nil
	}
	supported := map[string]bool{}
	for _, provider := range []ProviderConfig{providers.OpenCode, providers.OpenCodeGo, providers.Zen} {
		if provider.Enabled != nil && *provider.Enabled {
			for _, protocol := range provider.Protocols {
				supported[protocol] = true
			}
		}
	}
	var errs []error
	reported := map[string]bool{}
	for _, tier := range tiers {
		for _, routeID := range ExpandTierRoutes(tier, cat) {
			if reported[routeID] {
				continue
			}
			reported[routeID] = true
			info, known := cat.Route(routeID)
			if !known {
				continue // unknown routes are reported by validateTiers
			}
			if !supported[info.Protocol] {
				errs = append(errs, fmt.Errorf("route %q (tier %q) requires protocol %q, which no enabled provider supports", routeID, tier.Name, info.Protocol))
			}
		}
	}
	return errs
}
