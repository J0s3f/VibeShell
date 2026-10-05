package sshserver_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/sshserver"
	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termdecode"
)

// dialTimeout bounds every client handshake in these tests. It is generous
// because a failure should be reported as a protocol problem, not a timeout.
const dialTimeout = 10 * time.Second

// harness runs one Server on a loopback listener.
type harness struct {
	addr   string
	served chan error
	logs   *syncBuffer
}

// start brings up a server with opts and returns its address. The server is
// shut down and its goroutines joined when the test ends.
func start(t *testing.T, opts sshserver.Options) *harness {
	t.Helper()
	logs := &syncBuffer{}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if opts.ShellHandler == nil {
		opts.ShellHandler = func(*sshserver.Session) {}
	}
	server, err := sshserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after cancellation")
		}
	})
	return &harness{addr: listener.Addr().String(), served: served, logs: logs}
}

func (h *harness) clientConfig(username string, auth ...ssh.AuthMethod) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: username,
		Auth: auth,
		// The spike generates an ephemeral host key per server; a test must not
		// depend on it.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	}
}

// dial connects a client. An empty auth slice models a client that offers no
// credential at all, which is what public mode has to accept.
func (h *harness) dial(t *testing.T, username string, auth ...ssh.AuthMethod) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", h.addr, h.clientConfig(username, auth...))
	if err != nil {
		t.Fatalf("dial as %q: %v", username, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// dialExpectingFailure connects a client that has to be refused.
func (h *harness) dialExpectingFailure(t *testing.T, username string, auth ...ssh.AuthMethod) error {
	t.Helper()
	client, err := ssh.Dial("tcp", h.addr, h.clientConfig(username, auth...))
	if err == nil {
		_ = client.Close()
		t.Fatalf("dial as %q succeeded but had to be refused", username)
	}
	return err
}

// ---------------------------------------------------------------------------
// Session test doubles
// ---------------------------------------------------------------------------

// observed pairs a decoded event with the session that produced it, so a test
// can tell two sessions on one connection apart.
type observed struct {
	session *sshserver.Session
	event   termdecode.Event
}

// probe is the spike's stand-in for a VibeShell shell session: it records what
// it decoded, echoes a line per event, and exits when the client closes its
// input.
type probe struct {
	events  chan observed
	signals chan string
	started chan *sshserver.Session
	notes   chan string
}

func newProbe() *probe {
	return &probe{
		events:  make(chan observed, 1024),
		signals: make(chan string, 16),
		started: make(chan *sshserver.Session, 8),
		notes:   make(chan string, 64),
	}
}

func (p *probe) handler(session *sshserver.Session) {
	fmt.Fprintf(session, "welcome %s terminal=%s\r\n", session.Principal().Username, session.Terminal())
	select {
	case p.started <- session:
	default:
	}
	for {
		select {
		case <-session.Context().Done():
			return
		case <-session.InputClosed():
			p.note("input-closed err=%v", session.InputError())
			_ = session.Exit(0)
			return
		case signal := <-session.Signals():
			select {
			case p.signals <- signal:
			default:
			}
			p.note("signal %s", signal)
		case event := <-session.Events():
			select {
			case p.events <- observed{session: session, event: event}:
			default:
			}
			fmt.Fprintf(session, "event %s\r\n", describe(event))
		}
	}
}

// note records something the session observed. A full queue must not block a
// session, so a dropped note is acceptable.
func (p *probe) note(format string, args ...any) {
	select {
	case p.notes <- fmt.Sprintf(format, args...):
	default:
	}
}

// await waits for the probe's handler to start and returns the server session.
func (p *probe) await(t *testing.T) *sshserver.Session {
	t.Helper()
	select {
	case session := <-p.started:
		return session
	case <-time.After(dialTimeout):
		t.Fatal("session handler did not start")
		return nil
	}
}

func (p *probe) awaitEvent(t *testing.T) observed {
	t.Helper()
	select {
	case got := <-p.events:
		return got
	case <-time.After(dialTimeout):
		t.Fatal("no decoded event arrived")
		return observed{}
	}
}

// awaitNote waits for a note matching prefix and returns it.
func (p *probe) awaitNote(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.After(dialTimeout)
	for {
		select {
		case note := <-p.notes:
			if strings.HasPrefix(note, prefix) {
				return note
			}
		case <-deadline:
			t.Fatalf("no note starting with %q", prefix)
			return ""
		}
	}
}

func describe(event termdecode.Event) string {
	switch typed := event.(type) {
	case termdecode.Key:
		return "key(" + typed.String() + ")"
	case termdecode.Paste:
		return fmt.Sprintf("paste(%q)", typed.Text)
	case termdecode.Resize:
		return fmt.Sprintf("resize(%dx%d)", typed.Cols, typed.Rows)
	default:
		return fmt.Sprintf("%T", event)
	}
}

// streams captures what a client session receives.
type streams struct {
	stdout *syncBuffer
	stderr *syncBuffer
}

func attachStreams(session *ssh.Session) *streams {
	captured := &streams{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	session.Stdout = captured.stdout
	session.Stderr = captured.stderr
	return captured
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// clientSession is a client session plus the pipe that feeds its stdin. Closing
// that pipe is how a test sends SSH EOF, which is not the same as disconnecting.
type clientSession struct {
	*ssh.Session
	stdin   io.WriteCloser
	streams *streams
}

// newSession returns a client session with its streams captured and its stdin
// connected to a pipe the test writes into.
func newSession(t *testing.T, client *ssh.Client) *clientSession {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	captured := attachStreams(session)
	reader, writer := io.Pipe()
	session.Stdin = reader
	return &clientSession{Session: session, stdin: writer, streams: captured}
}

// eof ends the client's input stream without disconnecting.
func (s *clientSession) eof() {
	_ = s.stdin.Close()
}

// startShell requests a pty with the given cell size, starts the shell, and
// returns the client session.
func startShell(t *testing.T, client *ssh.Client, term string, cols, rows int) *clientSession {
	t.Helper()
	session := newSession(t, client)
	if term != "" {
		// RequestPty takes (term, height, width).
		if err := session.RequestPty(term, rows, cols, ssh.TerminalModes{}); err != nil {
			t.Fatalf("RequestPty: %v", err)
		}
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	return session
}

// openRawSession opens a session channel without the ssh.Session wrapper, so a
// test can read the failure message a refusal carries. The request stream is
// drained because the server closes the channel after every refusal.
func openRawSession(t *testing.T, client *ssh.Client) (ssh.Channel, *syncBuffer) {
	t.Helper()
	channel, requests, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open session channel: %v", err)
	}
	go func() {
		for req := range requests {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()
	stderr := &syncBuffer{}
	go func() {
		_, _ = io.Copy(stderr, channel.Stderr())
	}()
	t.Cleanup(func() { _ = channel.Close() })
	return channel, stderr
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

func TestPublicModeAcceptsAnyUsernameWithNoCredential(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})

	for _, username := range []string{"ada", "root", "user-with-dots.and-dashes", "用户"} {
		client := h.dial(t, username) // no Auth: the client offers no credential at all
		if got := client.User(); got != username {
			t.Errorf("client user = %q, want %q", got, username)
		}
	}
}

func TestPublicModeNeverChallengesForACredential(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})

	// A client that is willing to type a password still connects without one:
	// the server accepts the SSH "none" method, so there is nothing to prompt
	// for. An x/crypto client cannot observe which methods a server advertises,
	// so the OpenSSH client's behaviour is the receipt evidence.
	client := h.dial(t, "ada", ssh.Password("anything"))
	session := startShell(t, client, "xterm", 80, 24)
	session.eof()
	if err := session.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestPublicModeRefusesUnusableUsernames(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})

	for _, username := range []string{"", "two words", "with\ttab", "with\nnewline", "path/segment", strings.Repeat("n", 65)} {
		h.dialExpectingFailure(t, username)
	}
}

func TestPasswordModeAuthenticatesOnlyWithTheRightPassword(t *testing.T) {
	probe := newProbe()
	passwords := sshserver.PasswordFile{
		"ada":  {Password: "correct horse"},
		"off":  {Password: "correct horse", Disabled: true},
		"long": {Password: strings.Repeat("x", sshserver.MaxPasswordBytes+1)},
	}
	h := start(t, sshserver.Options{
		Mode:         sshserver.ModePassword,
		Passwords:    passwords,
		ShellHandler: probe.handler,
	})

	t.Run("correct password", func(t *testing.T) {
		client := h.dial(t, "ada", ssh.Password("correct horse"))
		session := startShell(t, client, "xterm-256color", 80, 24)
		session.eof()
		if err := session.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if note := probe.awaitNote(t, "input-closed"); note != "input-closed err=<nil>" {
			t.Errorf("session input ended with %q, want a clean EOF", note)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		h.dialExpectingFailure(t, "ada", ssh.Password("wrong horse"))
	})

	t.Run("unknown user", func(t *testing.T) {
		h.dialExpectingFailure(t, "grace", ssh.Password("correct horse"))
	})

	t.Run("disabled account", func(t *testing.T) {
		h.dialExpectingFailure(t, "off", ssh.Password("correct horse"))
	})

	t.Run("oversized password", func(t *testing.T) {
		h.dialExpectingFailure(t, "ada", ssh.Password(strings.Repeat("x", sshserver.MaxPasswordBytes+1)))
	})

	t.Run("no credential offered", func(t *testing.T) {
		h.dialExpectingFailure(t, "ada")
	})
}

func TestPasswordModeOffersNoOtherMethod(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{
		Mode:         sshserver.ModePassword,
		Passwords:    sshserver.PasswordFile{"ada": {Password: "secret"}},
		ShellHandler: probe.handler,
	})

	// Public-key, certificate, and keyboard-interactive clients are all refused:
	// the contract advertises password authentication only.
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	h.dialExpectingFailure(t, "ada", ssh.PublicKeys(signer))
	h.dialExpectingFailure(t, "ada", ssh.KeyboardInteractive(
		func(name, instruction string, questions []string, echos []bool) ([]string, error) {
			return make([]string, len(questions)), nil
		}))
}

// ---------------------------------------------------------------------------
// Session channels
// ---------------------------------------------------------------------------

func TestSessionCarriesStdinStdoutEOFAndExitStatus(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	session := startShell(t, client, "xterm-256color", 80, 24)

	if _, err := session.stdin.Write([]byte("echo hello\r")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	got := waitForText(t, func() string { return session.streams.stdout.String() }, "welcome ada")
	if !strings.Contains(got, "welcome ada terminal=xterm-256color 80x24") {
		t.Fatalf("stdout = %q, want the welcome line with the negotiated terminal", got)
	}
	// The pty-req size arrives as the first event, then the typed runes.
	assertResize(t, probe.awaitEvent(t).event, 80, 24)
	assertKey(t, probe.awaitEvent(t).event, termdecode.KeyRune, 'e')

	// EOF is the client closing its input stream, not a disconnect.
	session.eof()
	if err := session.Wait(); err != nil {
		t.Fatalf("Wait after EOF: %v, want a clean exit", err)
	}
	if note := probe.awaitNote(t, "input-closed"); note != "input-closed err=<nil>" {
		t.Errorf("input ended with %q, want a clean EOF", note)
	}
}

func TestPTYRequestCarriesTerminalAndSize(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	_ = startShell(t, client, "xterm-256color", 132, 43)

	server := probe.await(t)
	if !server.Terminal().Requested {
		t.Fatal("terminal was not recorded as requested")
	}
	if got := server.Terminal().Name; got != "xterm-256color" {
		t.Errorf("TERM = %q, want xterm-256color", got)
	}
	if got, want := server.Terminal().Cols, 132; got != want {
		t.Errorf("cols = %d, want %d", got, want)
	}
	if got, want := server.Terminal().Rows, 43; got != want {
		t.Errorf("rows = %d, want %d", got, want)
	}
	// The initial size is also delivered as a resize event, so a handler that
	// only watches events still sees the first size.
	assertResize(t, probe.awaitEvent(t).event, 132, 43)
}

func TestPTYRequestOutOfRangeDimensionsIsRefused(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})

	for _, testCase := range []struct {
		name string
		cols int
		rows int
	}{
		{name: "absurd columns", cols: 1 << 20, rows: 24},
		{name: "absurd rows", cols: 80, rows: 1 << 20},
		{name: "both absurd", cols: 1 << 20, rows: 1 << 20},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := h.dial(t, "ada")
			session, err := client.NewSession()
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			if err := session.RequestPty("xterm", testCase.rows, testCase.cols, ssh.TerminalModes{}); err == nil {
				t.Fatalf("RequestPty(%d, %d) was accepted", testCase.cols, testCase.rows)
			}
		})
	}
}

func TestPTYRequestWithoutUsableDimensionsUsesTheDocumentedDefault(t *testing.T) {
	// A real OpenSSH client reports zero when it has no size to send, which is
	// what `ssh -tt host` does when stdin is a pipe. Zero means "unknown", not
	// "malformed".
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	session := startShell(t, client, "dumb", 0, 0)

	server := probe.await(t)
	if !server.Terminal().Requested {
		t.Fatal("the pty request was not recorded")
	}
	if got := server.Terminal().Cols; got != 80 {
		t.Errorf("cols = %d, want the default 80", got)
	}
	if got := server.Terminal().Rows; got != 24 {
		t.Errorf("rows = %d, want the default 24", got)
	}
	session.eof()
	if err := session.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestPTYRequestWithOversizedModesIsRefused(t *testing.T) {
	// A real OpenSSH client sends about 150 bytes of terminal modes for a stock
	// xterm, so the bound is generous; a longer payload is still refused.
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	err = session.RequestPty("xterm", 24, 80, modes)
	if err != nil {
		t.Fatalf("RequestPty with normal terminal modes was refused: %v", err)
	}

	// The payload is built by hand because the client API cannot express an
	// oversized modes string.
	channel, requests, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open session channel: %v", err)
	}
	go ssh.DiscardRequests(requests)
	oversized := ssh.Marshal(struct {
		Term                      string
		Columns, Rows             uint32
		WidthPixels, HeightPixels uint32
		Modes                     string
	}{"xterm", 24, 80, 0, 0, string(make([]byte, sshserver.DefaultLimits.MaxTerminalModesBytes+1))})
	ok, err := channel.SendRequest("pty-req", true, oversized)
	if err != nil {
		t.Fatalf("SendRequest(pty-req): %v", err)
	}
	if ok {
		t.Fatal("a pty request with oversized terminal modes was accepted")
	}
}

func TestPTYRequestWithUnsafeTerminalNameIsRefused(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// A TERM holding a control character must never reach a screen or an
	// environment variable.
	if err := session.RequestPty("xterm\x1b[31m", 24, 80, ssh.TerminalModes{}); err == nil {
		t.Fatal("RequestPty with a control character in TERM was accepted")
	}
}

func TestWindowChangeDeliversResize(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	session := startShell(t, client, "xterm", 80, 24)
	server := probe.await(t)
	assertResize(t, probe.awaitEvent(t).event, 80, 24) // the initial size

	// WindowChange takes (rows, columns).
	if err := session.WindowChange(50, 132); err != nil {
		t.Fatalf("WindowChange: %v", err)
	}
	assertResize(t, probe.awaitEvent(t).event, 132, 50)
	if got, want := server.Terminal().Cols, 132; got != want {
		t.Errorf("Terminal().Cols = %d, want %d", got, want)
	}
	if got, want := server.Terminal().Rows, 50; got != want {
		t.Errorf("Terminal().Rows = %d, want %d", got, want)
	}
}

func TestEnvRequestsAreAcceptedOnlyForLocaleVariables(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	channel, _ := openRawSession(t, client)

	accepted := []struct{ name, value string }{
		{"LANG", "en_US.UTF-8"},
		{"LC_ALL", "C.UTF-8"},
		{"LANGUAGE", "en"},
	}
	for _, entry := range accepted {
		ok, err := channel.SendRequest("env", true, ssh.Marshal(struct{ Name, Value string }{entry.name, entry.value}))
		if err != nil {
			t.Fatalf("env %s: %v", entry.name, err)
		}
		if !ok {
			t.Errorf("env %s was refused but is a locale variable", entry.name)
		}
	}
	refused := []string{"LD_PRELOAD", "PATH", "SSH_AUTH_SOCK", "TERM_PROGRAM", "VIBESHELL_SECRET"}
	for _, name := range refused {
		ok, err := channel.SendRequest("env", true, ssh.Marshal(struct{ Name, Value string }{name, "value"}))
		if err != nil {
			t.Fatalf("env %s: %v", name, err)
		}
		if ok {
			t.Errorf("env %s was accepted but is not a locale variable", name)
		}
	}
	shellOK, err := channel.SendRequest("shell", true, nil)
	if err != nil || !shellOK {
		t.Fatalf("shell request after env requests: ok=%t err=%v", shellOK, err)
	}
	server := probe.await(t)
	for _, entry := range accepted {
		got, ok := server.Env(entry.name)
		if !ok || got != entry.value {
			t.Errorf("session Env(%q) = %q,%t want %q", entry.name, got, ok, entry.value)
		}
	}
	if _, ok := server.Env("LD_PRELOAD"); ok {
		t.Error("LD_PRELOAD reached the session environment")
	}
}

func TestEnvValueWithControlCharacterIsRefused(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	channel, _ := openRawSession(t, client)
	ok, err := channel.SendRequest("env", true, ssh.Marshal(struct{ Name, Value string }{"LANG", "en\x1b[31m"}))
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if ok {
		t.Fatal("an env value containing a control character was accepted")
	}
}

func TestSignalRequestsAreDeliveredAndBounded(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")
	session := startShell(t, client, "xterm", 80, 24)
	probe.await(t)

	// Signal uses the client's own encoder for the accepted case.
	if err := session.Signal(ssh.SIGINT); err != nil {
		t.Fatalf("signal INT: %v", err)
	}
	select {
	case got := <-probe.signals:
		if got != "INT" {
			t.Fatalf("signal = %q, want INT", got)
		}
	case <-time.After(dialTimeout):
		t.Fatal("INT never reached the session")
	}

	// The refusal closes the channel, so the client may see an error instead of
	// a rejection; either way USR1 must not be accepted.
	ok, err := session.SendRequest("signal", true, ssh.Marshal(struct{ Name string }{"USR1"}))
	if err == nil && ok {
		t.Fatal("signal USR1 was accepted")
	}
}

func TestExitStatusIsSentOnceAndReportedToTheClient(t *testing.T) {
	ready := make(chan *sshserver.Session, 1)
	handler := func(session *sshserver.Session) {
		ready <- session
		<-session.Context().Done()
	}
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: handler})
	client := h.dial(t, "ada")
	session := startShell(t, client, "xterm", 80, 24)

	server := <-ready
	if err := server.Exit(7); err != nil {
		t.Fatalf("first Exit: %v", err)
	}
	if err := server.Exit(9); err == nil {
		t.Fatal("a second Exit was accepted")
	}
	err := session.Wait()
	if err == nil {
		t.Fatal("Wait returned no error, want the exit status")
	}
	var exitErr *ssh.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("Wait error = %v, want an exit status", err)
	}
	if got := exitErr.ExitStatus(); got != 7 {
		t.Fatalf("exit status = %d, want 7", got)
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestChannelRequestsThatAreRefused(t *testing.T) {
	for _, testCase := range []struct {
		request  string
		payload  []byte
		contains string
	}{
		{request: "exec", payload: ssh.Marshal(struct{ Command string }{"ls -la"}), contains: "exec is not supported"},
		{request: "subsystem", payload: ssh.Marshal(struct{ Name string }{"sftp"}), contains: "subsystem is not supported"},
		{request: "x11-req", payload: nil, contains: "x11 forwarding is not supported"},
		{request: "auth-agent-req@openssh.com", payload: nil, contains: "agent forwarding is not supported"},
		{request: "eow@openssh.com", payload: nil, contains: "is not supported"},
		{request: "hostkeys-00@openssh.com", payload: nil, contains: "is not supported"},
		{request: "keepalive", payload: nil, contains: "is not supported"},
	} {
		t.Run(testCase.request, func(t *testing.T) {
			probe := newProbe()
			h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
			client := h.dial(t, "ada")
			channel, stderr := openRawSession(t, client)

			ok, err := channel.SendRequest(testCase.request, true, testCase.payload)
			if err != nil {
				t.Fatalf("SendRequest(%s): %v", testCase.request, err)
			}
			if ok {
				t.Fatalf("%s was accepted", testCase.request)
			}
			// The client must see the reason on its error stream, because a plain
			// `ssh host command` user does not run with -v. The SSH failure
			// message payload is only visible to an OpenSSH client run with -v;
			// the real-client receipt checks that too.
			got := waitForText(t, stderr.String, testCase.contains)
			if !strings.Contains(got, testCase.contains) {
				t.Errorf("stderr = %q, want it to mention %q", got, testCase.contains)
			}
		})
	}
}

func TestChannelTypesThatAreRefused(t *testing.T) {
	for _, channelType := range []string{"direct-tcpip", "forwarded-tcpip", "x11", "auth-agent@openssh.com"} {
		t.Run(channelType, func(t *testing.T) {
			probe := newProbe()
			h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
			client := h.dial(t, "ada")
			channel, requests, err := client.OpenChannel(channelType, nil)
			if err == nil {
				_ = channel.Close()
				t.Fatalf("channel type %q was accepted", channelType)
			}
			if requests != nil {
				t.Error("a refused channel still delivered a request stream")
			}
			if !strings.Contains(err.Error(), "not supported") {
				t.Errorf("refusal = %q, want it to say the channel type is not supported", err)
			}
		})
	}
}

func TestConnectionLevelRequestsAreRefused(t *testing.T) {
	probe := newProbe()
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: probe.handler})
	client := h.dial(t, "ada")

	// tcpip-forward is what `ssh -L` and `ssh -R` send. Forwarding is not part
	// of the contract, so the advertisement must be refused.
	forward := struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 8080}
	for _, request := range []string{"tcpip-forward", "cancel-tcpip-forward"} {
		ok, message, err := client.SendRequest(request, true, ssh.Marshal(forward))
		if err != nil {
			t.Fatalf("%s: %v", request, err)
		}
		if ok {
			t.Fatalf("%s was accepted", request)
		}
		if !strings.Contains(string(message), "not supported") {
			t.Errorf("%s refusal = %q, want it to say the request is not supported", request, message)
		}
	}
}

// ---------------------------------------------------------------------------
// Session isolation, cancellation, and back pressure
// ---------------------------------------------------------------------------

func TestChannelsOnOneConnectionKeepIndependentState(t *testing.T) {
	var (
		mu        sync.Mutex
		terminals = map[*sshserver.Session]sshserver.Terminal{}
		runes     = map[*sshserver.Session][]rune{}
		ready     = make(chan *sshserver.Session, 4)
	)
	handler := func(session *sshserver.Session) {
		mu.Lock()
		terminals[session] = session.Terminal()
		mu.Unlock()
		ready <- session
		for {
			select {
			case <-session.Context().Done():
				return
			case <-session.InputClosed():
				_ = session.Exit(0)
				return
			case event := <-session.Events():
				if key, ok := event.(termdecode.Key); ok && key.Name == termdecode.KeyRune {
					mu.Lock()
					runes[session] = append(runes[session], key.Rune)
					mu.Unlock()
				}
			}
		}
	}
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: handler})
	client := h.dial(t, "ada")

	first := startShell(t, client, "xterm", 80, 24)
	second := startShell(t, client, "linux", 132, 43)
	firstServer := <-ready
	secondServer := <-ready
	if firstServer == secondServer {
		t.Fatal("both channels shared one session")
	}

	if _, err := first.stdin.Write([]byte("abc")); err != nil {
		t.Fatalf("write to first channel: %v", err)
	}
	if _, err := second.stdin.Write([]byte("xyz")); err != nil {
		t.Fatalf("write to second channel: %v", err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(runes[firstServer]) >= 3 && len(runes[secondServer]) >= 3
	}, "both channels to receive their input")

	mu.Lock()
	defer mu.Unlock()
	if got := string(runes[firstServer]); got != "abc" {
		t.Errorf("first channel received %q, want abc", got)
	}
	if got := string(runes[secondServer]); got != "xyz" {
		t.Errorf("second channel received %q, want xyz", got)
	}
	if got := terminals[firstServer]; got.Name != "xterm" || got.Cols != 80 || got.Rows != 24 {
		t.Errorf("first channel terminal = %s, want xterm 80x24", got)
	}
	if got := terminals[secondServer]; got.Name != "linux" || got.Cols != 132 || got.Rows != 43 {
		t.Errorf("second channel terminal = %s, want linux 132x43", got)
	}
}

func TestDisconnectCancelsTheSessionContextPromptly(t *testing.T) {
	const bound = 2 * time.Second
	ready := make(chan struct{})
	cancelled := make(chan struct{})
	handler := func(session *sshserver.Session) {
		close(ready)
		<-session.Context().Done()
		close(cancelled)
	}
	h := start(t, sshserver.Options{Mode: sshserver.ModePublic, ShellHandler: handler})
	client := h.dial(t, "ada")
	_ = startShell(t, client, "xterm", 80, 24)
	<-ready

	closedAt := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	select {
	case <-cancelled:
		elapsed := time.Since(closedAt)
		if elapsed > bound {
			t.Fatalf("session context was cancelled %s after the client closed, bound is %s", elapsed, bound)
		}
		t.Logf("session context cancelled %s after the client closed", elapsed)
	case <-time.After(30 * time.Second):
		t.Fatal("session context was never cancelled")
	}
}

func TestSlowClientDoesNotBlockAnotherSession(t *testing.T) {
	const (
		slowOutput   = 8 << 20
		outputLimit  = 64 << 10
		settle       = 500 * time.Millisecond
		bound        = 10 * time.Second
		slowUsername = "slowpoke"
	)
	slowWritten := make(chan int64, 1)
	fastEvents := make(chan observed, 64)

	handler := func(session *sshserver.Session) {
		if session.Principal().Username == slowUsername {
			go func() {
				payload := bytes.Repeat([]byte("s"), 32<<10)
				for written := 0; written < slowOutput; written += len(payload) {
					if _, err := session.Write(payload); err != nil {
						return
					}
				}
				slowWritten <- session.DroppedBytes()
			}()
			<-session.Context().Done()
			return
		}
		for {
			select {
			case <-session.Context().Done():
				return
			case <-session.InputClosed():
				_ = session.Exit(0)
				return
			case event := <-session.Events():
				select {
				case fastEvents <- observed{session: session, event: event}:
				default:
				}
			}
		}
	}
	h := start(t, sshserver.Options{
		Mode:         sshserver.ModePublic,
		Limits:       sshserver.Limits{MaxOutputQueueBytes: outputLimit},
		ShellHandler: handler,
	})

	// The slow client opens a session and never reads it.
	slowClient := h.dial(t, slowUsername)
	slowSession := startShell(t, slowClient, "xterm", 80, 24)
	_ = slowSession
	time.Sleep(settle)

	// A second connection to the same server must be unaffected.
	fastClient := h.dial(t, "responsive")
	fastSession := startShell(t, fastClient, "xterm", 80, 24)
	if _, err := fastSession.stdin.Write([]byte("ok\r")); err != nil {
		t.Fatalf("write to the responsive session: %v", err)
	}
	// awaitRune waits for the first key press, ignoring the initial size.
	awaitRune := func() {
		t.Helper()
		deadline := time.After(bound)
		for {
			select {
			case got := <-fastEvents:
				key, ok := got.event.(termdecode.Key)
				if !ok || key.Name != termdecode.KeyRune {
					continue
				}
				if key.Rune != 'o' {
					t.Fatalf("responsive session received %q first, want the typed rune", key.Rune)
				}
				return
			case <-deadline:
				t.Fatal("a second connection was blocked by the client that stopped reading")
			}
		}
	}
	awaitRune()
	fastSession.eof()
	if err := fastSession.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// The slow session's writer must finish too: a bounded queue drops rather
	// than blocks.
	select {
	case dropped := <-slowWritten:
		if dropped == 0 {
			t.Error("the slow session dropped no bytes; the bounded queue was never exercised")
		}
		t.Logf("slow session wrote %d bytes with a %d byte queue and dropped %d", slowOutput, outputLimit, dropped)
	case <-time.After(bound):
		t.Fatal("the slow session's writer blocked instead of dropping output")
	}
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

func asExitError(err error, target **ssh.ExitError) bool {
	for err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok { //nolint:errorlint // the client returns this type directly
			*target = exitErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func assertKey(t *testing.T, event termdecode.Event, name termdecode.KeyName, r rune) {
	t.Helper()
	key, ok := event.(termdecode.Key)
	if !ok {
		t.Fatalf("event is %s, want a key", describe(event))
	}
	if key.Name != name {
		t.Fatalf("key name = %s, want %s", key.Name, name)
	}
	if name == termdecode.KeyRune && key.Rune != r {
		t.Fatalf("key rune = %q, want %q", key.Rune, r)
	}
}

func assertResize(t *testing.T, event termdecode.Event, cols, rows int) {
	t.Helper()
	resize, ok := event.(termdecode.Resize)
	if !ok {
		t.Fatalf("event is %s, want a resize", describe(event))
	}
	if resize.Cols != cols || resize.Rows != rows {
		t.Fatalf("resize = %dx%d, want %dx%d", resize.Cols, resize.Rows, cols, rows)
	}
}

// waitForText polls read until it contains want, returning the last text read.
func waitForText(t *testing.T, read func() string, want string) string {
	t.Helper()
	deadline := time.Now().Add(dialTimeout)
	got := read()
	for time.Now().Before(deadline) && !strings.Contains(got, want) {
		time.Sleep(2 * time.Millisecond)
		got = read()
	}
	return got
}

// waitFor polls condition until it holds.
func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(dialTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
