package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/export"
	"j0s.at/vibeshell/internal/system"
)

// adminCommand is the internal-only operator surface (PLAN 10.5, 12.4). It is
// reached through the container executable, for example
// `podman exec vibeshell vibeshell admin ...`; no second public port is opened
// and none of these operations exist as simulated shell commands. Passwords
// and secret material are read from the TTY or stdin, never from argv.
func adminCommand(args []string) error {
	if len(args) == 0 {
		printAdminUsage()
		return nil
	}
	if isFlag(args[0]) {
		return fmt.Errorf("admin needs an operation (try: vibeshell admin help)")
	}
	op := args[0]
	rest := args[1:]

	switch op {
	case "config":
		return adminConfig(rest)
	case "user":
		return adminUser(rest)
	case "sessions":
		return adminSessions(rest)
	case "export":
		return adminExport(rest)
	case "status":
		return adminStatus(rest)
	case "probe":
		return adminProbe(rest)
	case "integrity":
		return adminIntegrity(rest)
	case "backup":
		return adminBackup(rest)
	case "restore":
		return adminRestore(rest)
	case "app":
		return adminApp(rest)
	case "help", "-h", "--help":
		printAdminUsage()
		return nil
	default:
		return fmt.Errorf("unknown admin operation %q (try: vibeshell admin help)", op)
	}
}

// printAdminUsage writes the admin operation help to stdout.
func printAdminUsage() {
	fmt.Print(`VibeShell admin operations (internal-only; run through the container executable)

Usage:
  vibeshell admin config validate [-config PATH] [-secret-dir D]
  vibeshell admin user list|add|set-password|enable|disable|remove [-config PATH] -username NAME
        add/set-password read the password from the TTY or stdin, never argv
  vibeshell admin sessions list [-config PATH] [-limit N] [-cursor C]
  vibeshell admin export [-config PATH] -format jsonl|transcript|asciicast -out PATH
        [-session ID ...] [-user ID ...] [-from-ms N] [-to-ms N] [-redact-secrets]
  vibeshell admin status [-config PATH]
  vibeshell admin probe trigger [-config PATH] -route RID [-account AID] [-scope S]
  vibeshell admin probe run [-config PATH]
  vibeshell admin integrity [-config PATH]
  vibeshell admin backup [-config PATH] -dest DIR
  vibeshell admin restore [-config PATH] -source DIR -dest-db PATH [-dest-extras DIR]
  vibeshell admin app rollback [-config PATH] -app AID -version AVID [-reason TEXT]

Every operation requires the strict JSON configuration:
  -config PATH     configuration file (default /etc/vibeshell/vibeshell.json)
  -secret-dir D    allowed secret directory (repeatable)
`)
}

// ---------------------------------------------------------------------------
// Dependency construction
// ---------------------------------------------------------------------------

// adminSnapshot loads and validates the configuration snapshot. Admin
// operations read the same strict JSON document the service runs from.
func adminSnapshot(configPath string, dirs []string) (*config.Snapshot, string, error) {
	snapshot, configDir, err := openSnapshot(configPath, dirs)
	if err != nil {
		return nil, "", err
	}
	return snapshot, configDir, nil
}

// configFlagsFor registers the shared config flags on a subcommand flag set.
func configFlagsFor(fs *flag.FlagSet, defaultConfig string) (*string, *secretDirs) {
	return configFlags(fs, defaultConfig)
}

// adminStorage is an open database plus its event store and a close function.
// It is opened lazily: an operation that needs no storage (config validation)
// never touches the database or applies migrations.
type adminStorage struct {
	db     *sqlite.DB
	events *sqlite.Events
	clock  *system.Clock
}

// openAdminStorage opens the configured database and event store. It records
// its own clock so recovery has a consistent time source.
func openAdminStorage(cfg *config.Config) (*adminStorage, func() error, error) {
	if cfg.Persistence == nil || cfg.Persistence.DatabasePath == "" {
		return nil, nil, errors.New("administration requires persistence.database_path in the configuration")
	}
	db, err := sqlite.Open(cfg.Persistence.DatabasePath, sqlite.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("open database %q: %w", cfg.Persistence.DatabasePath, err)
	}
	clock := system.NewClock()
	var opts sqlite.EventsOptions
	if cfg.Persistence.WriterQueueDepth > 0 {
		opts.QueueCapacity = cfg.Persistence.WriterQueueDepth
	}
	events, err := sqlite.NewEvents(db.SQL(), opts)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("open event store: %w", err)
	}
	storage := &adminStorage{db: db, events: events, clock: clock}
	closeFn := func() error {
		events.Close()
		return db.Close()
	}
	return storage, closeFn, nil
}

// openAdminPasswords opens the secure-mode password store when the
// configuration selects secure authentication. Public mode has no password
// file, so user maintenance reports a typed unavailable error instead of
// inventing one.
func openAdminPasswords(cfg *config.Config) (*config.PasswordStore, error) {
	if cfg.Auth == nil || cfg.Auth.Mode != config.AuthModeSecure {
		return nil, nil
	}
	if cfg.Auth.PasswordFile == "" {
		return nil, errors.New("secure authentication is configured without auth.password_file")
	}
	store, err := config.OpenPasswordStore(cfg.Auth.PasswordFile)
	if err != nil {
		return nil, fmt.Errorf("open password store %q: %w", cfg.Auth.PasswordFile, err)
	}
	return store, nil
}

// newAdminService assembles the admin service over exactly the dependencies
// this build can provide. Passing a nil storage or password store leaves the
// matching operations reporting a typed unavailable error rather than
// pretending to run.
func newAdminService(cfg *config.Config, configDir string, storage *adminStorage, passwords *config.PasswordStore) (*admin.Service, error) {
	clock := system.NewClock()
	random := system.NewRandom()
	loader := config.NewLoader(configDir, config.Options{})

	deps := admin.Deps{
		Clock:     clock,
		Random:    random,
		Validator: loader,
	}
	if passwords != nil {
		deps.Passwords = admin.NewPasswordStoreBinding(passwords, random, config.DefaultParams)
	}
	if storage != nil {
		deps.Sessions = sqlite.NewRecovery(storage.db, storage.events, storage.clock)
		deps.Recovery = sqlite.NewRecovery(storage.db, storage.events, storage.clock)
		deps.Backups = sqlite.NewMaintenance(storage.db)
		// Route and account health is durable, so a separate exec process reads
		// the records the running service maintains instead of an empty store.
		health, err := sqlite.NewRouteHealth(storage.db.SQL(), storage.clock)
		if err != nil {
			return nil, fmt.Errorf("open route health store: %w", err)
		}
		deps.Health = health
		exporter, err := export.NewService(storage.events, storage.events, storage.db, storage.clock, random, export.Options{
			Policy:          domain.DefaultScopePolicy(),
			ExporterVersion: "vibeshell/admin",
		})
		if err != nil {
			return nil, fmt.Errorf("build export service: %w", err)
		}
		deps.Exporter = exporter

		// App rollback only moves the active pointer; it never runs a sandbox,
		// so the CLI admin needs no sandbox engine here.
		state, err := openDurableState(storage.db, storage.clock)
		if err != nil {
			return nil, fmt.Errorf("open durable app state: %w", err)
		}
		appService := apps.NewService(state.apps, nil, storage.clock, random, state.apps)
		deps.Apps = admin.NewAppServiceBinding(state.apps, appService)
	}
	if cfg.SSH != nil && cfg.SSH.HostKeyFile != "" {
		deps.BackupExtras = []admin.BackupExtra{{Name: "host_key", Path: cfg.SSH.HostKeyFile}}
	}
	return admin.New(deps), nil
}

// ---------------------------------------------------------------------------
// config validate
// ---------------------------------------------------------------------------

// adminConfig dispatches the config subcommands.
func adminConfig(args []string) error {
	if len(args) == 0 || isFlag(args[0]) {
		return fmt.Errorf("config needs an operation: validate")
	}
	switch args[0] {
	case "validate":
		return adminConfigValidate(args[1:])
	default:
		return fmt.Errorf("unknown config operation %q (try: validate)", args[0])
	}
}

// adminConfigValidate parses and semantically validates the configuration file
// without resolving secret values or opening storage.
func adminConfigValidate(args []string) error {
	fs := flag.NewFlagSet("admin config validate", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	service, err := newAdminService(snapshot.Config, configDir, nil, nil)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read configuration %q: %w", *configPath, err)
	}
	if err := service.ValidateConfig(context.Background(), raw); err != nil {
		return err
	}
	fmt.Printf("configuration is valid: %s\n", snapshot)
	return nil
}

// ---------------------------------------------------------------------------
// user maintenance
// ---------------------------------------------------------------------------

// adminUser dispatches user/hash maintenance. The password never appears in
// argv; add and set-password read it from the TTY or stdin.
func adminUser(args []string) error {
	if len(args) == 0 || isFlag(args[0]) {
		return fmt.Errorf("user needs an operation: list, add, set-password, enable, disable, remove")
	}
	op := args[0]
	rest := args[1:]
	if op == "list" {
		return adminUserList(rest)
	}

	fs := flag.NewFlagSet("admin user "+op, flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	username := fs.String("username", "", "user name (required)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if strings.TrimSpace(*username) == "" {
		return errors.New("-username is required")
	}

	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	passwords, err := openAdminPasswords(snapshot.Config)
	if err != nil {
		return err
	}
	service, err := newAdminService(snapshot.Config, configDir, nil, passwords)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch op {
	case "add":
		password, err := readPassword(fmt.Sprintf("password for %s: ", *username))
		if err != nil {
			return err
		}
		id, err := service.AddUser(ctx, *username, password)
		clearBytes(password)
		if err != nil {
			return err
		}
		fmt.Printf("user %q created with identity %s\n", *username, id)
	case "set-password":
		password, err := readPassword(fmt.Sprintf("new password for %s: ", *username))
		if err != nil {
			return err
		}
		err = service.SetPassword(ctx, *username, password)
		clearBytes(password)
		if err != nil {
			return err
		}
		fmt.Printf("password updated for %q\n", *username)
	case "enable":
		if err := service.SetEnabled(ctx, *username, true); err != nil {
			return err
		}
		fmt.Printf("user %q enabled\n", *username)
	case "disable":
		if err := service.SetEnabled(ctx, *username, false); err != nil {
			return err
		}
		fmt.Printf("user %q disabled\n", *username)
	case "remove":
		if err := service.RemoveUser(ctx, *username); err != nil {
			return err
		}
		fmt.Printf("user %q removed\n", *username)
	default:
		return fmt.Errorf("unknown user operation %q", op)
	}
	return nil
}

// adminUserList lists password-file entries without hash material.
func adminUserList(args []string) error {
	fs := flag.NewFlagSet("admin user list", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	passwords, err := openAdminPasswords(snapshot.Config)
	if err != nil {
		return err
	}
	service, err := newAdminService(snapshot.Config, configDir, nil, passwords)
	if err != nil {
		return err
	}
	users, err := service.ListUsers(context.Background())
	if err != nil {
		return err
	}
	fmt.Println("username  enabled  identity")
	for _, user := range users {
		fmt.Printf("%-9s %-8t %s\n", user.Username, user.Enabled, user.Identity)
	}
	return nil
}

// readPassword reads one password from the TTY (without echo) or from stdin.
// It is the only path by which a password reaches the admin service, so a
// password can never appear in the process arguments.
func readPassword(prompt string) ([]byte, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, fmt.Errorf("read password: %w", err)
		}
		return password, nil
	}
	return readPasswordLine(os.Stdin)
}

// readPasswordLine reads one newline-terminated password from a byte stream.
// It is split out so the stdin path can be tested without a terminal.
func readPasswordLine(r io.Reader) ([]byte, error) {
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read password from stdin: %w", err)
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

// clearBytes overwrites a password buffer once it has been handed to the
// password store, so the plaintext does not linger.
func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// adminSessions dispatches session listing.
func adminSessions(args []string) error {
	if len(args) == 0 || isFlag(args[0]) {
		return fmt.Errorf("sessions needs an operation: list")
	}
	switch args[0] {
	case "list":
		return adminSessionsList(args[1:])
	default:
		return fmt.Errorf("unknown sessions operation %q (try: list)", args[0])
	}
}

// adminSessionsList lists sessions derived from the event log.
func adminSessionsList(args []string) error {
	fs := flag.NewFlagSet("admin sessions list", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	limit := fs.Int("limit", 50, "maximum sessions to list")
	cursor := fs.String("cursor", "", "opaque pagination cursor from a previous page")
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	storage, closeStorage, err := openAdminStorage(snapshot.Config)
	if err != nil {
		return err
	}
	defer closeStorage()
	service, err := newAdminService(snapshot.Config, configDir, storage, nil)
	if err != nil {
		return err
	}
	page, next, err := service.ListSessions(context.Background(), *limit, *cursor)
	if err != nil {
		return err
	}
	fmt.Println("session                            started    last       turns  interrupted  ended  recovered")
	for _, s := range page {
		fmt.Printf("%-34s %-10d %-10d %-6d %-12d %-6t %t\n",
			s.SessionID, s.StartedAtUnixMilli, s.LastActivityUnixMilli, s.TurnCount, s.InterruptedTurns, s.Ended, s.Recovered)
	}
	if next != "" {
		fmt.Printf("next cursor: %s\n", next)
	}
	return nil
}

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

// adminExport streams a research projection for a session, user, time range,
// or the whole experiment (PLAN 10.5).
func adminExport(args []string) error {
	fs := flag.NewFlagSet("admin export", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	format := fs.String("format", "jsonl", "export format: jsonl, transcript, or asciicast")
	out := fs.String("out", "", "output file or directory (required)")
	fromMs := fs.Int64("from-ms", 0, "include events at or after this unix-millisecond time")
	toMs := fs.Int64("to-ms", 0, "include events before this unix-millisecond time")
	redactSecrets := fs.Bool("redact-secrets", true, "replace known secret shapes in the export")
	sessionIDs := &stringList{}
	userIDs := &stringList{}
	fs.Var(sessionIDs, "session", "session ID to export (repeatable)")
	fs.Var(userIDs, "user", "user ID to export (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*out) == "" {
		return errors.New("-out is required")
	}

	query := domain.ExportQuery{
		Format:     *format,
		OutputPath: *out,
		Filter:     domain.RetrievalFilter{FromTime: *fromMs, ToTime: *toMs},
		Redact:     domain.RedactionPolicy{RedactSecrets: *redactSecrets, ReplacementText: "[REDACTED]"},
	}
	scope := domain.RetrievalScope{}
	for _, id := range sessionIDs.values {
		parsed, err := domain.ParseSessionID(id)
		if err != nil {
			return fmt.Errorf("parse session ID %q: %w", id, err)
		}
		scope.SessionIDs = append(scope.SessionIDs, parsed)
		scope.Scopes = append(scope.Scopes, domain.ScopeSession)
	}
	for _, id := range userIDs.values {
		parsed, err := domain.ParseUserID(id)
		if err != nil {
			return fmt.Errorf("parse user ID %q: %w", id, err)
		}
		scope.UserIDs = append(scope.UserIDs, parsed)
		scope.Scopes = append(scope.Scopes, domain.ScopeUser)
	}
	if len(scope.Scopes) == 0 {
		// A whole-experiment export spans every scope the admin policy permits.
		scope.Scopes = []domain.Scope{domain.ScopeSession, domain.ScopeUser, domain.ScopeShared, domain.ScopeBaseline}
		scope.IncludeShared = true
	}
	query.Scope = scope

	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	storage, closeStorage, err := openAdminStorage(snapshot.Config)
	if err != nil {
		return err
	}
	defer closeStorage()
	service, err := newAdminService(snapshot.Config, configDir, storage, nil)
	if err != nil {
		return err
	}
	result, err := service.Export(context.Background(), query)
	if err != nil {
		return err
	}
	printExportResult(result)
	return nil
}

// ---------------------------------------------------------------------------
// status (model/account health)
// ---------------------------------------------------------------------------

// adminStatus reports configured accounts and the current route/account health
// records. Health is durable, so this exec process reports the records the
// running service maintains rather than per-process state.
func adminStatus(args []string) error {
	fs := flag.NewFlagSet("admin status", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	storage, closeStorage, err := openAdminStorage(snapshot.Config)
	if err != nil {
		return err
	}
	defer closeStorage()
	service, err := newAdminService(snapshot.Config, configDir, storage, nil)
	if err != nil {
		return err
	}
	cfg := snapshot.Config
	fmt.Printf("accounts: %d configured\n", len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		fmt.Printf("  %s  quota_group=%s  products=%s\n", account.ID, account.QuotaGroup, strings.Join(account.PermittedProducts, ","))
	}
	fmt.Printf("routes:   %d configured\n", len(cfg.Routes))
	for _, route := range cfg.Routes {
		fmt.Printf("  %s  provider=%s  product=%s  model=%s\n", route.ID, route.Provider, route.Product, route.Model)
	}
	records, err := service.RouteStatus(context.Background())
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Println("health:   no route/account health records recorded")
		return nil
	}
	fmt.Println("health:")
	for _, rec := range records {
		fmt.Printf("  %s  state=%s  failures=%d\n", healthKeyString(rec), rec.State, rec.ConsecutiveFailures)
	}
	return nil
}

// healthKeyString renders a health record's scope key for display.
func healthKeyString(rec domain.HealthRecord) string {
	key := domain.HealthKey{RouteID: rec.RouteID, Scope: rec.Scope}
	if rec.AccountID != nil {
		key.AccountID = *rec.AccountID
	}
	return key.String()
}

// ---------------------------------------------------------------------------
// probe
// ---------------------------------------------------------------------------

// adminProbe dispatches bounded probe control.
func adminProbe(args []string) error {
	if len(args) == 0 || isFlag(args[0]) {
		return fmt.Errorf("probe needs an operation: trigger, run")
	}
	switch args[0] {
	case "trigger":
		return adminProbeTrigger(args[1:])
	case "run":
		// This build wires no probe runner into the admin service (the router
		// owns probe scheduling), so running a batch reports the unavailable
		// dependency rather than silently doing nothing.
		return runAdminProbe(args[1:])
	default:
		return fmt.Errorf("unknown probe operation %q (try: trigger, run)", args[0])
	}
}

// adminProbeTrigger admits exactly one bounded probe for a due health key. The
// admitted key is written to the durable health store, so the running service
// picks it up on its next due-probe scan.
func adminProbeTrigger(args []string) error {
	fs := flag.NewFlagSet("admin probe trigger", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	route := fs.String("route", "", "route ID (required)")
	account := fs.String("account", "", "account ID (optional; empty for a route-scoped record)")
	scope := fs.String("scope", "route", "health scope: credential, account, route, product, or provider")
	if err := fs.Parse(args); err != nil {
		return err
	}
	routeID, err := domain.ParseRouteID(*route)
	if err != nil {
		return fmt.Errorf("parse route ID: %w", err)
	}
	key := domain.HealthKey{RouteID: routeID, Scope: *scope}
	if *account != "" {
		accountID, err := domain.ParseAccountID(*account)
		if err != nil {
			return fmt.Errorf("parse account ID: %w", err)
		}
		key.AccountID = accountID
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	storage, closeStorage, err := openAdminStorage(snapshot.Config)
	if err != nil {
		return err
	}
	defer closeStorage()
	service, err := newAdminService(snapshot.Config, configDir, storage, nil)
	if err != nil {
		return err
	}
	if err := service.TriggerProbe(context.Background(), key); err != nil {
		return err
	}
	fmt.Printf("probe admitted for %s\n", key)
	return nil
}

// runAdminProbe reports the probe runner as unavailable: the router owns probe
// execution and is not reachable from the admin exec surface.
func runAdminProbe(args []string) error {
	fs := flag.NewFlagSet("admin probe run", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	service, err := newAdminService(snapshot.Config, configDir, nil, nil)
	if err != nil {
		return err
	}
	ran, err := service.RunDueProbes(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("%d probes ran\n", ran)
	return nil
}

// ---------------------------------------------------------------------------
// integrity
// ---------------------------------------------------------------------------

// adminIntegrity checks the live database and prints its SQLite integrity
// answer, foreign-key violations, schema version, and fingerprint.
func adminIntegrity(args []string) error {
	fs := flag.NewFlagSet("admin integrity", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	storage, closeStorage, err := openAdminStorage(snapshot.Config)
	if err != nil {
		return err
	}
	defer closeStorage()
	service, err := newAdminService(snapshot.Config, configDir, storage, nil)
	if err != nil {
		return err
	}
	report, err := service.IntegrityCheck(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("integrity:     %s\n", report.IntegrityCheck)
	fmt.Printf("foreign_keys:  %d violation(s)\n", report.ForeignKeyViolations)
	fmt.Printf("schema:        v%d\n", report.SchemaVersion)
	fmt.Printf("fingerprint:   %s\n", report.LogicalFingerprint)
	for table, count := range report.RowCounts {
		fmt.Printf("rows[%s]: %d\n", table, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// backup and restore
// ---------------------------------------------------------------------------

// adminBackup captures a consistent snapshot into a directory.
func adminBackup(args []string) error {
	fs := flag.NewFlagSet("admin backup", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	dest := fs.String("dest", "", "destination directory for the backup (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dest) == "" {
		return errors.New("-dest is required")
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	storage, closeStorage, err := openAdminStorage(snapshot.Config)
	if err != nil {
		return err
	}
	defer closeStorage()
	service, err := newAdminService(snapshot.Config, configDir, storage, nil)
	if err != nil {
		return err
	}
	report, err := service.CreateBackup(context.Background(), admin.BackupRequest{DestDir: *dest})
	if err != nil {
		return err
	}
	fmt.Printf("backup written to %s\n", report.DestDir)
	fmt.Printf("database:    %s (%d bytes, %d events, schema v%d)\n",
		report.DatabaseFile, report.DatabaseBytes, report.EventRows, report.SchemaVersion)
	fmt.Printf("fingerprint: %s\n", report.LogicalFingerprint)
	for _, extra := range report.ExtraFiles {
		fmt.Printf("extra[%s]: sha256=%s size=%d\n", extra.Name, extra.SHA256, extra.Size)
	}
	return nil
}

// adminRestore rebuilds a database from a verified backup into fresh paths.
func adminRestore(args []string) error {
	fs := flag.NewFlagSet("admin restore", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	source := fs.String("source", "", "backup directory to restore from (required)")
	destDB := fs.String("dest-db", "", "path for the rebuilt database (required; must not be the live database)")
	destExtras := fs.String("dest-extras", "", "directory for restored sidecar files (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*source) == "" {
		return errors.New("-source is required")
	}
	if strings.TrimSpace(*destDB) == "" {
		return errors.New("-dest-db is required")
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	storage, closeStorage, err := openAdminStorage(snapshot.Config)
	if err != nil {
		return err
	}
	defer closeStorage()
	service, err := newAdminService(snapshot.Config, configDir, storage, nil)
	if err != nil {
		return err
	}
	report, err := service.RestoreBackup(context.Background(), admin.RestoreRequest{
		SourceDir:     *source,
		DestDatabase:  *destDB,
		DestExtrasDir: *destExtras,
	})
	if err != nil {
		return err
	}
	fmt.Printf("database rebuilt at %s (%d bytes, schema v%d)\n", report.DatabaseFile, report.DatabaseBytes, report.SchemaVersion)
	fmt.Printf("fingerprint: %s\n", report.LogicalFingerprint)
	for _, extra := range report.ExtraFiles {
		fmt.Printf("extra[%s]: sha256=%s size=%d\n", extra.Name, extra.SHA256, extra.Size)
	}
	return nil
}

// ---------------------------------------------------------------------------
// app rollback
// ---------------------------------------------------------------------------

// adminApp dispatches application administration.
func adminApp(args []string) error {
	if len(args) == 0 || isFlag(args[0]) {
		return fmt.Errorf("app needs an operation: rollback")
	}
	switch args[0] {
	case "rollback":
		return adminAppRollback(args[1:])
	default:
		return fmt.Errorf("unknown app operation %q (try: rollback)", args[0])
	}
}

// adminAppRollback restores an application's active pointer to a prior
// accepted version. This build wires no app registry into the admin surface,
// so the operation reports the unavailable dependency rather than silently
// succeeding.
func adminAppRollback(args []string) error {
	fs := flag.NewFlagSet("admin app rollback", flag.ContinueOnError)
	configPath, dirs := configFlagsFor(fs, defaultConfigPath)
	app := fs.String("app", "", "application ID (required)")
	version := fs.String("version", "", "target app version ID (required)")
	reason := fs.String("reason", "", "operator reason recorded with the rollback")
	if err := fs.Parse(args); err != nil {
		return err
	}
	appID, err := domain.ParseAppID(*app)
	if err != nil {
		return fmt.Errorf("parse app ID: %w", err)
	}
	versionID, err := domain.ParseAppVersionID(*version)
	if err != nil {
		return fmt.Errorf("parse app version ID: %w", err)
	}
	snapshot, configDir, err := adminSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	service, err := newAdminService(snapshot.Config, configDir, nil, nil)
	if err != nil {
		return err
	}
	if err := service.RollbackApp(context.Background(), appID, versionID, *reason); err != nil {
		return err
	}
	fmt.Printf("application %s rolled back to version %s\n", appID, versionID)
	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// printExportResult renders an export outcome in a stable operator layout.
func printExportResult(result domain.ExportResult) {
	fmt.Printf("export %s complete: %d event(s), %d bytes, format %s\n",
		result.ExportID, result.EventCount, result.SizeBytes, result.Format)
	fmt.Printf("checksum: %s\n", result.Checksum)
}

// stringList collects a repeatable string flag.
type stringList struct{ values []string }

func (l *stringList) String() string { return strings.Join(l.values, ",") }

func (l *stringList) Set(value string) error {
	l.values = append(l.values, value)
	return nil
}
