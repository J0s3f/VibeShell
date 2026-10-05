package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/observability"
)

// TestShutdownWaitsForHandlerAndRecordsSessionEnd is the regression test for
// the D04 shutdown race: component.shutdown used to close the event store
// while SSH handler goroutines could still call shell.End, so a session.end
// event could be lost. The test starts the real service, opens a session, and
// triggers shutdown while a session handler is still in flight; it then proves
// shutdown waited for that handler and that the session end is durable.
func TestShutdownWaitsForHandlerAndRecordsSessionEnd(t *testing.T) {
	dir := t.TempDir()
	port := freeLoopbackPort(t)
	configPath := filepath.Join(dir, "vibeshell.json")
	if err := os.WriteFile(configPath, []byte(shutdownTestConfig(dir, port)), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	snapshot, err := config.NewLoader(dir, config.Options{}).Load(mustReadFile(t, configPath))
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}

	logger := observability.NewLogger(slog.NewJSONHandler(io.Discard, nil))
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBuild()
	c, err := build(buildCtx, snapshot, dir, observability.NewReadinessReporter(), logger)
	if err != nil {
		t.Fatalf("build component: %v", err)
	}

	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- c.start(serveCtx) }()

	// Open a real SSH session so a genuine handler goroutine is running.
	client := dialPublic(t, port)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open ssh session: %v", err)
	}
	defer sess.Close()
	// Hold stdin open with a pipe so the server never sees EOF and ends the
	// session on its own before shutdown is triggered.
	stdinReader, stdinWriter := io.Pipe()
	sess.Stdin = stdinReader
	sess.Stdout = io.Discard
	defer stdinWriter.Close()
	if err := sess.RequestPty("xterm", 24, 80, cryptossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("request shell: %v", err)
	}
	// Wait until the handler has been admitted and opened its session, so the
	// in-flight handler the test relies on is really registered.
	waitForActiveSession(t, c)

	// Hold one extra handler token so shutdown must wait for an in-flight
	// handler deterministically. Without the fix shutdown does not wait for
	// handlers at all, so it would return while this token is held.
	if !c.handler.sessions.enter() {
		t.Fatal("handler admission was already closed before shutdown")
	}
	held := true
	defer func() {
		if held {
			c.handler.sessions.leave()
		}
	}()

	shutdownReturned := make(chan error, 1)
	go func() { shutdownReturned <- c.shutdown() }()

	select {
	case err := <-shutdownReturned:
		t.Fatalf("shutdown returned while a session handler was still in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// No new session may be admitted once shutdown has begun.
	if c.handler.sessions.enter() {
		t.Fatal("handler admission stayed open during shutdown")
	}

	// Release the held handler; shutdown must now finish.
	c.handler.sessions.leave()
	held = false
	select {
	case err := <-shutdownReturned:
		if err != nil {
			t.Fatalf("shutdown returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown did not return after the in-flight handler finished")
	}

	// Give the serve goroutine a moment to observe the ended context; the
	// listener is closed by shutdown, so Serve must return.
	select {
	case <-serveDone:
	case <-time.After(10 * time.Second):
		t.Fatal("start did not return after shutdown")
	}

	assertEverySessionEnded(t, filepath.Join(dir, "world.db"))
}

// shutdownTestConfig is a public-mode configuration with one loopback listener
// and no provider account, so the fallback engine answers every command.
func shutdownTestConfig(dir string, port int) string {
	return fmt.Sprintf(`{
  "version": 1,
  "identity": {"system_name": "VibeOS", "shell_name": "VibeShell", "hostname": "vibeshell.test"},
  "ssh": {"listen_address": "127.0.0.1", "listen_port": %d, "host_key_file": %q, "handshake_timeout_ms": 10000},
  "auth": {"mode": "public"},
  "sharing": {"enabled": false},
  "providers": [{"name": "opencode", "products": [{"name": "console", "base_url": "https://opencode.example.invalid", "protocols": ["chat"], "default_protocol": "chat"}]}],
  "routes": [{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "shutdown-test", "protocol": "chat"}],
  "tiers": [{"name": "shutdown-test", "routes": ["rte_0123456789ABCDEFGHJKMNPQRS"]}],
  "persistence": {"database_path": %q},
  "operations": {"shutdown_grace_ms": 8000}
}`, port, filepath.Join(dir, "host_key"), filepath.Join(dir, "world.db"))
}

// waitForActiveSession polls until the coordinator has registered the session
// the client opened, which proves the handler passed admission.
func waitForActiveSession(t *testing.T, c *component) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c.coordinator.ActiveSessions() >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the opened session was never registered with the coordinator")
}

// freeLoopbackPort reserves and immediately releases a loopback port. The
// component binds it during build, so the window between release and bind is
// the ordinary bind race a test accepts on a loopback interface.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return port
}

// dialPublic connects a public-mode client with no credential, retrying while
// the freshly started server reaches its accept loop.
func dialPublic(t *testing.T, port int) *cryptossh.Client {
	t.Helper()
	cfg := &cryptossh.ClientConfig{
		User:            "alice",
		HostKeyCallback: cryptossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var lastErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		client, err := cryptossh.Dial("tcp", addr, cfg)
		if err == nil {
			t.Cleanup(func() { _ = client.Close() })
			return client
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", addr, lastErr)
	return nil
}

// assertEverySessionEnded reopens the durable store and requires that every
// recorded session has exactly one session.end event.
func assertEverySessionEnded(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sqlite.Open(dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer db.Close()

	rows, err := db.SQL().QueryContext(context.Background(),
		`SELECT session_id,
		        SUM(kind = 'session.start') AS starts,
		        SUM(kind = 'session.end') AS ends
		   FROM events GROUP BY session_id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()

	sessions := 0
	for rows.Next() {
		var sessionID string
		var starts, ends sql.NullInt64
		if err := rows.Scan(&sessionID, &starts, &ends); err != nil {
			t.Fatalf("scan events: %v", err)
		}
		sessions++
		if starts.Int64 < 1 {
			t.Errorf("session %s has no session.start", sessionID)
		}
		if ends.Int64 != 1 {
			t.Errorf("session %s recorded %d session.end events, want exactly 1", sessionID, ends.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	if sessions == 0 {
		t.Fatal("no sessions were recorded")
	}
}

// mustReadFile reads a file or fails the test.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
