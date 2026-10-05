package config

// SchemaVersion is the only configuration document version this build
// accepts. The schema documents it as "version": 1.
const SchemaVersion = 1

// Options carries the purely syntactic constraints a parse needs. Secret
// directory enforcement happens during validation (no I/O), while reading
// the values is the SecretResolver's job.
type Options struct {
	// SecretDirs are the only directories file-backed secret references may
	// resolve to. Paths are container paths, so they use POSIX rules. An
	// empty list rejects every file-backed reference: secret locations are
	// fail-closed, never "wherever the operator pointed".
	SecretDirs []string
}

// Protocol names the wire protocols an OpenCode product endpoint accepts.
// The set matches the codecs qualified in A06 (docs/research/2026-10-03-
// opencode-protocol-spike.md).
type Protocol string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolResponses Protocol = "responses"
	ProtocolMessages  Protocol = "messages"
	ProtocolGemini    Protocol = "gemini"
)

// AuthMode is the required explicit authentication mode (PLAN 11): public
// accepts any username without a password, secure requires the password
// file. There is deliberately no default.
type AuthMode string

const (
	AuthModePublic AuthMode = "public"
	AuthModeSecure AuthMode = "secure"
)

// Config is the versioned strict JSON service configuration (PLAN 11).
// Required groups are plain pointers so an absent required group is
// distinguishable from an empty one and cannot be dereferenced by
// validation; optional groups are nil when the operator left them out and
// then inherit documented defaults elsewhere.
type Config struct {
	Version int `json:"version"`

	Identity     *Identity           `json:"identity,omitempty"`
	SSH          *SSH                `json:"ssh,omitempty"`
	Auth         *Auth               `json:"auth,omitempty"`
	Sharing      *Sharing            `json:"sharing,omitempty"`
	World        *World              `json:"world,omitempty"`
	Apps         *Apps               `json:"apps,omitempty"`
	Terminal     *Terminal           `json:"terminal,omitempty"`
	Prompts      *Prompts            `json:"prompts,omitempty"`
	Providers    []Provider          `json:"providers,omitempty"`
	Routes       []Route             `json:"routes,omitempty"`
	Accounts     []Account           `json:"accounts,omitempty"`
	AccountPools map[string][]string `json:"account_pools,omitempty"`
	Tiers        []Tier              `json:"tiers,omitempty"`
	Discovery    *Discovery          `json:"discovery,omitempty"`
	Health       *Health             `json:"health,omitempty"`
	Limits       *Limits             `json:"limits,omitempty"`
	Inference    *Inference          `json:"inference,omitempty"`
	Spending     *Spending           `json:"spending,omitempty"`
	Persistence  *Persistence        `json:"persistence,omitempty"`
	Exports      *Exports            `json:"exports,omitempty"`
	Operations   *Operations         `json:"operations,omitempty"`
}

// Identity names the simulated system and host (PLAN 11).
type Identity struct {
	SystemName       string `json:"system_name"`
	ShellName        string `json:"shell_name"`
	Hostname         string `json:"hostname"`
	PresentationSeed int64  `json:"presentation_seed,omitempty"`
}

// SSH configures the single public service port and terminal bounds (PLAN 4.3, 11).
type SSH struct {
	ListenAddress      string `json:"listen_address,omitempty"`
	ListenPort         int    `json:"listen_port"`
	HostKeyFile        string `json:"host_key_file"`
	MaxConnections     int    `json:"max_connections,omitempty"`
	IdleTimeoutMs      int    `json:"idle_timeout_ms,omitempty"`
	HandshakeTimeoutMs int    `json:"handshake_timeout_ms,omitempty"`
	MaxTerminalRows    int    `json:"max_terminal_rows,omitempty"`
	MaxTerminalCols    int    `json:"max_terminal_cols,omitempty"`
	MaxInputBytes      int    `json:"max_input_bytes,omitempty"`
}

// Auth selects the explicit authentication mode and its limits (PLAN 11).
type Auth struct {
	Mode                     AuthMode `json:"mode"`
	PasswordFile             string   `json:"password_file,omitempty"`
	MaxAttemptsPerConnection int      `json:"max_attempts_per_connection,omitempty"`
	MaxConcurrentAuth        int      `json:"max_concurrent_auth,omitempty"`
	FailedAttemptsPerMinute  int      `json:"failed_attempts_per_minute,omitempty"`
}

// Sharing is the cross-user world/history/app access policy (PLAN 5.2, 11).
type Sharing struct {
	Enabled        bool `json:"enabled"`
	PolicyRevision int  `json:"policy_revision,omitempty"`
}

// World bounds world state and staged changes (PLAN 5, 11).
type World struct {
	BaselineVersion            string `json:"baseline_version,omitempty"`
	MaxObjects                 int    `json:"max_objects,omitempty"`
	MaxContentBytes            int    `json:"max_content_bytes,omitempty"`
	MaterializationBudgetBytes int    `json:"materialization_budget_bytes,omitempty"`
	MaxStagedChanges           int    `json:"max_staged_changes,omitempty"`
	ConflictRetries            int    `json:"conflict_retries,omitempty"`
}

// Apps bounds generated-application sources, state, and execution (PLAN 5.6, 11).
type Apps struct {
	EngineABIVersion    string `json:"engine_abi_version,omitempty"`
	MaxSourceBytes      int    `json:"max_source_bytes,omitempty"`
	MaxStateBytes       int    `json:"max_state_bytes,omitempty"`
	HeapLimitBytes      int    `json:"heap_limit_bytes,omitempty"`
	StackLimitBytes     int    `json:"stack_limit_bytes,omitempty"`
	ExecutionDeadlineMs int    `json:"execution_deadline_ms,omitempty"`
	InstancePoolSize    int    `json:"instance_pool_size,omitempty"`
	MaxGenerations      int    `json:"max_generations,omitempty"`
	MaxRepairs          int    `json:"max_repairs,omitempty"`
}

// Terminal configures rendering and input budgets (PLAN 6, 11).
type Terminal struct {
	ScrollbackLines    int    `json:"scrollback_lines,omitempty"`
	MaxPasteBytes      int    `json:"max_paste_bytes,omitempty"`
	RedrawPolicy       string `json:"redraw_policy,omitempty"`
	RefreshBudgetLines int    `json:"refresh_budget_lines,omitempty"`
	CompletionCache    string `json:"completion_cache,omitempty"`
}

// Prompts holds administrator-owned prompt file paths (PLAN 7.4, 11).
// Relative paths resolve against the directory of the configuration file.
type Prompts struct {
	Motd                 string `json:"motd"`
	ShellBehavior        string `json:"shell_behavior"`
	AppGeneration        string `json:"app_generation"`
	AppExtension         string `json:"app_extension,omitempty"`
	WorldMaterialization string `json:"world_materialization"`
	Summary              string `json:"summary"`
	Repair               string `json:"repair"`
}

// Provider declares one provider's products, supported protocols, and
// administrator-controlled endpoint overrides (PLAN 11).
type Provider struct {
	Name string `json:"name"`
	// Kind selects the provider implementation: "http" (default) uses the
	// direct provider API, "cli" runs the OpenCode CLI.
	Kind     string    `json:"kind,omitempty"`
	Products []Product `json:"products"`
}

// Product is one OpenCode product route (Console, Go) behind a base URL.
// EndpointOverrides maps a supported protocol to an administrator-approved
// endpoint URL, which is how a protocol/model routing exception (A06 open
// question 2) becomes explicit configuration instead of inference.
type Product struct {
	Name              string            `json:"name"`
	BaseURL           string            `json:"base_url"`
	Protocols         []Protocol        `json:"protocols"`
	DefaultProtocol   Protocol          `json:"default_protocol,omitempty"`
	EndpointOverrides map[string]string `json:"endpoint_overrides,omitempty"`
}

// Route names one concrete model route. Its ID is a domain.RouteID string
// (validated at load), and provider/product/protocol must agree with the
// provider declaration: an unsupported protocol/model combination is a
// validation error, not a runtime discovery.
type Route struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Product  string   `json:"product"`
	Model    string   `json:"model"`
	Protocol Protocol `json:"protocol,omitempty"`
	// Purposes restricts the route to named request purposes ("motd",
	// "generation"). An empty list serves every purpose.
	Purposes []string `json:"purposes,omitempty"`
}

// Account is a provider account with a quota group and a secret reference
// (PLAN 8.2, 11). ID is a domain.AccountID string; SecretRef carries a
// reference, never a secret value.
type Account struct {
	ID                string   `json:"id"`
	QuotaGroup        string   `json:"quota_group"`
	PermittedProducts []string `json:"permitted_products,omitempty"`
	SecretRef         string   `json:"secret_ref"`
}

// Tier is one ordered routing tier: explicit routes plus an optional
// suitable-free expansion and eligible account pool (PLAN 8.4, 11).
type Tier struct {
	Name        string   `json:"name"`
	Routes      []string `json:"routes"`
	AutoFree    bool     `json:"auto_free,omitempty"`
	AccountPool string   `json:"account_pool,omitempty"`
}

// Discovery configures catalogue refresh and capability requirements (PLAN 8.3, 11).
type Discovery struct {
	RefreshIntervalMs int                 `json:"refresh_interval_ms,omitempty"`
	StaleAgeMs        int64               `json:"stale_age_ms,omitempty"`
	MetadataURL       string              `json:"metadata_url,omitempty"`
	MinContextTokens  int                 `json:"min_context_tokens,omitempty"`
	Capabilities      []string            `json:"capabilities,omitempty"`
	ExplicitAllow     []string            `json:"explicit_allow,omitempty"`
	ProtocolOverrides map[string]Protocol `json:"protocol_overrides,omitempty"`
}

// Health configures retry/backoff/probe budgets (PLAN 9, 11). The probe
// fields map onto domain.ProbeBudget.
type Health struct {
	MaxConcurrentProbes int     `json:"max_concurrent_probes,omitempty"`
	ProbeIntervalMs     int64   `json:"probe_interval_ms,omitempty"`
	MaxProbesPerHour    int     `json:"max_probes_per_hour,omitempty"`
	InitialBackoffMs    int64   `json:"initial_backoff_ms,omitempty"`
	MaxBackoffMs        int64   `json:"max_backoff_ms,omitempty"`
	JitterRatio         float64 `json:"jitter_ratio,omitempty"`
	ResetGraceMs        int64   `json:"reset_grace_ms,omitempty"`
}

// Limits are the schema-baseline request limits shared by routing and
// inference (schemas/configuration.schema.json "limits").
type Limits struct {
	TurnDeadlineMs  int `json:"turn_deadline_ms,omitempty"`
	MaxAttempts     int `json:"max_attempts,omitempty"`
	MaxOutputBytes  int `json:"max_output_bytes,omitempty"`
	MaxContentBytes int `json:"max_content_bytes,omitempty"`
}

// Inference bounds one turn's model work (PLAN 11).
type Inference struct {
	RequestDeadlineMs     int `json:"request_deadline_ms,omitempty"`
	MaxSteps              int `json:"max_steps,omitempty"`
	MaxOutputTokens       int `json:"max_output_tokens,omitempty"`
	GlobalConcurrency     int `json:"global_concurrency,omitempty"`
	MaxAccountConcurrency int `json:"max_account_concurrency,omitempty"`
	WaitQueueDepth        int `json:"wait_queue_depth,omitempty"`
}

// Spending holds optional monetary limits (PLAN 9.3, 11). There is no
// shipped default ceiling: a zero/absent limit means "no limit configured".
type Spending struct {
	InstanceLimitUSD  float64 `json:"instance_limit_usd,omitempty"`
	AccountLimitUSD   float64 `json:"account_limit_usd,omitempty"`
	UserLimitUSD      float64 `json:"user_limit_usd,omitempty"`
	SessionLimitUSD   float64 `json:"session_limit_usd,omitempty"`
	UnknownCostPolicy string  `json:"unknown_cost_policy,omitempty"`
}

// Persistence configures the durable database, writer queues, and backups
// (PLAN 10, 11). DatabasePath changes require a restart.
type Persistence struct {
	DatabasePath       string `json:"database_path"`
	Durability         string `json:"durability,omitempty"`
	BackupDir          string `json:"backup_dir,omitempty"`
	WriterQueueDepth   int    `json:"writer_queue_depth,omitempty"`
	BackupIntervalMs   int64  `json:"backup_interval_ms,omitempty"`
	EventRetention     string `json:"event_retention,omitempty"`
	EventRetentionDays int    `json:"event_retention_days,omitempty"`
}

// Exports configures research exports (PLAN 10.5, 11).
type Exports struct {
	OutputDir         string   `json:"output_dir"`
	Formats           []string `json:"formats,omitempty"`
	MaxConcurrentJobs int      `json:"max_concurrent_jobs,omitempty"`
	MaxExportBytes    int      `json:"max_export_bytes,omitempty"`
	RedactSecrets     *bool    `json:"redact_secrets,omitempty"`
}

// Operations configures service logging, health checks, and shutdown (PLAN 12.3-12.4, 11).
type Operations struct {
	LogLevel              string `json:"log_level,omitempty"`
	HealthCheckIntervalMs int64  `json:"health_check_interval_ms,omitempty"`
	ShutdownGraceMs       int64  `json:"shutdown_grace_ms,omitempty"`
	Instrumentation       string `json:"instrumentation,omitempty"`
}

// promptFile pairs a Prompts field name with its configured file path so the
// loader can check every reference uniformly.
type promptFile struct {
	field string
	file  string
}

// promptFiles lists the prompt paths of p in document order. An empty path
// (the optional app_extension) is included so validation can require it only
// when the operator wrote the field.
func promptFiles(p *Prompts) []promptFile {
	return []promptFile{
		{field: "motd", file: p.Motd},
		{field: "shell_behavior", file: p.ShellBehavior},
		{field: "app_generation", file: p.AppGeneration},
		{field: "app_extension", file: p.AppExtension},
		{field: "world_materialization", file: p.WorldMaterialization},
		{field: "summary", file: p.Summary},
		{field: "repair", file: p.Repair},
	}
}
