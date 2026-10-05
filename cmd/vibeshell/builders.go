package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"path"
	"syscall"
	"time"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/export"
	"j0s.at/vibeshell/internal/inference"
	"j0s.at/vibeshell/internal/observability"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/presentation"
	"j0s.at/vibeshell/internal/system"
)

// buildAdmittedGateway puts the inference admission gate in front of the
// provider gateway, so the configured concurrency and wait-queue limits apply to
// every model request no matter which component issues it (PLAN 9.3).
//
// The gate is opt-in. Without inference.global_concurrency the operator has
// configured no instance-wide bound, and a gate without one protects nothing,
// so the gateway is used unchanged. When the bound is configured, a limit the
// gate cannot honor fails startup instead of quietly meaning "no limit" once
// requests are already running.
func buildAdmittedGateway(cfg *config.Config, inner ports.ModelGateway) (ports.ModelGateway, error) {
	if cfg.Inference == nil || cfg.Inference.GlobalConcurrency <= 0 {
		return inner, nil
	}
	gate, err := inference.NewGate(inner, inference.Config{
		GlobalConcurrency:     cfg.Inference.GlobalConcurrency,
		MaxAccountConcurrency: cfg.Inference.MaxAccountConcurrency,
		WaitQueueDepth:        cfg.Inference.WaitQueueDepth,
	})
	if err != nil {
		return nil, err
	}
	return gate, nil
}

// requestLoggingGateway records the outcome and duration of every model
// request at debug level. The provider adapter normalizes faults into error
// envelopes and the callers that swallow them (MOTD and generation fall back
// truthfully) would otherwise leave an operator unable to tell a provider
// outage from a local refusal. The decorator carries no routing policy: it
// forwards the request and its result unchanged.
type requestLoggingGateway struct {
	inner   ports.ModelGateway
	logger  *observability.Logger
	metrics *observability.Registry
}

var _ ports.ModelGateway = (*requestLoggingGateway)(nil)

// Request forwards one model request and logs its route, account, duration,
// and (on failure) the normalized error message. It also records the model
// request counters, so the operational metric set is populated from real
// traffic.
func (g *requestLoggingGateway) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	start := time.Now()
	resp, err := g.inner.Request(ctx, req)
	recordModelRequest(g.metrics, err)
	attrs := []slog.Attr{
		attr(observability.FieldOperation, "model_request"),
		attr(observability.FieldRouteID, req.RouteID.String()),
		attr(observability.FieldAccountID, req.AccountID.String()),
		attr(observability.FieldDurationMS, fmt.Sprint(time.Since(start).Milliseconds())),
	}
	if err != nil {
		attrs = append(attrs,
			attr(observability.FieldResult, "error"),
			attr(observability.FieldError, err.Error()))
		g.logger.Debug("model request failed", attrs...)
		return resp, err
	}
	attrs = append(attrs, attr(observability.FieldResult, "success"))
	g.logger.Debug("model request completed", attrs...)
	return resp, nil
}

// recordModelRequest increments the model-request counters. A registry error
// (an unlisted name or label) is impossible here and is ignored rather than
// failing a request.
func recordModelRequest(metrics *observability.Registry, err error) {
	if metrics == nil {
		return
	}
	if counter, cerr := metrics.Counter(observability.MetricModelRequestsTotal, nil); cerr == nil {
		counter.Inc()
	}
	if err != nil {
		if counter, cerr := metrics.Counter(observability.MetricModelRequestErrors, nil); cerr == nil {
			counter.Inc()
		}
	}
}

// buildMOTD assembles the per-session MOTD policy: the administrator prompt
// file, the model-gateway generator, and the hard presentation bounds. When
// no prompt file or no provider account exists, the returned service still
// produces the truthful service-unavailable message.
func buildMOTD(cfg *config.Config, configDir string, identity presentation.SystemIdentity, executor failoverExecutor, clock *system.Clock) (*presentation.MOTDService, error) {
	provider, err := newFilePromptProvider(cfg, configDir)
	if err != nil {
		return nil, err
	}
	generator := &gatewayMOTDGenerator{
		executor:  executor,
		clock:     clock,
		maxWaitMs: 20000,
	}
	return presentation.NewMOTDService(presentation.DefaultMOTDConfig(), provider, generator, identity)
}

// firstRouteAccount selects the deterministic route/account pair used for
// MOTD generation: the first configured route for the first configured
// account. It returns zero values when none is configured, which makes the
// generator report an error and the MOTD policy fall back truthfully.
func firstRouteAccount(cfg *config.Config) (domain.RouteID, domain.AccountID) {
	var route domain.RouteID
	for _, r := range cfg.Routes {
		id, err := domain.ParseRouteID(r.ID)
		if err != nil {
			continue
		}
		route = id
		break
	}
	var account domain.AccountID
	for _, a := range cfg.Accounts {
		id, err := domain.ParseAccountID(a.ID)
		if err != nil {
			continue
		}
		account = id
		break
	}
	return route, account
}

// buildAdmin assembles the operator service over the adapters that exist in
// this build. Optional dependencies that no merged adapter provides (probe
// runner) are left nil so the admin surface reports them as unavailable rather
// than pretending to run.
func buildAdmin(c *component) *admin.Service {
	loader := config.NewLoader(c.configDir, config.Options{SecretDirs: []string{}})
	deps := admin.Deps{
		Clock:     c.clock,
		Random:    c.random,
		Validator: loader,
		Sessions:  c.recovery,
		Recovery:  c.recovery,
		Backups:   c.maintenance,
		Exporter:  c.exporter,
		Health:    c.health,
	}
	if c.passwords != nil {
		deps.Passwords = admin.NewPasswordStoreBinding(c.passwords, c.random, config.DefaultParams)
	}
	if c.durable != nil && c.appService != nil {
		deps.Apps = admin.NewAppServiceBinding(c.durable.apps, c.appService)
	}
	if c.cfg.SSH != nil && c.cfg.SSH.HostKeyFile != "" {
		deps.BackupExtras = []admin.BackupExtra{{Name: "host_key", Path: c.cfg.SSH.HostKeyFile}}
	}
	return admin.New(deps)
}

// buildExporter assembles the research export service over the event and
// content stores. Exports use a permissive admin policy so an operator export
// can span scopes; the simulated shell never reaches this path.
func buildExporter(c *component) *export.Service {
	svc, err := export.NewService(c.events, c.events, c.db, c.clock, c.random, export.Options{
		Policy:          domain.DefaultScopePolicy(),
		ExporterVersion: "vibeshell/d03",
	})
	if err != nil {
		// A construction failure here is a programming error, not an
		// operator condition. The service still starts so the SSH path stays
		// available, but the operator must see that export is unavailable.
		c.logger.Error("export service is unavailable",
			attr(observability.FieldComponent, "export"),
			attr(observability.FieldOperation, "compose"),
			attr(observability.FieldResult, "error"),
			attr(observability.FieldError, err.Error()))
		return nil
	}
	return svc
}

// startupContext returns a context cancelled by SIGINT/SIGTERM so the process
// owns signal handling instead of a library.
func startupContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// storageSummary is the minimal storage health the status subcommand reports.
type storageSummary struct {
	SchemaVersion int
	EventRows     int64
}

// inspectStorage opens the configured database and reports its schema version
// and event count for the status subcommand. sqlite.Open applies pending
// migrations, so a status check against a current database is a no-op write
// path rather than a read-only probe.
func inspectStorage(ctx context.Context, cfg *config.Config) (storageSummary, error) {
	if cfg.Persistence == nil || cfg.Persistence.DatabasePath == "" {
		return storageSummary{}, fmt.Errorf("no persistence.database_path configured")
	}
	db, err := sqlite.Open(cfg.Persistence.DatabasePath, sqlite.Options{})
	if err != nil {
		return storageSummary{}, err
	}
	defer db.Close()
	version, err := sqlite.SchemaVersion(ctx, db.SQL())
	if err != nil {
		return storageSummary{}, err
	}
	summary := storageSummary{SchemaVersion: version}
	var count int64
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err == nil {
		summary.EventRows = count
	}
	return summary, nil
}

// resolvePromptPath returns the resolved MOTD prompt path for status output.
func resolvePromptPath(cfg *config.Config, configDir string) string {
	if cfg.Prompts == nil || cfg.Prompts.Motd == "" {
		return ""
	}
	if path.IsAbs(cfg.Prompts.Motd) {
		return cfg.Prompts.Motd
	}
	return path.Join(configDir, cfg.Prompts.Motd)
}
