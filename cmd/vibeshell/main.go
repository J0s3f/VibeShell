// Command vibeshell is the entry point of the VibeShell service.
//
// It is the composition root: it parses flags, loads and validates the
// strict JSON configuration, constructs every adapter and application
// service, starts the SSH transport, and owns startup/shutdown ordering
// (PLAN 12.3). Business rules do not live here; each dependency is built and
// handed to the component that owns it.
//
// Subcommands:
//
//	vibeshell run     start the service (default)
//	vibeshell validate print the effective configuration and exit
//	vibeshell status   report readiness of the local configuration and storage
//	vibeshell admin    internal-only operator operations (PLAN 10.5)
//
// The status/validate/admin paths are the internal-only operator surface
// (PLAN 12.4): no second public port is opened.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"j0s.at/vibeshell/internal/buildinfo"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vibeshell: "+err.Error())
		os.Exit(1)
	}
}

// run dispatches the requested subcommand. An unrecognised first argument is
// treated as a flag for the default "run" command so `vibeshell -config X`
// works without a subcommand.
func run(args []string) error {
	command := "run"
	if len(args) > 0 && !isFlag(args[0]) {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "run":
		return runCommand(args)
	case "validate":
		return validateCommand(args)
	case "status":
		return statusCommand(args)
	case "admin":
		return adminCommand(args)
	case "version":
		fmt.Println(buildinfo.Current().String())
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (try: vibeshell help)", command)
	}
}

// isFlag reports whether arg begins a flag rather than naming a subcommand.
func isFlag(arg string) bool {
	return len(arg) > 0 && arg[0] == '-'
}

// printUsage writes the command help to stdout.
func printUsage() {
	fmt.Print(`VibeShell service

Usage:
  vibeshell [run]      [-config PATH] [-publish-ssh] start the service
  vibeshell validate   [-config PATH] parse and validate configuration
  vibeshell status     [-config PATH] report readiness of config and storage
  vibeshell admin      internal-only operator operations (try: vibeshell admin help)
  vibeshell version

Flags:
  -config PATH   strict JSON configuration file (default /etc/vibeshell/vibeshell.json)
  -secret-dir D  allowed secret directory (repeatable; file secret refs are fail-closed without one)
`)
}

// configFlags registers the flags shared by every subcommand.
func configFlags(fs *flag.FlagSet, defaultConfig string) (*string, *secretDirs) {
	configPath := fs.String("config", defaultConfig, "path to the strict JSON configuration file")
	dirs := &secretDirs{}
	fs.Var(dirs, "secret-dir", "allowed secret directory (repeatable)")
	return configPath, dirs
}

// secretDirs collects repeatable secret directory flags.
type secretDirs []string

func (d *secretDirs) String() string { return fmt.Sprint([]string(*d)) }

func (d *secretDirs) Set(value string) error {
	*d = append(*d, value)
	return nil
}

// errConfigRequired names a missing configuration path clearly.
var errConfigRequired = errors.New("a configuration file is required (-config PATH)")
