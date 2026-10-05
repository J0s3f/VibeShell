// Package config qualifies strict configuration parsing, semantic
// validation, and secret references for PLAN 11. It is a
// self-contained qualification spike: standard library only, no
// dependency on the application module.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Bounds and constants. The values up to MaxInferenceAttempts mirror
// schemas/configuration.schema.json (contract version 1); the
// extension-group bounds are this spike's policy, named here so a
// check and its error message share one definition.
const (
	ConfigurationVersion = 1

	SystemNameConst = "VibeOS"
	ShellNameConst  = "VibeShell"

	MaxHostnameLen         = 64
	MinListenPort          = 1
	MaxListenPort          = 65535
	MaxSSHConnections      = 10000
	MaxAuthAttemptsPerConn = 10

	MaxTiers         = 8
	MaxTierNameLen   = 64
	MaxRoutesPerTier = 64

	MaxInferenceAttempts = 32

	MinDiscoveryRefreshMs = 1000

	MaxInferenceTimeoutMs  = 600000
	MaxTokensPerTurn       = 1048576
	MaxInferenceConcurrent = 1024
	MaxInferenceQueueDepth = 1024

	MaxShutdownGraceMs = 60000

	AuthModePublic = "public"
	AuthModeSecure = "secure"

	DurabilityFull   = "FULL"
	DurabilityNormal = "NORMAL"
)

// Config is the root of the strict configuration document. The
// groups from identity through persistence mirror contract version
// 1 (schemas/configuration.schema.json). providers, prompts,
// discovery, inference, and operations are PLAN 11 groups that are
// not part of contract version 1 yet; they are optional pointer
// groups so a version 1 document stays valid without them, and this
// spike can still qualify their validation.
type Config struct {
	Version     int               `json:"version" required:"true"`
	Identity    IdentityConfig    `json:"identity"`
	SSH         SSHConfig         `json:"ssh"`
	Auth        AuthConfig        `json:"auth"`
	Sharing     SharingConfig     `json:"sharing"`
	Tiers       []TierConfig      `json:"tiers" required:"true"`
	Accounts    []AccountConfig   `json:"accounts"`
	Limits      LimitsConfig      `json:"limits"`
	Persistence PersistenceConfig `json:"persistence"`

	Providers  *ProvidersConfig  `json:"providers"`
	Prompts    *PromptsConfig    `json:"prompts"`
	Discovery  *DiscoveryConfig  `json:"discovery"`
	Inference  *InferenceConfig  `json:"inference"`
	Operations *OperationsConfig `json:"operations"`
}

// IdentityConfig pins the simulated system identity. The schema
// fixes both names as constants; a document claiming anything else
// is not a VibeShell configuration.
type IdentityConfig struct {
	SystemName string `json:"system_name" required:"true"`
	ShellName  string `json:"shell_name" required:"true"`
	Hostname   string `json:"hostname" required:"true"`
}

// SSHConfig describes the simulated SSH listener.
type SSHConfig struct {
	ListenPort     int    `json:"listen_port" required:"true"`
	HostKeyFile    string `json:"host_key_file" required:"true"`
	MaxConnections int    `json:"max_connections"`
	IdleTimeoutMs  int    `json:"idle_timeout_ms"`
}

// AuthConfig carries the explicit authentication mode. The mode is
// required; "public" must not pair with a password file and
// "secure" must.
type AuthConfig struct {
	Mode               string `json:"mode" required:"true"`
	PasswordFile       string `json:"password_file"`
	MaxAttemptsPerConn int    `json:"max_attempts_per_connection"`
}

// SharingConfig toggles cross-user access. Enabled is a pointer so
// a missing field is distinguishable from an explicit false, which
// the schema requires.
type SharingConfig struct {
	Enabled        *bool `json:"enabled" required:"true"`
	PolicyRevision int   `json:"policy_revision"`
}

// TierConfig is one ordered model-route tier. A tier lists either
// exact route IDs or, when AutoFree is set, a single suitable-free
// discovery pattern.
type TierConfig struct {
	Name        string   `json:"name" required:"true"`
	Routes      []string `json:"routes" required:"true"`
	AutoFree    bool     `json:"auto_free"`
	AccountPool string   `json:"account_pool"`
}

// AccountConfig is one provider account. The secret travels as a
// reference ({file:...} or {env:...}); the value never appears in
// the document.
type AccountConfig struct {
	ID                string   `json:"id" required:"true"`
	QuotaGroup        string   `json:"quota_group" required:"true"`
	PermittedProducts []string `json:"permitted_products"`
	SecretRef         string   `json:"secret_ref" required:"true"`
}

// LimitsConfig bounds one turn. A zero field means the operator
// accepted the documented default; only explicitly set values are
// bounds-checked.
type LimitsConfig struct {
	TurnDeadlineMs  int `json:"turn_deadline_ms"`
	MaxAttempts     int `json:"max_attempts"`
	MaxOutputBytes  int `json:"max_output_bytes"`
	MaxContentBytes int `json:"max_content_bytes"`
}

// PersistenceConfig locates durable state. The schema defaults
// durability to FULL, so an absent value behaves as FULL.
type PersistenceConfig struct {
	DatabasePath string `json:"database_path" required:"true"`
	Durability   string `json:"durability"`
	BackupDir    string `json:"backup_dir"`
}

// ProvidersConfig declares the enabled provider transports. Only
// the named providers exist; an unknown provider name is an unknown
// field and fails strict decoding.
type ProvidersConfig struct {
	OpenCode   ProviderConfig `json:"opencode"`
	OpenCodeGo ProviderConfig `json:"opencode-go"`
	Zen        ProviderConfig `json:"zen"`
}

// ProviderConfig describes one provider endpoint. Enabled is a
// pointer for the same presence reason as SharingConfig.Enabled.
type ProviderConfig struct {
	Enabled   *bool    `json:"enabled" required:"true"`
	Endpoint  string   `json:"endpoint"`
	Protocols []string `json:"protocols"`
}

// PromptsConfig locates the prompt files the shell loads at startup.
// Paths resolve against the configuration file's directory.
type PromptsConfig struct {
	MOTD        string `json:"motd" required:"true"`
	Shell       string `json:"shell" required:"true"`
	AppGenerate string `json:"app_generate" required:"true"`
	AppExtend   string `json:"app_extend" required:"true"`
	World       string `json:"world" required:"true"`
	Summary     string `json:"summary" required:"true"`
	Repair      string `json:"repair" required:"true"`
}

// DiscoveryConfig configures suitable-free route discovery. The
// group is optional; when present, all three fields are required.
type DiscoveryConfig struct {
	RefreshMs      int    `json:"refresh_ms" required:"true"`
	StaleAfterMs   int    `json:"stale_after_ms" required:"true"`
	MetadataSource string `json:"metadata_source" required:"true"`
}

// InferenceConfig bounds inference requests.
type InferenceConfig struct {
	RequestTimeoutMs int `json:"request_timeout_ms" required:"true"`
	MaxTokensPerTurn int `json:"max_tokens_per_turn" required:"true"`
	MaxConcurrent    int `json:"max_concurrent" required:"true"`
	QueueDepth       int `json:"queue_depth" required:"true"`
}

// OperationsConfig configures service operations.
type OperationsConfig struct {
	LogLevel        string `json:"log_level"`
	ShutdownGraceMs int    `json:"shutdown_grace_ms"`
}

// ParseConfig strictly decodes a configuration document: the
// document must be a single JSON object, duplicate object keys are
// rejected, unknown fields are rejected at every nesting level,
// wrong types are rejected, and every required field must be
// present. Parsing performs no I/O and resolves no references.
func ParseConfig(data []byte) (Config, error) {
	var cfg Config
	if err := scanDuplicateKeys(data); err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("strict decode failed: %w", err)
	}
	if tok, err := dec.Token(); !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("strict decode failed: unexpected content after the configuration object (token %v, error %v)", tok, err)
	}
	if errs := missingRequired(reflect.ValueOf(cfg), "$"); len(errs) > 0 {
		return cfg, errors.Join(errs...)
	}
	return cfg, nil
}

// scanScope is one nesting level of the duplicate-key
// scanner: a JSON object tracking its seen keys, or a
// JSON array tracking its element count.
type scanScope struct {
	name      string // path segment of this container, e.g. $.auth
	isObject  bool
	keys      map[string]bool
	expectKey bool
	pending   string // key whose value is being read
	index     int    // array element counter
}

// scanDuplicateKeys rejects documents that use the same
// object key twice at the same level. encoding/json
// silently keeps the last value for a duplicated key,
// which would hide operator mistakes, so strict
// parsing fails instead. The scan also rejects
// malformed JSON and non-object roots before
// decoding. It reads keys only and records nothing
// about values.
func scanDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var stack []scanScope
	sawRoot := false
	sawToken := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("malformed JSON: %w", err)
		}
		sawToken = true
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				name, isRoot := childName(&stack)
				if isRoot {
					sawRoot = true
					name = "$"
				}
				stack = append(stack, scanScope{name: name, isObject: true, keys: map[string]bool{}, expectKey: true})
			case '[':
				name, isRoot := childName(&stack)
				if isRoot {
					return fmt.Errorf("configuration root must be a JSON object, got an array")
				}
				stack = append(stack, scanScope{name: name})
			case '}', ']':
				if len(stack) == 0 {
					return fmt.Errorf("malformed JSON: unmatched closing delimiter at offset %d", dec.InputOffset())
				}
				stack = stack[:len(stack)-1]
				// The closed container completed the pending
				// key's value, so the parent object expects
				// its next key.
				if n := len(stack); n > 0 && stack[n-1].isObject {
					stack[n-1].expectKey = true
				}
			}
		default:
			if len(stack) == 0 {
				if sawRoot {
					return fmt.Errorf("unexpected content after the configuration object at offset %d", dec.InputOffset())
				}
				continue
			}
			top := &stack[len(stack)-1]
			if top.isObject && top.expectKey {
				key, ok := tok.(string)
				if !ok {
					return fmt.Errorf("malformed JSON: object key is not a string at offset %d", dec.InputOffset())
				}
				if top.keys[key] {
					return fmt.Errorf("duplicate key %q at %s (offset %d)", key, top.name, dec.InputOffset())
				}
				top.keys[key] = true
				top.expectKey = false
				top.pending = key
				continue
			}
			// A scalar value completes the pending key or
			// one array element.
			if top.isObject {
				top.expectKey = true
				top.pending = ""
			} else {
				top.index++
			}
		}
	}
	if !sawToken {
		return fmt.Errorf("configuration document is empty")
	}
	if !sawRoot {
		return fmt.Errorf("configuration root must be a JSON object")
	}
	if len(stack) != 0 {
		return fmt.Errorf("malformed JSON: document ends inside %s", stack[len(stack)-1].name)
	}
	return nil
}

// childName derives the path segment for a container
// about to be pushed: the pending key inside an
// object, or the next element index inside an array.
// It reports whether the container is the document
// root.
func childName(stack *[]scanScope) (string, bool) {
	if len(*stack) == 0 {
		return "", true
	}
	parent := &(*stack)[len(*stack)-1]
	if parent.isObject {
		name := parent.name + "." + parent.pending
		parent.pending = ""
		return name, false
	}
	name := fmt.Sprintf("%s[%d]", parent.name, parent.index)
	parent.index++
	return name, false
}

// missingRequired reports every required field that is absent or
// zero, using JSON paths. A missing group reports its missing
// leaves, so an absent identity lists every required field it lost
// instead of one opaque "identity is required".
func missingRequired(rv reflect.Value, path string) []error {
	var errs []error
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil // an absent optional group
		}
		return missingRequired(rv.Elem(), path)
	case reflect.Struct:
		rt := rv.Type()
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if field.PkgPath != "" {
				continue // unexported
			}
			child := rv.Field(i)
			childPath := joinPath(path, jsonName(field))
			if field.Tag.Get("required") == "true" && isAbsent(child) {
				errs = append(errs, fmt.Errorf("required field %s is missing", childPath))
				continue
			}
			errs = append(errs, missingRequired(child, childPath)...)
		}
	case reflect.Slice:
		for i := 0; i < rv.Len(); i++ {
			errs = append(errs, missingRequired(rv.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return errs
}

// isAbsent reports whether a required field's value counts as
// missing. A nil pointer or slice is absent; a scalar counts the
// zero value as missing, which is why required booleans are
// pointers (an explicit false is a value, not an absence).
func isAbsent(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer:
		return v.IsNil()
	case reflect.Slice:
		return v.Len() == 0
	default:
		return v.IsZero()
	}
}

func jsonName(field reflect.StructField) string {
	name := strings.Split(field.Tag.Get("json"), ",")[0]
	if name == "" {
		return field.Name
	}
	return name
}

func joinPath(parent, child string) string {
	if parent == "$" {
		return "$." + child
	}
	return parent + "." + child
}
