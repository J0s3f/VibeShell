package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// validConfig returns a configuration that satisfies every
// rule this spike validates, mirroring the shipped example.
func validConfig() Config {
	return Config{
		Version:  ConfigurationVersion,
		Identity: IdentityConfig{SystemName: SystemNameConst, ShellName: ShellNameConst, Hostname: "vibeshell.example"},
		SSH:      SSHConfig{ListenPort: 2222, HostKeyFile: "/etc/vibeshell/host_key", MaxConnections: 512, IdleTimeoutMs: 300000},
		Auth:     AuthConfig{Mode: AuthModeSecure, PasswordFile: "/etc/vibeshell/passwords.argon2", MaxAttemptsPerConn: 5},
		Sharing:  SharingConfig{Enabled: boolPtr(true), PolicyRevision: 3},
		Tiers: []TierConfig{
			{Name: "named-free", Routes: []string{"opencode/ling-3.1-flash-free", "opencode/deepseek-v4.1-flash-free", "opencode/qwen3.6-flash-free"}},
			{Name: "free-discovery", AutoFree: true, Routes: []string{"opencode/-free"}},
			{Name: "go-native", Routes: []string{"opencode-go/ling-3.1-flash", "opencode-go/deepseek-v4.1-flash"}},
			{Name: "cheap-payg", Routes: []string{"zen/gpt-5.1-nano", "zen/minimax-m3-turbo"}, AccountPool: "payg-accounts"},
		},
		Accounts: []AccountConfig{
			{ID: "opencode-primary", QuotaGroup: "team-a", PermittedProducts: []string{"opencode", "opencode-go"}, SecretRef: "{file:accounts/opencode-primary.key}"},
			{ID: "zen-payg", QuotaGroup: "payg", PermittedProducts: []string{"zen"}, SecretRef: "{env:VIBESHELL_ZEN_API_KEY_FAKE}"},
		},
		Limits:      LimitsConfig{TurnDeadlineMs: 120000, MaxAttempts: 4, MaxOutputBytes: 1048576, MaxContentBytes: 4194304},
		Persistence: PersistenceConfig{DatabasePath: "/var/lib/vibeshell/world.db", Durability: DurabilityFull, BackupDir: "/var/backups/vibeshell"},
		Prompts: &PromptsConfig{
			MOTD: "prompts/motd.md", Shell: "prompts/shell.md", AppGenerate: "prompts/app_generate.md", AppExtend: "prompts/app_extend.md",
			World: "prompts/world.md", Summary: "prompts/summary.md", Repair: "prompts/repair.md",
		},
		Providers: &ProvidersConfig{
			OpenCode:   ProviderConfig{Enabled: boolPtr(true), Endpoint: "https://opencode.example.internal", Protocols: []string{"opencode"}},
			OpenCodeGo: ProviderConfig{Enabled: boolPtr(true), Endpoint: "https://opencode-go.example.internal", Protocols: []string{"opencode-go"}},
			Zen:        ProviderConfig{Enabled: boolPtr(true), Endpoint: "https://zen.example.internal", Protocols: []string{"zen"}},
		},
		Discovery:  &DiscoveryConfig{RefreshMs: 60000, StaleAfterMs: 600000, MetadataSource: "https://catalogue.example.internal/routes.json"},
		Inference:  &InferenceConfig{RequestTimeoutMs: 30000, MaxTokensPerTurn: 65536, MaxConcurrent: 64, QueueDepth: 256},
		Operations: &OperationsConfig{LogLevel: "info", ShutdownGraceMs: 5000},
	}
}

func TestParseConfigAcceptsTheExampleDocument(t *testing.T) {
	data, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	if cfg.Version != 1 || cfg.Identity.SystemName != "VibeOS" || cfg.Auth.Mode != "secure" {
		t.Fatalf("decoded fields wrong: %+v", cfg)
	}
	if cfg.Prompts == nil || cfg.Discovery == nil || cfg.Inference == nil || cfg.Operations == nil || cfg.Providers == nil {
		t.Fatalf("extension group presence wrong: providers=%v prompts=%v discovery=%v inference=%v operations=%v",
			cfg.Providers, cfg.Prompts, cfg.Discovery, cfg.Inference, cfg.Operations)
	}
}

func TestParseConfigRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"unknown top-level field", `{"version":1,"surprise":true}`, `unknown field "surprise"`},
		{"unknown nested field", `{"version":1,"auth":{"mode":"secure","surprise":true}}`, `unknown field "surprise"`},
		{"unknown deeply nested field", `{"version":1,"providers":{"zen":{"enabled":true,"protocols":["zen"],"surprise":1}}}`, `unknown field "surprise"`},
		{"wrong type for version", `{"version":"1"}`, "cannot unmarshal string"},
		{"wrong type for listen_port", `{"version":1,"ssh":{"listen_port":"2222"}}`, "cannot unmarshal string"},
		{"wrong type for tiers", `{"version":1,"tiers":"named-free"}`, "cannot unmarshal string"},
		{"wrong type for sharing.enabled", `{"version":1,"sharing":{"enabled":"yes"}}`, "cannot unmarshal string"},
		{"wrong type for a route entry", `{"version":1,"tiers":[{"name":"a","routes":["x",5]}]}`, "cannot unmarshal number"},
		{"trailing content", `{"version":1} "extra"`, "unexpected content after the configuration object"},
		{"trailing garbage", `{"version":1} garbage`, "malformed JSON"},
		{"array root", `[]`, "configuration root must be a JSON object, got an array"},
		{"scalar root", `42`, "configuration root must be a JSON object"},
		{"empty document", ``, "configuration document is empty"},
		{"malformed JSON", `{"version":`, "malformed JSON"},
		{"unmatched closing delimiter", `{"version":1}}`, "malformed JSON"},
		{"duplicate top-level key", `{"version":1,"version":2}`, `duplicate key "version" at $`},
		{"duplicate identity key", `{"version":1,"identity":{"hostname":"a"},"identity":{"hostname":"b"}}`, `duplicate key "identity" at $`},
		{"duplicate nested key", `{"version":1,"auth":{"mode":"secure","mode":"public"}}`, `duplicate key "mode" at $.auth`},
		{"duplicate key in a tier", `{"version":1,"tiers":[{"name":"a","name":"b","routes":["x"]}]}`, `duplicate key "name" at $.tiers[0]`},
		{"duplicate account key", `{"version":1,"accounts":[{"id":"a","id":"b","quota_group":"g","secret_ref":"{env:X}"}]}`, `duplicate key "id" at $.accounts[0]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(test.doc))
			if err == nil {
				t.Fatalf("document accepted: %s", test.doc)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), test.want)
			}
		})
	}
}

// removeJSONKey deletes one key from a JSON document so a
// required-field test can start from a valid document.
func removeJSONKey(t *testing.T, doc string, keys ...string) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	current := root
	for _, key := range keys[:len(keys)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("fixture: key path %v is not nested objects", keys)
		}
		current = next
	}
	delete(current, keys[len(keys)-1])
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return string(out)
}

func TestParseConfigRequiresDocumentedFields(t *testing.T) {
	full, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		keys  []string
		wants []string
	}{
		{"missing version", []string{"version"}, []string{"required field $.version is missing"}},
		{"missing identity", []string{"identity"}, []string{"$.identity.system_name", "$.identity.shell_name", "$.identity.hostname"}},
		{"missing ssh", []string{"ssh"}, []string{"$.ssh.listen_port", "$.ssh.host_key_file"}},
		{"missing auth mode", []string{"auth", "mode"}, []string{"required field $.auth.mode is missing"}},
		{"missing sharing enabled", []string{"sharing", "enabled"}, []string{"required field $.sharing.enabled is missing"}},
		{"missing tiers", []string{"tiers"}, []string{"required field $.tiers is missing"}},
		{"missing persistence database path", []string{"persistence", "database_path"}, []string{"required field $.persistence.database_path is missing"}},
		{"missing prompts group field", []string{"prompts", "motd"}, []string{"required field $.prompts.motd is missing"}},
		{"missing provider enabled", []string{"providers", "zen", "enabled"}, []string{"required field $.providers.zen.enabled is missing"}},
		{"missing discovery refresh", []string{"discovery", "refresh_ms"}, []string{"required field $.discovery.refresh_ms is missing"}},
		{"missing inference bounds", []string{"inference", "max_concurrent"}, []string{"required field $.inference.max_concurrent is missing"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := removeJSONKey(t, string(full), test.keys...)
			_, err := ParseConfig([]byte(doc))
			if err == nil {
				t.Fatalf("document with missing %v accepted", test.keys)
			}
			for _, want := range test.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestValidateAcceptsTheExampleConfiguration(t *testing.T) {
	if err := Validate(validConfig(), ExampleCatalogue()); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
}

// A contract version 1 document carries no providers group;
// route-to-protocol checks are skipped for it because
// nothing declares protocols to check against.
func TestValidateAcceptsContractVersionOneDocument(t *testing.T) {
	cfg := validConfig()
	cfg.Providers = nil
	cfg.Discovery = nil
	cfg.Inference = nil
	cfg.Operations = nil
	if err := Validate(cfg, ExampleCatalogue()); err != nil {
		t.Fatalf("version 1 configuration rejected: %v", err)
	}
}

func TestValidateRejectsSemanticViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"wrong system name", func(c *Config) { c.Identity.SystemName = "VibeOS2" }, `identity.system_name must be "VibeOS"`},
		{"wrong shell name", func(c *Config) { c.Identity.ShellName = "VibeShell2" }, `identity.shell_name must be "VibeShell"`},
		{"hostname too long", func(c *Config) { c.Identity.Hostname = strings.Repeat("h", MaxHostnameLen+1) }, "identity.hostname must be at most 64 characters"},
		{"listen port too low", func(c *Config) { c.SSH.ListenPort = 0 }, "ssh.listen_port must be in [1, 65535]"},
		{"listen port too high", func(c *Config) { c.SSH.ListenPort = 65536 }, "ssh.listen_port must be in [1, 65535]"},
		{"too many connections", func(c *Config) { c.SSH.MaxConnections = 10001 }, "ssh.max_connections must be at most 10000"},
		{"unknown auth mode", func(c *Config) { c.Auth.Mode = "insecure" }, `auth.mode must be "public" or "secure"`},
		{"public mode with password file", func(c *Config) { c.Auth.Mode = AuthModePublic }, `auth.mode "public" must not set auth.password_file`},
		{"secure mode without password file", func(c *Config) { c.Auth.PasswordFile = "" }, `auth.mode "secure" requires auth.password_file`},
		{"too many auth attempts", func(c *Config) { c.Auth.MaxAttemptsPerConn = 11 }, "auth.max_attempts_per_connection must be at most 10"},
		{"sharing without policy revision", func(c *Config) { c.Sharing.PolicyRevision = 0 }, "sharing.enabled requires sharing.policy_revision >= 1"},
		{"duplicate account ID", func(c *Config) { c.Accounts = append(slices.Clone(c.Accounts), c.Accounts[0]) }, `duplicate account ID "opencode-primary"`},
		{"duplicate tier name", func(c *Config) { c.Tiers[1].Name = "named-free" }, `duplicate tier name "named-free"`},
		{"discovery tier first", func(c *Config) { c.Tiers[0], c.Tiers[1] = c.Tiers[1], c.Tiers[0] }, "must not be the first tier"},
		{"two discovery tiers", func(c *Config) {
			c.Tiers[3] = TierConfig{Name: "more-free", AutoFree: true, Routes: []string{"opencode/-free"}}
		}, "at most one tier may use auto_free discovery, found 2"},
		{"discovery tier with account pool", func(c *Config) { c.Tiers[1].AccountPool = "pool" }, "the discovery tier (auto_free) must not set account_pool"},
		{"unknown model route", func(c *Config) { c.Tiers[0].Routes = []string{"opencode/no-such-route"} }, `unknown model route "opencode/no-such-route"`},
		{"discovery pattern in a plain tier", func(c *Config) { c.Tiers[0].Routes = []string{"opencode/-free"} }, `route "opencode/-free" is a discovery pattern`},
		{"discovery pattern matches nothing", func(c *Config) { c.Tiers[1].Routes = []string{"zen/-free"} }, `discovery pattern "zen/-free" matches no routes`},
		{"explicit route in a discovery tier", func(c *Config) {
			c.Tiers[1].Routes = []string{"opencode/-free", "opencode/ling-3.1-flash-free"}
		}, `explicit route "opencode/ling-3.1-flash-free" in an auto_free discovery tier`},
		{"tier name too long", func(c *Config) { c.Tiers[0].Name = strings.Repeat("n", MaxTierNameLen+1) }, "name must be at most 64 characters"},
		{"too many tiers", func(c *Config) {
			c.Tiers = make([]TierConfig, MaxTiers+1)
			for i := range c.Tiers {
				c.Tiers[i] = TierConfig{Name: fmt.Sprintf("tier-%d", i), Routes: []string{"opencode/ling-3.1-flash-free"}}
			}
		}, "tiers must list at most 8 entries"},
		{"too many routes in a tier", func(c *Config) {
			c.Tiers[0].Routes = make([]string, MaxRoutesPerTier+1)
			for i := range c.Tiers[0].Routes {
				c.Tiers[0].Routes[i] = "opencode/ling-3.1-flash-free"
			}
		}, "at most 64 routes"},
		{"disabled provider breaks protocol support", func(c *Config) { *c.Providers.Zen.Enabled = false }, `requires protocol "zen", which no enabled provider supports`},
		{"provider protocol mismatch", func(c *Config) { c.Providers.OpenCode.Protocols = []string{"bogus"} }, `requires protocol "opencode", which no enabled provider supports`},
		{"provider missing enabled", func(c *Config) { c.Providers.Zen.Enabled = nil }, "required field $.providers.zen.enabled is missing"},
		{"enabled provider without endpoint", func(c *Config) { c.Providers.Zen.Endpoint = "" }, "an enabled provider requires $.providers.zen.endpoint"},
		{"provider without protocols", func(c *Config) { c.Providers.Zen.Protocols = nil }, "at least one protocol is required in $.providers.zen.protocols"},
		{"persistence full without backup dir", func(c *Config) { c.Persistence.BackupDir = "" }, "persistence.durability FULL (the default) requires persistence.backup_dir"},
		{"invalid durability", func(c *Config) { c.Persistence.Durability = "FAST" }, `persistence.durability must be "FULL" or "NORMAL"`},
		{"negative turn deadline", func(c *Config) { c.Limits.TurnDeadlineMs = -1 }, "limits.turn_deadline_ms must be at least 1 ms"},
		{"too many attempts", func(c *Config) { c.Limits.MaxAttempts = MaxInferenceAttempts + 1 }, "limits.max_attempts must be at most 32"},
		{"negative output bytes", func(c *Config) { c.Limits.MaxOutputBytes = -1 }, "limits.max_output_bytes must be at least 1"},
		{"discovery refresh too fast", func(c *Config) { c.Discovery.RefreshMs = 500 }, "discovery.refresh_ms must be at least 1000 ms"},
		{"stale age below refresh", func(c *Config) { c.Discovery.StaleAfterMs = c.Discovery.RefreshMs - 1 }, "discovery.stale_after_ms"},
		{"inference timeout out of range", func(c *Config) { c.Inference.RequestTimeoutMs = MaxInferenceTimeoutMs + 1 }, "inference.request_timeout_ms must be in [1, 600000]"},
		{"inference token limit out of range", func(c *Config) { c.Inference.MaxTokensPerTurn = MaxTokensPerTurn + 1 }, "inference.max_tokens_per_turn must be in [1, 1048576]"},
		{"inference concurrency out of range", func(c *Config) { c.Inference.MaxConcurrent = MaxInferenceConcurrent + 1 }, "inference.max_concurrent must be in [1, 1024]"},
		{"inference queue depth out of range", func(c *Config) { c.Inference.QueueDepth = MaxInferenceQueueDepth + 1 }, "inference.queue_depth must be in [0, 1024]"},
		{"unknown log level", func(c *Config) { c.Operations.LogLevel = "verbose" }, "operations.log_level must be one of debug, info, warn, error"},
		{"shutdown grace out of range", func(c *Config) { c.Operations.ShutdownGraceMs = MaxShutdownGraceMs + 1 }, "operations.shutdown_grace_ms must be in [0, 60000]"},
		{"inline secret value", func(c *Config) { c.Accounts[0].SecretRef = "sk-12345" }, "inline secret values are not allowed"},
		{"malformed secret reference", func(c *Config) { c.Accounts[0].SecretRef = "{file}" }, `must be {file:...} or {env:...}`},
		{"empty secret reference target", func(c *Config) { c.Accounts[0].SecretRef = "{env:}" }, "empty env target"},
		{"dot target secret reference", func(c *Config) { c.Accounts[0].SecretRef = "{file:..}" }, "outside the allowed directory"},
		{"unknown secret reference kind", func(c *Config) { c.Accounts[0].SecretRef = "{token:abc}" }, `must be {file:...} or {env:...}`},
		{"empty permitted product", func(c *Config) { c.Accounts[0].PermittedProducts = []string{"opencode", ""} }, "permitted_products must not contain empty entries"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.mutate(&cfg)
			err := Validate(cfg, ExampleCatalogue())
			if err == nil {
				t.Fatalf("violation accepted")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), test.want)
			}
		})
	}
}

// writePromptFiles writes the seven prompt files a
// loaded configuration references, relative to dir.
func writePromptFiles(t *testing.T, dir string) {
	t.Helper()
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"motd", "shell", "app_generate", "app_extend", "world", "summary", "repair"} {
		if err := os.WriteFile(filepath.Join(promptsDir, name+".md"), []byte("example prompt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeConfigFile marshals a configuration for loader
// and store tests.
func writeConfigFile(t *testing.T, path string, cfg Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// newTestStore writes one valid configuration, its secrets,
// and the environment a default resolver needs, then
// returns a store whose first load has already published.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(filepath.Join(secretsDir, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "accounts", "opencode-primary.key"), []byte("FAKE-PRIMARY-KEY-0001"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "accounts", "rotated.key"), []byte("FAKE-PRIMARY-KEY-0002"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ENV_SECRET", "FAKE-ENV-SECRET-0002")
	t.Setenv("VIBESHELL_ZEN_API_KEY_FAKE", "FAKE-ZEN-KEY-0002")
	writePromptFiles(t, dir)

	loader := Loader{SecretsDir: secretsDir, Catalogue: ExampleCatalogue()}
	store := NewStore(loader)
	first := validConfig()
	writeConfigFile(t, filepath.Join(dir, "v1.json"), first)
	if _, err := store.LoadFile(filepath.Join(dir, "v1.json")); err != nil {
		t.Fatal(err)
	}
	return store, dir
}
