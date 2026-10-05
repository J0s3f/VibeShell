// Command e2etool prepares and inspects the environment of the SSH
// acceptance suite in tests/e2e.
//
// The suite itself is a POSIX shell script driving the real OpenSSH client
// (tests/e2e/ssh-acceptance.sh). Three things cannot be expressed in that
// script and live here instead:
//
//   - prepare   writes the two strict-JSON service configurations and the
//     owner-only password file the secure-mode run needs. The password file is
//     created through the real config.PasswordStore, so the fixture cannot
//     drift from the format the service accepts.
//   - events    reports the durable state a restart must preserve, read
//     straight from the SQLite event store.
//   - resize    sends a pty-req followed by a window-change request. The
//     OpenSSH command line only emits window-change on SIGWINCH, so a scripted
//     client is the only way to exercise that request deterministically.
//
// No credential material is a literal here: prepare reads the passwords from
// the E2E_ALICE_PASSWORD and E2E_BOB_PASSWORD environment variables, which the
// script generates at run time, and nothing prints them.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/system"

	cryptossh "golang.org/x/crypto/ssh"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "e2etool: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: e2etool prepare|events|resize ...")
	}
	switch args[0] {
	case "prepare":
		return prepare(args[1:])
	case "events":
		return events(args[1:])
	case "resize":
		return resize(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// newFlagSet returns a flag set that reports parse errors instead of exiting,
// so every subcommand fails through the same main error path.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// ---------------------------------------------------------------------------
// prepare
// ---------------------------------------------------------------------------

// testUsers are the secure-mode accounts. "disabled" proves that a disabled
// entry is refused even with the correct password.
var testUsers = []struct {
	username string
	password string // environment variable holding the password
	enabled  bool
}{
	{"alice", "E2E_ALICE_PASSWORD", true},
	{"bob", "E2E_BOB_PASSWORD", true},
	{"disabled", "E2E_DISABLED_PASSWORD", false},
}

// e2eRouteID and e2eAccountIDShape are syntactically valid identities that no
// provider can satisfy: the acceptance run configures a route but no account,
// so every generation attempt has to report the truthful unavailable state.
const (
	e2eRouteID  = "rte_0123456789ABCDEFGHJKMNPQRS"
	e2eHostname = "vibeshell.e2e"
)

func prepare(args []string) error {
	flags := newFlagSet("prepare")
	dir := flags.String("dir", "", "directory to write the fixtures into")
	port := flags.Int("port", 0, "loopback SSH port for the public configuration")
	securePort := flags.Int("secure-port", 0, "loopback SSH port for the secure configuration")
	promptDir := flags.String("prompt-dir", "", "directory holding the administrator prompt files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *port == 0 || *securePort == 0 || *promptDir == "" {
		return errors.New("prepare needs -dir, -port, -secure-port, and -prompt-dir")
	}
	if err := os.MkdirAll(filepath.Join(*dir, "prompts"), 0o700); err != nil {
		return fmt.Errorf("create prompt directory: %w", err)
	}
	if err := copyPrompts(*promptDir, filepath.Join(*dir, "prompts")); err != nil {
		return err
	}

	public, err := renderConfig("public", *port, *dir, "public", "")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "public.json"), public, 0o600); err != nil {
		return fmt.Errorf("write public configuration: %w", err)
	}

	passwordFile := filepath.Join(*dir, "passwords.json")
	if err := writePasswordFile(passwordFile); err != nil {
		return err
	}
	secure, err := renderConfig("secure", *securePort, *dir, "secure", passwordFile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "secure.json"), secure, 0o600); err != nil {
		return fmt.Errorf("write secure configuration: %w", err)
	}

	fmt.Printf("prepared public.json (127.0.0.1:%d) and secure.json (127.0.0.1:%d) in %s\n",
		*port, *securePort, *dir)
	return nil
}

// copyPrompts copies the administrator prompt files into the acceptance work
// directory so both configurations can reference them with relative paths that
// resolve wherever the suite runs.
func copyPrompts(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read prompt directory %q: %w", src, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			return fmt.Errorf("read prompt %q: %w", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o600); err != nil {
			return fmt.Errorf("write prompt %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// writePasswordFile creates the secure-mode password file through the real
// password store, so the fixture always matches the accepted on-disk format.
func writePasswordFile(path string) error {
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale password file: %w", err)
		}
	}
	store, err := config.CreatePasswordStore(path)
	if err != nil {
		return fmt.Errorf("create password file: %w", err)
	}
	random := system.NewRandom()
	for _, user := range testUsers {
		password, err := passwordFromEnv(user.password)
		if err != nil {
			return err
		}
		if _, err := store.AddUser(random, user.username, password, config.DefaultParams); err != nil {
			return fmt.Errorf("add user %q: %w", user.username, err)
		}
		if !user.enabled {
			if err := store.SetEnabled(user.username, false); err != nil {
				return fmt.Errorf("disable user %q: %w", user.username, err)
			}
		}
	}
	return nil
}

// passwordFromEnv refuses to invent a credential: a missing password is an
// error so a run can never silently authenticate with an empty secret.
func passwordFromEnv(name string) ([]byte, error) {
	value := os.Getenv(name)
	if value == "" {
		return nil, fmt.Errorf("%s is not set; the suite generates a random password per run", name)
	}
	return []byte(value), nil
}

// renderConfig writes one strict-JSON configuration. The public and secure
// runs use separate databases so neither can observe the other's state.
func renderConfig(name string, port int, dir, mode, passwordFile string) ([]byte, error) {
	auth := fmt.Sprintf(`"auth": {"mode": %q`, mode)
	if passwordFile != "" {
		auth += fmt.Sprintf(`, "password_file": %q`, passwordFile)
	}
	auth += "}"

	document := fmt.Sprintf(`{
  "version": 1,
  "identity": {
    "system_name": "VibeOS",
    "shell_name": "VibeShell",
    "hostname": %q
  },
  "ssh": {
    "listen_address": "127.0.0.1",
    "listen_port": %d,
    "host_key_file": %q,
    "handshake_timeout_ms": 10000
  },
  %s,
  "sharing": {"enabled": false, "policy_revision": 1},
  "prompts": {
    "motd": "prompts/motd.txt",
    "shell_behavior": "prompts/shell-behavior.txt",
    "app_generation": "prompts/app-generation.txt",
    "app_extension": "prompts/app-extension.txt",
    "world_materialization": "prompts/world-materialization.txt",
    "summary": "prompts/summary-repair.txt",
    "repair": "prompts/summary-repair.txt"
  },
  "providers": [
    {
      "name": "opencode",
      "products": [
        {
          "name": "console",
          "base_url": "https://opencode.e2e.invalid",
          "protocols": ["chat"],
          "default_protocol": "chat"
        }
      ]
    }
  ],
  "routes": [
    {"id": %q, "provider": "opencode", "product": "console", "model": "e2e-never-called", "protocol": "chat"}
  ],
  "tiers": [{"name": "e2e", "routes": [%q]}],
  "persistence": {"database_path": %q},
  "operations": {"log_level": "debug", "shutdown_grace_ms": 5000}
}
`, e2eHostname, port, filepath.Join(dir, name+"_host_key"), auth, e2eRouteID, e2eRouteID,
		filepath.Join(dir, name+".db"))
	return []byte(document), nil
}

// ---------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------

// events reports the durable session journal. The acceptance script uses it to
// show that two sessions of one principal share one durable record and that a
// restart neither loses nor rewrites it.
func events(args []string) error {
	flags := newFlagSet("events")
	dbPath := flags.String("db", "", "path to the service database")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("events needs -db")
	}
	db, err := sqlite.Open(*dbPath, sqlite.Options{})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	rows, err := db.SQL().QueryContext(context.Background(),
		`SELECT session_id, kind, payload_inline FROM events ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	type session struct {
		events int
		kinds  map[string]bool
		user   string
		mode   string
		closed bool
	}
	order := []string{}
	byID := map[string]*session{}
	total := 0
	for rows.Next() {
		var sessionID, kind string
		var payload sql.NullString
		if err := rows.Scan(&sessionID, &kind, &payload); err != nil {
			return fmt.Errorf("scan event: %w", err)
		}
		entry, ok := byID[sessionID]
		if !ok {
			entry = &session{kinds: map[string]bool{}}
			byID[sessionID] = entry
			order = append(order, sessionID)
		}
		entry.events++
		entry.kinds[kind] = true
		total++
		switch kind {
		case string(domain.EventKindSessionStart):
			var start domain.SessionStartPayload
			if err := json.Unmarshal([]byte(payload.String), &start); err != nil {
				return fmt.Errorf("decode session.start payload of %s: %w", sessionID, err)
			}
			entry.user = start.UserID.String()
			entry.mode = start.AuthMode
		case string(domain.EventKindSessionEnd):
			entry.closed = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate events: %w", err)
	}

	users := map[string]bool{}
	closed := 0
	for _, id := range order {
		entry := byID[id]
		users[entry.user] = true
		if entry.closed {
			closed++
		}
		kinds := make([]string, 0, len(entry.kinds))
		for kind := range entry.kinds {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		fmt.Printf("session id=%s user=%s mode=%s events=%d closed=%t kinds=%s\n",
			id, entry.user, entry.mode, entry.events, entry.closed, strings.Join(kinds, ","))
	}
	fmt.Printf("summary events=%d sessions=%d users=%d closed=%d\n", total, len(order), len(users), closed)
	return nil
}

// ---------------------------------------------------------------------------
// resize
// ---------------------------------------------------------------------------

// resize opens one public-mode session with a pty, waits for the prompt, sends
// a window-change request with different dimensions, and proves the session
// still serves a command afterwards.
func resize(args []string) error {
	flags := newFlagSet("resize")
	addr := flags.String("addr", "", "host:port of the running service")
	user := flags.String("user", "alice", "username to authenticate as")
	cols := flags.Int("cols", 100, "initial terminal columns")
	rows := flags.Int("rows", 40, "initial terminal rows")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *addr == "" {
		return errors.New("resize needs -addr")
	}

	client, err := cryptossh.Dial("tcp", *addr, &cryptossh.ClientConfig{
		User:            *user,
		Auth:            nil, // public mode accepts the "none" method
		HostKeyCallback: cryptossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open session channel: %w", err)
	}
	defer session.Close()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	modes := cryptossh.TerminalModes{cryptossh.ECHO: 1}
	if err := session.RequestPty("xterm-256color", *rows, *cols, modes); err != nil {
		return fmt.Errorf("pty-req %dx%d: %w", *cols, *rows, err)
	}
	if err := session.Shell(); err != nil {
		return fmt.Errorf("shell request: %w", err)
	}

	reader := newLineReader(stdout)
	if _, err := reader.await(":", 20*time.Second); err != nil {
		return fmt.Errorf("await initial prompt: %w", err)
	}
	newCols, newRows := *cols+20, *rows+10
	if err := session.WindowChange(newRows, newCols); err != nil {
		return fmt.Errorf("window-change %dx%d: %w", newCols, newRows, err)
	}
	if _, err := stdin.Write([]byte("pwd\r")); err != nil {
		return fmt.Errorf("write command after resize: %w", err)
	}
	line, err := reader.await("/home/", 20*time.Second)
	if err != nil {
		return fmt.Errorf("await command output after window-change: %w", err)
	}
	_, _ = stdin.Write([]byte("\x04"))
	_ = session.Wait()

	fmt.Printf("resize ok pty=%dx%d window-change=%dx%d session-survived=%t cwd=%s\n",
		*cols, *rows, newCols, newRows, strings.Contains(line, "/home/"), strings.TrimSpace(line))
	return nil
}

// lineReader turns the raw session stream into readable lines with a bounded
// wait, so the acceptance run fails with a clear reason instead of hanging.
type lineReader struct {
	source chan []byte
	buffer []byte
}

func newLineReader(stdout interface{ Read([]byte) (int, error) }) *lineReader {
	reader := &lineReader{source: make(chan []byte, 256)}
	go func() {
		defer close(reader.source)
		chunk := make([]byte, 4096)
		for {
			n, err := stdout.Read(chunk)
			if n > 0 {
				reader.source <- append([]byte(nil), chunk[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	return reader
}

// await reads until a chunk containing want arrives or the deadline ends.
func (r *lineReader) await(want string, timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-r.source:
			if !ok {
				return "", fmt.Errorf("session stream ended while waiting for %q (buffer %q)", want, r.buffer)
			}
			r.buffer = append(r.buffer, chunk...)
			if strings.Contains(string(chunk), want) {
				return strings.TrimSpace(string(chunk)), nil
			}
		case <-deadline:
			return "", fmt.Errorf("timed out waiting for %q (buffer %q)", want, r.buffer)
		}
	}
}
