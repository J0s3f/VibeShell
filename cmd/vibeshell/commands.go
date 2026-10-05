package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/observability"
)

// runCommand starts the service and blocks until a signal requests shutdown.
func runCommand(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath, dirs := configFlags(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}

	snapshot, configDir, err := openSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}

	logger := newServiceLogger(snapshot.Config)
	readiness := observability.NewReadinessReporter()

	ctx, stop := startupContext()
	defer stop()

	c, err := build(ctx, snapshot, configDir, readiness, logger)
	if err != nil {
		return err
	}
	return c.start(ctx)
}

// validateCommand parses and validates the configuration without starting
// anything. It never resolves secret values into output (PLAN 12.4).
func validateCommand(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	configPath, dirs := configFlags(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, _, err := openSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	fmt.Printf("configuration is valid: %s\n", snapshot)
	return nil
}

// statusCommand performs a local readiness check of the configuration and
// storage without opening the SSH listener. It is the internal-only status
// path (PLAN 12.4): reachable through the executable, never a second port.
func statusCommand(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath, dirs := configFlags(fs, defaultConfigPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, configDir, err := openSnapshot(*configPath, *dirs)
	if err != nil {
		return err
	}
	cfg := snapshot.Config

	fmt.Println("subsystem   status     detail")
	fmt.Printf("config      healthy    validated %s\n", *configPath)
	if prompt := resolvePromptPath(cfg, configDir); prompt != "" {
		if _, statErr := os.Stat(prompt); statErr != nil {
			fmt.Printf("presentation degraded  MOTD prompt %s is not readable: %v\n", prompt, statErr)
		} else {
			fmt.Printf("presentation healthy   MOTD prompt %s\n", prompt)
		}
	} else {
		fmt.Println("presentation degraded  no prompts.motd configured")
	}

	summary, err := inspectStorage(context.Background(), cfg)
	if err != nil {
		fmt.Printf("storage     unhealthy  %v\n", err)
		return nil
	}
	fmt.Printf("storage     healthy    schema v%d, %d events\n", summary.SchemaVersion, summary.EventRows)

	if len(cfg.Accounts) == 0 {
		fmt.Println("provider    degraded   no account configured; generation unavailable")
	} else {
		fmt.Printf("provider    healthy    %d account(s) configured\n", len(cfg.Accounts))
	}
	fmt.Printf("ssh         ready      listen %s:%d\n", listenHost(cfg), listenPort(cfg))
	return nil
}

// newServiceLogger builds the operational logger with the configured level.
func newServiceLogger(cfg *config.Config) *observability.Logger {
	level := slog.LevelInfo
	if cfg != nil && cfg.Operations != nil {
		switch cfg.Operations.LogLevel {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return observability.NewLogger(handler)
}

// listenHost returns the configured listen address or the bind-all default.
func listenHost(cfg *config.Config) string {
	if cfg.SSH != nil && cfg.SSH.ListenAddress != "" {
		return cfg.SSH.ListenAddress
	}
	return "0.0.0.0"
}

// listenPort returns the configured SSH port.
func listenPort(cfg *config.Config) int {
	if cfg.SSH != nil {
		return cfg.SSH.ListenPort
	}
	return 0
}
