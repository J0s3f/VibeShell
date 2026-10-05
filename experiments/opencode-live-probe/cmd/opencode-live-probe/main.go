// Command opencode-live-probe runs the E02 bounded live probe.
//
// The key is accepted ONLY via --key-env NAME or --key-file PATH, used
// only for --live, as an Authorization header value. It is never read
// from a literal flag, printed, or written to receipts.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"j0s.at/vibeshell/experiments/opencode-live-probe/probe"
	gateway "j0s.at/vibeshell/experiments/opencode-protocol"
)

func main() {
	fs := flag.NewFlagSet("opencode-live-probe", flag.ExitOnError)
	live := fs.Bool("live", false, "run the bounded live probe (requires a key via --key-env/--key-file)")
	keyEnv := fs.String("key-env", "", "name of the environment variable holding the key (only with --live)")
	keyFile := fs.String("key-file", "", "path to a file holding the key (only with --live)")
	outDir := fs.String("out", "receipts", "directory for dated receipts (live mode)")
	metadata := fs.String("metadata", "", "models.dev snapshot for family classification")
	consoleBase := fs.String("console-base", "", "override Console base URL")
	goBase := fs.String("go-base", "", "override Go base URL")
	timeout := fs.Duration("timeout", 60*time.Second, "per-request timeout")
	families := fs.String("families", "", "comma list of protocols to probe (default all)")
	_ = fs.Parse(os.Args[1:])

	if (*keyEnv != "" || *keyFile != "") && !*live {
		fmt.Fprintln(os.Stderr, "key source flags require --live")
		os.Exit(2)
	}

	cfg := &probe.Config{
		Live:         *live,
		KeyEnv:       *keyEnv,
		KeyFile:      *keyFile,
		ConsoleBase:  *consoleBase,
		GoBase:       *goBase,
		MetadataPath: *metadata,
		OutDir:       *outDir,
		Timeout:      *timeout,
	}
	if *families != "" {
		for _, f := range strings.Split(*families, ",") {
			cfg.Families = append(cfg.Families, gateway.Protocol(strings.TrimSpace(f)))
		}
	}
	os.Exit(probe.Run(context.Background(), cfg))
}
