package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
)

// Loader validates everything that needs the filesystem and builds
// snapshots. It is the only place configuration files, prompt files, and
// secret values are read.
type Loader struct {
	// BaseDir is the configuration file's directory. Relative prompt paths
	// resolve against it; a relative path without a BaseDir is an error
	// rather than a process-cwd guess.
	BaseDir string
	// Options carries the secret-directory policy for parsing.
	Options Options
	// LookupEnv resolves environment secret references; nil means
	// os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// NewLoader returns a loader for a configuration file located at dir.
func NewLoader(dir string, opts Options) *Loader {
	return &Loader{BaseDir: dir, Options: opts, LookupEnv: os.LookupEnv}
}

// Validate parses the document and checks every referenced file (prompts,
// the secure-mode password file) without resolving secret values, so it can
// answer "is this configuration usable?" without holding credentials.
func (l *Loader) Validate(raw []byte) error {
	cfg, err := Parse(raw, l.Options)
	if err != nil {
		return err
	}
	return newValidationError(l.checkFiles(cfg))
}

// Load validates and resolves every secret reference into a snapshot. Any
// problem returns a ValidationError and no snapshot: partial configurations
// never reach the published state.
func (l *Loader) Load(raw []byte) (*Snapshot, error) {
	cfg, err := Parse(raw, l.Options)
	if err != nil {
		return nil, err
	}
	problems := l.checkFiles(cfg)
	secrets, secretProblems := l.resolveSecrets(cfg)
	problems = append(problems, secretProblems...)
	if err := newValidationError(problems); err != nil {
		return nil, err
	}
	return &Snapshot{Config: cfg, secrets: secrets}, nil
}

// checkFiles verifies that prompt and password file references exist, are
// regular files, and are readable.
func (l *Loader) checkFiles(cfg *Config) []Problem {
	var problems []Problem
	if cfg.Prompts != nil {
		for _, pf := range promptFiles(cfg.Prompts) {
			if pf.file == "" { // optional app_extension left unset
				continue
			}
			field := "prompts." + pf.field
			resolved, err := l.resolveRef(pf.file)
			if err != nil {
				problems = append(problems, Problem{Path: field, Message: err.Error()})
				continue
			}
			if err := checkReferencedFile(resolved, true); err != nil {
				problems = append(problems, Problem{Path: field, Message: err.Error()})
			}
		}
	}
	if cfg.Auth != nil && cfg.Auth.Mode == AuthModeSecure && cfg.Auth.PasswordFile != "" {
		if err := checkPasswordFile(cfg.Auth.PasswordFile); err != nil {
			problems = append(problems, Problem{Path: "auth.password_file", Message: err.Error()})
		}
	}
	return problems
}

// resolveSecrets reads each account's secret exactly once, keyed by the
// reference so accounts sharing a reference share one value.
func (l *Loader) resolveSecrets(cfg *Config) (map[string]Secret, []Problem) {
	secrets := map[string]Secret{}
	if cfg == nil || len(cfg.Accounts) == 0 {
		return secrets, nil
	}
	resolver := &SecretResolver{AllowedDirs: l.Options.SecretDirs, LookupEnv: l.LookupEnv}
	var problems []Problem
	for i, account := range cfg.Accounts {
		if account.SecretRef == "" { // credential-less cli account: nothing to resolve
			continue
		}
		value, err := resolver.Resolve(account.SecretRef)
		if err != nil {
			problems = append(problems, Problem{
				Path:    fmt.Sprintf("accounts[%d].secret_ref", i),
				Message: err.Error(),
			})
			continue
		}
		secrets[account.SecretRef] = value
	}
	return secrets, problems
}

// resolveRef turns a configured prompt path into the path to open.
func (l *Loader) resolveRef(file string) (string, error) {
	if path.IsAbs(file) {
		return file, nil
	}
	if l.BaseDir == "" {
		return "", fmt.Errorf("relative path %q needs the configuration file's directory to resolve", file)
	}
	return path.Join(l.BaseDir, file), nil
}

// checkReferencedFile verifies existence, regular-file type, readability,
// and (for prompts) non-empty content.
func checkReferencedFile(file string, requireContent bool) error {
	info, err := os.Stat(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("referenced file %q does not exist", file)
		}
		return fmt.Errorf("cannot stat referenced file %q: %v", file, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("referenced file %q is not a regular file", file)
	}
	if requireContent && info.Size() == 0 {
		return fmt.Errorf("referenced file %q is empty", file)
	}
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("referenced file %q is not readable: %v", file, err)
	}
	_ = f.Close()
	return nil
}
