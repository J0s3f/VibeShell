package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Loader reads configuration documents from disk and turns
// them into validated candidates. Parsing and semantic
// validation are pure; the loader is the boundary that
// performs I/O: prompt files must exist and every account
// secret reference must resolve.
type Loader struct {
	// BaseDir is the directory prompt paths resolve against.
	// When empty, the configuration file's own directory
	// is used.
	BaseDir string
	// Resolver resolves secret references. When nil, a
	// FileEnvResolver with SecretsDir as the allowed
	// directory is used.
	Resolver SecretResolver
	// SecretsDir is the allowed directory for {file:...}
	// references of the default resolver.
	SecretsDir string
	// Catalogue validates model-route references.
	Catalogue Catalogue
}

// Candidate is a fully validated configuration: every
// semantic rule passed, every prompt file exists, and every
// account secret reference resolved. A candidate becomes
// visible to turns only when a Store publishes it.
type Candidate struct {
	Config Config

	secrets map[string]Secret   // account ID -> resolved secret
	routes  map[string][]string // tier name -> expanded route IDs
}

// Load reads, strictly parses, semantically validates, and
// resolves one configuration document. It performs no
// publication: the caller decides when a candidate
// replaces the active snapshot.
func (l *Loader) Load(path string) (Candidate, error) {
	var candidate Candidate
	data, err := os.ReadFile(path)
	if err != nil {
		return candidate, fmt.Errorf("cannot read configuration file %q: %w", path, err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return candidate, fmt.Errorf("configuration file %q is not valid: %w", path, err)
	}
	if err := Validate(cfg, l.Catalogue); err != nil {
		return candidate, fmt.Errorf("configuration file %q failed semantic validation: %w", path, err)
	}
	baseDir := l.BaseDir
	if baseDir == "" {
		baseDir = filepath.Dir(path)
	}
	if cfg.Prompts != nil {
		if err := checkPromptFiles(*cfg.Prompts, baseDir); err != nil {
			return candidate, fmt.Errorf("configuration file %q: %w", path, err)
		}
	}
	resolver := l.Resolver
	if resolver == nil {
		resolver = FileEnvResolver{AllowedDir: l.SecretsDir}
	}
	secrets := make(map[string]Secret, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		secret, err := resolver.Resolve(account.SecretRef)
		if err != nil {
			return candidate, fmt.Errorf("account %q: secret reference %s failed: %w", account.ID, account.SecretRef, err)
		}
		secrets[account.ID] = secret
	}
	routes := make(map[string][]string, len(cfg.Tiers))
	for _, tier := range cfg.Tiers {
		routes[tier.Name] = ExpandTierRoutes(tier, l.Catalogue)
	}
	return Candidate{Config: cfg, secrets: secrets, routes: routes}, nil
}

// checkPromptFiles verifies that every configured prompt
// file exists and stays inside the configuration
// directory, so a configuration bundle is self-contained.
func checkPromptFiles(prompts PromptsConfig, baseDir string) error {
	for _, prompt := range []struct {
		name string
		path string
	}{
		{"prompts.motd", prompts.MOTD},
		{"prompts.shell", prompts.Shell},
		{"prompts.app_generate", prompts.AppGenerate},
		{"prompts.app_extend", prompts.AppExtend},
		{"prompts.world", prompts.World},
		{"prompts.summary", prompts.Summary},
		{"prompts.repair", prompts.Repair},
	} {
		if err := checkPromptFile(prompt.name, prompt.path, baseDir); err != nil {
			return err
		}
	}
	return nil
}

func checkPromptFile(name, path, baseDir string) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	path = filepath.Clean(path)
	if !withinDir(filepath.Clean(baseDir), path) {
		return fmt.Errorf("%s: %q escapes the configuration directory %s", name, path, baseDir)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: prompt file does not exist: %w", name, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s: %q is a directory, not a prompt file", name, path)
	}
	return nil
}
