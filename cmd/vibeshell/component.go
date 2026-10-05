package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/opencode"
	"j0s.at/vibeshell/internal/adapters/sandbox"
	"j0s.at/vibeshell/internal/adapters/sqlite"
	ssh "j0s.at/vibeshell/internal/adapters/ssh"
	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/export"
	"j0s.at/vibeshell/internal/observability"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/presentation"
	"j0s.at/vibeshell/internal/routing"
	"j0s.at/vibeshell/internal/system"
	"j0s.at/vibeshell/internal/terminal/renderer"
)

// defaultConfigPath is where the runtime image mounts the configuration.
const defaultConfigPath = "/etc/vibeshell/vibeshell.json"

// component is the constructed service graph. It owns every dependency and
// the process lifecycle; nothing in it contains business rules.
type component struct {
	snapshot  *config.Snapshot
	cfg       *config.Config
	configDir string

	clock  *system.Clock
	random *system.Random

	db          *sqlite.DB
	events      *sqlite.Events
	maintenance *sqlite.Maintenance
	recovery    *sqlite.Recovery
	durable     *durableState

	sandbox    *sandbox.Adapter
	appService *apps.Service
	// gateway is the provider adapter behind the inference admission gate, so
	// every consumer of it — router, MOTD generation, app generation — is
	// bounded by the same configured concurrency and wait-queue limits.
	gateway ports.ModelGateway
	// world resolves the principal's durable world namespaces for the shell's
	// filesystem commands.
	world *worldService
	// tools is the server-owned simulation tool executor behind the
	// application seam. useTools reports whether a real adapter was built.
	tools    application.ToolExecutor
	useTools bool

	health    *sqlite.RouteHealth
	router    *routing.Router
	admin     *admin.Service
	exporter  *export.Service
	renderer  *renderer.Renderer
	motd      *presentation.MOTDService
	identity  presentation.SystemIdentity
	passwords *config.PasswordStore

	coordinator *application.Coordinator
	server      *ssh.Server
	handler     *sshHandler

	readiness *observability.ReadinessReporter
	logger    *observability.Logger

	listener net.Listener
	authMode configAuthMode

	// serveCancel stops the SSH transport (closes the listener and every
	// connection); serveDone is closed when Serve returns. shutdown owns
	// both so it can stop admission and wait for the transport to quiesce.
	serveCancel context.CancelFunc
	serveDone   chan struct{}

	// shutdownOnce makes shutdown idempotent: a signal and a serve error can
	// both request it, and a caller must observe one result.
	shutdownOnce sync.Once
	shutdownErr  error
}

// build constructs the whole component from a validated configuration
// snapshot. It performs the side-effecting constructions (storage, host key,
// sandbox engine, listener) but starts no goroutine: start owns that.
func build(ctx context.Context, snapshot *config.Snapshot, configDir string, readiness *observability.ReadinessReporter, logger *observability.Logger) (*component, error) {
	if snapshot == nil || snapshot.Config == nil {
		return nil, errors.New("configuration snapshot is empty")
	}
	cfg := snapshot.Config

	c := &component{
		snapshot:  snapshot,
		cfg:       cfg,
		configDir: configDir,
		clock:     system.NewClock(),
		random:    system.NewRandom(),
		readiness: readiness,
		logger:    logger,
	}

	// --- storage: open, verify, migrate, recover (PLAN 12.3) ---------------
	if cfg.Persistence == nil || cfg.Persistence.DatabasePath == "" {
		return nil, errors.New("configuration has no persistence.database_path")
	}
	db, err := sqlite.Open(cfg.Persistence.DatabasePath, sqlite.Options{})
	if err != nil {
		readiness.SetUnhealthy(observability.SubsystemStorage, "database could not be opened")
		return nil, fmt.Errorf("open database: %w", err)
	}
	c.db = db
	var eventsOpts sqlite.EventsOptions
	if cfg.Persistence.WriterQueueDepth > 0 {
		eventsOpts.QueueCapacity = cfg.Persistence.WriterQueueDepth
	}
	events, err := sqlite.NewEvents(db.SQL(), eventsOpts)
	if err != nil {
		_ = db.Close()
		readiness.SetUnhealthy(observability.SubsystemStorage, "event store could not be opened")
		return nil, fmt.Errorf("open event store: %w", err)
	}
	c.events = events
	c.maintenance = sqlite.NewMaintenance(db)
	c.recovery = sqlite.NewRecovery(db, events, c.clock)
	readiness.SetHealthy(observability.SubsystemStorage, "database open and migrated")

	// --- durable application, health, and spending state (PLAN 5.6, 9.2, 10.2)
	// The store is open and migrated, so the adapters that own those rows can
	// be constructed before anything reads them.
	state, err := openDurableState(db, c.clock)
	if err != nil {
		_ = db.Close()
		readiness.SetUnhealthy(observability.SubsystemStorage, "durable stores could not be opened")
		return nil, err
	}
	c.durable = state
	c.health = state.health
	c.world = newWorldService(db, c.clock)

	// --- recovery of interrupted sessions (PLAN 12.3) ----------------------
	if _, err := c.recovery.RecoverIncomplete(ctx); err != nil {
		// Recovery is best effort at startup: a failure is reported but does
		// not prevent serving, matching PLAN 12.3's "recover incomplete
		// session/turn/attempt states".
		c.logger.Warn("startup recovery did not complete",
			attr(observability.FieldOperation, "recover"), attr(observability.FieldError, err.Error()))
	}

	// --- sandbox engine ----------------------------------------------------
	heapPages := uint32(0)
	if cfg.Apps != nil && cfg.Apps.HeapLimitBytes > 0 && cfg.Apps.HeapLimitBytes%65536 == 0 {
		heapPages = uint32(cfg.Apps.HeapLimitBytes / 65536)
	}
	adapter, err := sandbox.NewAdapter(ctx, heapPages, sandbox.Config{})
	if err != nil {
		return nil, fmt.Errorf("initialize sandbox engine: %w", err)
	}
	c.sandbox = adapter
	readiness.SetHealthy(observability.SubsystemSandbox, "sandbox engine compiled")

	// --- simulation tool layer --------------------------------------------
	c.tools, c.useTools = buildToolAdapter(c)

	// --- provider gateway --------------------------------------------------
	// One inference implementation per configured provider is registered by
	// route; the registry is the single gateway the rest of the graph sees.
	registry, err := buildProviders(cfg, snapshot)
	if err != nil {
		return nil, err
	}
	gateway, err := buildAdmittedGateway(cfg, registry)
	if err != nil {
		return nil, err
	}
	c.gateway = &requestLoggingGateway{inner: gateway, logger: c.logger, metrics: observability.DefaultRegistry}

	// --- host key and SSH listener ----------------------------------------
	hostKeyPath := ""
	address := ""
	port := 0
	if cfg.SSH != nil {
		hostKeyPath = cfg.SSH.HostKeyFile
		address = cfg.SSH.ListenAddress
		port = cfg.SSH.ListenPort
	}
	if hostKeyPath == "" || port == 0 {
		return nil, errors.New("configuration has no usable ssh.host_key_file/listen_port")
	}
	signer, err := ssh.LoadOrGenerateHostKey(hostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load host key: %w", err)
	}

	// --- authentication ----------------------------------------------------
	var auth *config.Auth
	if cfg.Auth != nil {
		auth = cfg.Auth
	}
	if auth == nil {
		return nil, errors.New("configuration has no auth group")
	}
	c.authMode = authModePublic
	if auth.Mode == config.AuthModeSecure {
		c.authMode = authModeSecure
		store, err := config.OpenPasswordStore(auth.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("open password store: %w", err)
		}
		c.passwords = store
	}

	// --- routing, health, and generated-application lifecycle ---------------
	c.appService = apps.NewService(state.apps, c.sandbox, c.clock, c.random, state.apps)
	c.router = buildRouter(c.gateway, state, c.clock, c.random, buildRoutingConfig(cfg, buildDiscovery(ctx, cfg, c.clock, c.logger)))

	// --- terminal renderer -------------------------------------------------
	rend, err := renderer.New(db, renderer.DefaultLimits())
	if err != nil {
		return nil, fmt.Errorf("build terminal renderer: %w", err)
	}
	c.renderer = rend

	// --- presentation ------------------------------------------------------
	c.identity = buildIdentity(cfg)
	if err := c.identity.Validate(); err != nil {
		return nil, fmt.Errorf("invalid system identity: %w", err)
	}
	motd, err := buildMOTD(cfg, c.configDir, c.identity, c.router, c.clock)
	if err != nil {
		// A missing administrator prompt file must not prevent the service
		// from starting; only the generated MOTD degrades to the truthful
		// fallback.
		c.logger.Warn("MOTD presentation is degraded",
			attr(observability.FieldOperation, "motd"), attr(observability.FieldError, err.Error()))
		motd = nil
	}
	c.motd = motd

	// --- coordinator -------------------------------------------------------
	coordinator, err := application.NewCoordinator(application.CoordinatorOptions{
		Engine:    buildTurnEngine(ctx, c),
		Tools:     c.tools,
		Events:    events,
		World:     db,
		Content:   db,
		Renderer:  rend,
		Clock:     c.clock,
		Random:    c.random,
		Snapshots: &staticSnapshots{snapshot: buildConfigSnapshot(cfg)},
		Limits:    buildTurnLimits(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("build coordinator: %w", err)
	}
	c.coordinator = coordinator

	// --- inbound SSH server ------------------------------------------------
	handler := &sshHandler{
		coordinator: coordinator,
		content:     db,
		motd:        motd,
		identity:    c.identity,
		authMode:    c.authMode,
		sharing:     cfg.Sharing != nil && cfg.Sharing.Enabled,
		recording:   true,
		logger:      logger,
		terminal: terminalBounds{
			maxCols: terminalMaxCols(cfg),
			maxRows: terminalMaxRows(cfg),
		},
		maxPasteBytes: terminalMaxPaste(cfg),
	}
	if c.authMode == authModeSecure {
		handler.passwords = &passwordLookup{store: c.passwords, clock: c.clock}
	}
	serverOpts := ssh.Options{
		Mode:    sshMode(c.authMode),
		HostKey: signer,
		Handler: handler,
		Logger:  logger.Logger,
		Limits:  sshLimits(cfg),
	}
	if c.authMode == authModeSecure {
		serverOpts.Passwords = &passwordAuthenticator{store: c.passwords}
	}
	server, err := ssh.NewServer(serverOpts)
	if err != nil {
		return nil, fmt.Errorf("build SSH server: %w", err)
	}
	c.server = server
	c.handler = handler

	// --- admin and export --------------------------------------------------
	c.exporter = buildExporter(c)
	c.admin = buildAdmin(c)

	// Bind the listener during construction so a port conflict fails
	// startup before any goroutine runs.
	listenAddr := address
	if listenAddr == "" {
		listenAddr = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(listenAddr, fmt.Sprint(port)))
	if err != nil {
		return nil, fmt.Errorf("listen on %s:%d: %w", listenAddr, port, err)
	}
	c.listener = ln

	if len(cfg.Accounts) == 0 {
		readiness.SetDegraded(observability.SubsystemProvider, "no provider account configured; generation unavailable")
	} else {
		readiness.SetHealthy(observability.SubsystemProvider, "provider routes configured")
	}
	readiness.SetHealthy(observability.SubsystemRouting, "routing policy loaded")
	readiness.SetHealthy(observability.SubsystemConfig, "configuration validated")

	return c, nil
}

// start serves SSH until the context is cancelled by a signal, then shuts
// down in PLAN 12.3 order.
func (c *component) start(ctx context.Context) error {
	c.readiness.SetHealthy(observability.SubsystemSSH, "listener ready")
	c.logger.Info("vibeshell ready",
		attr(observability.FieldComponent, "ssh"),
		attr(observability.FieldOperation, "startup"),
		attr(observability.FieldResult, "success"),
		attr(observability.FieldService, "vibeshell"),
	)
	if overall, msg := c.readiness.Overall(); overall != observability.StatusHealthy {
		c.logger.Warn("service is not fully ready",
			attr(observability.FieldOperation, "startup"),
			attr(observability.FieldHealthStatus, overall.String()),
			attr(observability.FieldResult, msg))
	}

	serveCtx, cancelServe := context.WithCancel(ctx)
	c.serveCancel = cancelServe
	c.serveDone = make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		defer close(c.serveDone)
		serveErr <- c.server.Serve(serveCtx, c.listener)
	}()

	select {
	case <-ctx.Done():
		return c.shutdown()
	case err := <-serveErr:
		// Serve returned on its own (listener failure); still shut down.
		shutdownErr := c.shutdown()
		if err != nil {
			return err
		}
		return shutdownErr
	}
}

// shutdown stops accepting sessions, waits for in-flight session handlers,
// ends live sessions, flushes and closes the event store, and only then
// releases the sandbox engine and storage (PLAN 12.3). It is idempotent.
func (c *component) shutdown() error {
	c.shutdownOnce.Do(func() { c.shutdownErr = c.shutdownAll() })
	return c.shutdownErr
}

// shutdownAll performs the ordered shutdown exactly once.
func (c *component) shutdownAll() error {
	c.logger.Info("shutting down", attr(observability.FieldOperation, "shutdown"), attr(observability.FieldShutdown, "begin"))
	c.readiness.SetDegraded(observability.SubsystemSSH, "not accepting new sessions")

	graceMs := int64(5000)
	if c.cfg.Operations != nil && c.cfg.Operations.ShutdownGraceMs > 0 {
		graceMs = c.cfg.Operations.ShutdownGraceMs
	}
	deadline := time.Now().Add(time.Duration(graceMs) * time.Millisecond)
	shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	var firstErr error

	// 1. Stop accepting new sessions and stop the transport. Cancelling the
	// serve context closes the listener and every connection, which ends the
	// in-flight handlers' reads.
	if c.serveCancel != nil {
		c.serveCancel()
	}

	// 2. Wait for in-flight session handlers to finish. Each handler records
	// its session.end on return, so waiting here is what guarantees the end
	// event is durable before the store closes. The wait is bounded by the
	// grace period; a handler that overruns is reported, not waited on
	// forever.
	if c.handler != nil {
		if !c.handler.sessions.wait(deadline) {
			c.logger.Warn("session handlers did not finish within the shutdown grace period",
				attr(observability.FieldOperation, "shutdown"),
				attr(observability.FieldResult, "timeout"))
		}
	}
	awaitServeDone(c.serveDone, deadline)

	// 3. End any session the coordinator still holds as live (a no-op for the
	// sessions the handlers already ended) and refuse further accepts.
	if c.coordinator != nil {
		if err := c.coordinator.Shutdown(shutdownCtx, application.EndShutdown); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 4. Flush and close the event store before storage itself.
	if c.events != nil {
		c.events.Close()
	}

	// 5. Release the sandbox engine and close storage.
	if c.sandbox != nil {
		if err := c.sandbox.Close(context.Background()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.db != nil {
		if err := c.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.logger.Info("metrics snapshot recorded",
		attr(observability.FieldOperation, "shutdown"),
		attr(observability.FieldResult, fmt.Sprint(len(observability.GetSnapshot()))))
	c.logger.Info("shutdown complete", attr(observability.FieldOperation, "shutdown"), attr(observability.FieldShutdown, "complete"))
	return firstErr
}

// awaitServeDone waits until Serve has returned or the deadline passes. It is
// separate from the handler wait because Serve returning and every handler
// returning are different conditions (a connection goroutine is not the
// handler goroutine), and shutdown only needs to bound the wait.
func awaitServeDone(done <-chan struct{}, deadline time.Time) {
	if done == nil {
		return
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// ---------------------------------------------------------------------------
// Construction helpers
// ---------------------------------------------------------------------------

// openSnapshot loads and validates the configuration file and publishes the
// first snapshot.
func openSnapshot(configPath string, dirs []string) (*config.Snapshot, string, error) {
	if configPath == "" {
		return nil, "", errConfigRequired
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, "", fmt.Errorf("read configuration %q: %w", configPath, err)
	}
	loader := config.NewLoader(path.Dir(configPath), config.Options{SecretDirs: dirs})
	snapshot, err := loader.Load(raw)
	if err != nil {
		return nil, "", err
	}
	return snapshot, path.Dir(configPath), nil
}

// buildIdentity maps configuration identity facts to the presentation
// identity, filling documented defaults for absent optional fields.
func buildIdentity(cfg *config.Config) presentation.SystemIdentity {
	identity := presentation.DefaultSystemIdentity()
	if cfg.Identity != nil {
		if cfg.Identity.SystemName != "" {
			identity.SystemName = cfg.Identity.SystemName
		}
		if cfg.Identity.ShellName != "" {
			identity.ShellName = cfg.Identity.ShellName
		}
		if cfg.Identity.Hostname != "" {
			identity.Hostname = cfg.Identity.Hostname
		}
	}
	return identity
}

// identityFields reduces the presentation identity to the fallback engine's
// view.
func (c *component) identityFields() presentationIdentity {
	return presentationIdentity{
		System:     c.identity.SystemName,
		Shell:      c.identity.ShellName,
		Hostname:   c.identity.Hostname,
		HomePrefix: "/home",
	}
}

// buildTurnLimits maps the configured app bounds onto the coordinator's turn
// limits. A zero apps.max_repairs leaves the documented default in place, since
// the config cannot distinguish "unset" from an explicit zero.
func buildTurnLimits(cfg *config.Config) application.Limits {
	limits := application.DefaultLimits()
	if cfg.Apps != nil && cfg.Apps.MaxRepairs > 0 {
		limits.MaxRepairs = cfg.Apps.MaxRepairs
	}
	return limits
}

// appLimitDeadline and appLimitMemory map the configured generated-application
// bounds onto the sandbox limits. Zero leaves the engine's documented default.
func appLimitDeadline(cfg *config.Config) int64 {
	if cfg.Apps != nil && cfg.Apps.ExecutionDeadlineMs > 0 {
		return int64(cfg.Apps.ExecutionDeadlineMs)
	}
	return 0
}

func appLimitMemory(cfg *config.Config) int64 {
	if cfg.Apps != nil && cfg.Apps.HeapLimitBytes > 0 {
		return int64(cfg.Apps.HeapLimitBytes)
	}
	return 0
}

// buildConfigSnapshot pins the coordinator's per-turn configuration values.
func buildConfigSnapshot(cfg *config.Config) application.ConfigSnapshot {
	snapshot := application.ConfigSnapshot{
		ConfigVersion:  1,
		PromptVersion:  presentation.DefaultPromptVersion,
		ScopePolicy:    scopePolicy(cfg),
		TurnDeadlineMs: 120000,
		MaxAttempts:    3,
		MaxRebases:     2,
	}
	if cfg.Limits != nil {
		if cfg.Limits.TurnDeadlineMs > 0 {
			snapshot.TurnDeadlineMs = int64(cfg.Limits.TurnDeadlineMs)
		}
		if cfg.Limits.MaxAttempts > 0 {
			snapshot.MaxAttempts = cfg.Limits.MaxAttempts
		}
	}
	if cfg.World != nil && cfg.World.ConflictRetries >= 0 {
		snapshot.MaxRebases = cfg.World.ConflictRetries
	}
	return snapshot
}

// scopePolicy maps the configured sharing mode to the domain scope policy.
func scopePolicy(cfg *config.Config) domain.ScopePolicy {
	if cfg.Sharing == nil || !cfg.Sharing.Enabled {
		return domain.RestrictedScopePolicy()
	}
	policy := domain.DefaultScopePolicy()
	if cfg.Sharing.PolicyRevision > 0 {
		policy.PolicyRevision = int64(cfg.Sharing.PolicyRevision)
	}
	return policy
}

// hasAccounts reports whether any provider account is configured. It drives
// only the wording of the fallback engine's unavailable message.
func hasAccounts(cfg *config.Config) bool { return len(cfg.Accounts) > 0 }

// sshMode maps the handler's auth mode to the transport's mode.
func sshMode(mode configAuthMode) ssh.Mode {
	if mode == authModeSecure {
		return ssh.ModePassword
	}
	return ssh.ModePublic
}

// terminalMaxCols/Rows read the configured terminal bounds, defaulting to the
// SSH adapter's conventional maximum when unset.
func terminalMaxCols(cfg *config.Config) int {
	if cfg.SSH != nil && cfg.SSH.MaxTerminalCols > 0 {
		return cfg.SSH.MaxTerminalCols
	}
	return 4096
}

func terminalMaxRows(cfg *config.Config) int {
	if cfg.SSH != nil && cfg.SSH.MaxTerminalRows > 0 {
		return cfg.SSH.MaxTerminalRows
	}
	return 4096
}

// terminalMaxPaste reads the configured paste bound, or zero to keep the input
// decoder's default.
func terminalMaxPaste(cfg *config.Config) int {
	if cfg.Terminal != nil && cfg.Terminal.MaxPasteBytes > 0 {
		return cfg.Terminal.MaxPasteBytes
	}
	return 0
}

// sshLimits maps configured SSH bounds onto the transport's limits, keeping
// the adapter defaults for unset fields.
func sshLimits(cfg *config.Config) ssh.Limits {
	limits := ssh.DefaultLimits
	if cfg.SSH == nil {
		return limits
	}
	if cfg.SSH.HandshakeTimeoutMs > 0 {
		limits.HandshakeTimeout = time.Duration(cfg.SSH.HandshakeTimeoutMs) * time.Millisecond
	}
	if cfg.Auth != nil && cfg.Auth.MaxAttemptsPerConnection > 0 {
		limits.MaxAuthTries = cfg.Auth.MaxAttemptsPerConnection
	}
	if cfg.SSH.MaxTerminalCols > 0 || cfg.SSH.MaxTerminalRows > 0 {
		max := limits.MaxDimension
		if cfg.SSH.MaxTerminalCols > max {
			max = cfg.SSH.MaxTerminalCols
		}
		if cfg.SSH.MaxTerminalRows > max {
			max = cfg.SSH.MaxTerminalRows
		}
		limits.MaxDimension = max
	}
	return limits
}

// buildGateway assembles the direct-HTTP gateway over every configured route.
// It remains for the gateway-focused tests; production composition uses
// buildProviders, which registers each route with its owning implementation.
func buildGateway(cfg *config.Config, snapshot *config.Snapshot) *opencode.Gateway {
	return buildHTTPProvider(cfg, snapshot, allRoutes(cfg))
}

// gatewaySecrets resolves a KeyRef to secret material using the snapshot's
// resolved reference map. The account's configured secret reference is
// resolved through the config adapter at load time; this adapter maps the
// KeyRef identity back to that value.
type gatewaySecrets struct {
	snapshot *config.Snapshot
	cfg      *config.Config
}

// ResolveKey returns the resolved secret for a key reference. The map is
// keyed by the account ID because the gateway's KeyRef is derived from the
// account's secret reference in this build.
func (g gatewaySecrets) ResolveKey(ref domain.KeyRef) (string, error) {
	for _, account := range g.cfg.Accounts {
		if accountKeyRef(account) == ref {
			secret, ok := g.snapshot.Secret(account.SecretRef)
			if !ok {
				return "", errorsNewSecret(ref)
			}
			return string(secret.Bytes()), nil
		}
	}
	return "", errorsNewSecret(ref)
}

// accountKeyRef derives the KeyRef of one account. It is the account's own
// identity value carried with the key prefix, so it is stable and never
// reveals secret material.
func accountKeyRef(account config.Account) domain.KeyRef {
	value := account.ID
	if _, v, err := domain.ParseIdentity(account.ID); err == nil {
		value = v
	}
	ref, err := domain.ParseKeyRef(domain.PrefixKeyRef + "_" + value)
	if err != nil {
		return domain.KeyRef{}
	}
	return ref
}

func errorsNewSecret(ref domain.KeyRef) error {
	return fmt.Errorf("opencode: no secret resolved for key reference %s", ref.Value())
}
