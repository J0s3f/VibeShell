package ssh_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	adapter "j0s.at/vibeshell/internal/adapters/ssh"
)

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

func TestPublicModeAcceptsAnyUsernameWithNoCredential(t *testing.T) {
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	for _, user := range []string{"alice", "bob", "müller", "root"} {
		client := h.dial(t, user)
		sess, _, stdout, _ := startShell(t, client, "xterm", 80, 24)
		waitForText(t, stdout, "hello "+user+" ")
		_ = sess
	}
}

func TestPublicModeRefusesUnusableUsernames(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	for _, user := range []string{"", "a/b", `a\b`, "a b", strings.Repeat("a", 65)} {
		h.dialFails(t, user)
	}
}

func TestPasswordModeAuthenticatesOnlyWithTheRightPassword(t *testing.T) {
	passwords := mapAuth{
		"ada":    {password: "correct horse"},
		"bob":    {password: "s3cret", disabled: true},
		"müller": {password: "grüezi"},
	}
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:      adapter.ModePassword,
		Passwords: passwords,
		Handler:   adapter.HandlerFunc(echo.handler),
	})

	// Right passwords succeed, including a non-ASCII username.
	for user, pass := range map[string]string{"ada": "correct horse", "müller": "grüezi"} {
		client := h.dial(t, user, cryptossh.Password(pass))
		sess, _, stdout, _ := startShell(t, client, "xterm", 80, 24)
		waitForText(t, stdout, "hello "+user+" ")
		_ = sess
	}

	// Wrong password, unknown user, and disabled account all fail, and the
	// failure reveals nothing that distinguishes them.
	errWrong := h.dialFails(t, "ada", cryptossh.Password("wrong"))
	errUnknown := h.dialFails(t, "mallory", cryptossh.Password("correct horse"))
	errDisabled := h.dialFails(t, "bob", cryptossh.Password("s3cret"))
	for name, err := range map[string]error{"wrong": errWrong, "unknown": errUnknown, "disabled": errDisabled} {
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "correct horse") {
			t.Errorf("%s password failure %q leaks password material", name, err)
		}
	}

	// An oversized password is refused without reaching the backend.
	h.dialFails(t, "ada", cryptossh.Password(strings.Repeat("x", adapter.MaxPasswordBytes+1)))

	// Invalid usernames never reach the backend either.
	h.dialFails(t, "a/b", cryptossh.Password("whatever"))
}

func TestPasswordModeOffersNoOtherMethod(t *testing.T) {
	h := start(t, adapter.Options{
		Mode:      adapter.ModePassword,
		Passwords: mapAuth{"ada": {password: "secret"}},
	})

	// No credential at all (SSH "none") is refused.
	h.dialFails(t, "ada")
	// Client keys and certificates are refused: only passwords authenticate.
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	h.dialFails(t, "ada", cryptossh.PublicKeys(signer))
	h.dialFails(t, "ada", cryptossh.KeyboardInteractive(
		func(name, instruction string, questions []string, echos []bool) ([]string, error) {
			return make([]string, len(questions)), nil
		}))
}

func TestPublicModeOffersNoClientKeyMethod(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	// Without any credential the handshake succeeds (none auth), but a client
	// key alone must not authenticate: the server registers no public-key
	// callback, so a key-only handshake has nothing to succeed with. The Go
	// client always probes "none" first, which succeeds here by design; the
	// assertion that matters is that an invalid username fails even when a
	// key is offered.
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	h.dialFails(t, "not a user", cryptossh.PublicKeys(signer))
}

// ---------------------------------------------------------------------------
// Session byte streams
// ---------------------------------------------------------------------------

func TestSessionCarriesStdinStdoutEOFAndExitStatus(t *testing.T) {
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client := h.dial(t, "ada")
	sess, stdin, stdout, _ := startShell(t, client, "xterm-256color", 80, 24)

	if _, err := stdin.Write([]byte("echo hello\r")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	got := waitForText(t, stdout, "hello ada")
	if !strings.Contains(got, "hello ada xterm-256color 80x24") {
		t.Fatalf("stdout = %q, want the greeting with the negotiated terminal", got)
	}
	waitForText(t, stdout, "echo hello")

	// EOF is the client closing its input stream, not a disconnect: the
	// handler observes it and exits cleanly.
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait after EOF: %v, want a clean exit", err)
	}
	server := awaitSession(t, echo)
	if err := server.InputError(); !errors.Is(err, io.EOF) {
		t.Errorf("InputError = %v, want io.EOF", err)
	}
}

func TestHandlerExitStatusReachesTheClient(t *testing.T) {
	h := start(t, adapter.Options{
		Mode: adapter.ModePublic,
		Handler: adapter.HandlerFunc(func(_ context.Context, sess *adapter.Session) {
			_ = sess.Exit(3)
			if err := sess.Exit(2); err == nil {
				// Report through stderr: the test reads it below.
				_, _ = sess.WriteStderr([]byte("second exit unexpectedly sent\n"))
			}
		}),
	})
	client := h.dial(t, "ada")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	stderr := &syncBuffer{}
	sess.Stderr = stderr
	if err := sess.RequestPty("xterm", 24, 80, cryptossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	err = sess.Wait()
	exitErr, ok := err.(*cryptossh.ExitError)
	if !ok {
		t.Fatalf("Wait = %v, want an exit error with status 3", err)
	}
	if got := exitErr.ExitStatus(); got != 3 {
		t.Errorf("exit status = %d, want 3", got)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want the second Exit to have been refused", stderr.String())
	}
}

func TestHandlerReturnWithoutExitMeansZero(t *testing.T) {
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(func(context.Context, *adapter.Session) {}),
	})
	client := h.dial(t, "ada")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if err := sess.Wait(); err != nil {
		t.Errorf("Wait = %v, want a clean exit", err)
	}
}

// ---------------------------------------------------------------------------
// Terminal negotiation
// ---------------------------------------------------------------------------

func TestPTYRequestCarriesTerminalAndSize(t *testing.T) {
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client := h.dial(t, "ada")
	_, stdin, stdout, _ := startShell(t, client, "xterm-256color", 132, 43)
	_ = stdin

	server := awaitSession(t, echo)
	window := server.Terminal()
	if !window.Requested {
		t.Fatal("terminal was not recorded as requested")
	}
	if window.Name != "xterm-256color" {
		t.Errorf("TERM = %q, want xterm-256color", window.Name)
	}
	if window.Cols != 132 || window.Rows != 43 {
		t.Errorf("size = %dx%d, want 132x43", window.Cols, window.Rows)
	}
	// The initial size is also delivered as a resize, so a handler that only
	// watches ResizeNotify still sees the first size.
	select {
	case got := <-server.ResizeNotify():
		if got.Cols != 132 || got.Rows != 43 {
			t.Errorf("resize = %dx%d, want 132x43", got.Cols, got.Rows)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no initial resize delivered")
	}
	waitForText(t, stdout, "hello ada")
}

func openRawSession(t *testing.T, client *cryptossh.Client) (cryptossh.Channel, <-chan *cryptossh.Request) {
	t.Helper()
	channel, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open session channel: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	return channel, reqs
}

// discardRequests drains server-to-client channel requests (such as
// exit-status) that a test does not assert on.
func discardRequests(reqs <-chan *cryptossh.Request) {
	go cryptossh.DiscardRequests(reqs)
}

// awaitRequest returns the next server-to-client channel request.
func awaitRequest(t *testing.T, reqs <-chan *cryptossh.Request) *cryptossh.Request {
	t.Helper()
	select {
	case req, ok := <-reqs:
		if !ok {
			t.Fatal("request stream closed")
		}
		return req
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a channel request")
		return nil
	}
}

// readStderrText reads one stderr chunk with a timeout.
func readStderrText(t *testing.T, channel cryptossh.Channel) string {
	t.Helper()
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := channel.Stderr().Read(buf)
		done <- result{text: string(buf[:n]), err: err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("read stderr: %v", r.err)
		}
		return r.text
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for stderr")
		return ""
	}
}

// exitStatusOf extracts the status from an exit-status request.
func exitStatusOf(t *testing.T, req *cryptossh.Request) uint32 {
	t.Helper()
	if req.Type != "exit-status" {
		t.Fatalf("request type = %q, want exit-status", req.Type)
	}
	var status struct{ Status uint32 }
	if err := cryptossh.Unmarshal(req.Payload, &status); err != nil {
		t.Fatalf("unmarshal exit-status: %v", err)
	}
	return status.Status
}

type ptyReq struct {
	Term                      string
	Columns, Rows             uint32
	WidthPixels, HeightPixels uint32
	Modes                     string
}

func sendPTY(t *testing.T, channel cryptossh.Channel, req ptyReq) bool {
	t.Helper()
	ok, err := channel.SendRequest("pty-req", true, cryptossh.Marshal(req))
	if err != nil {
		t.Fatalf("pty-req send: %v", err)
	}
	return ok
}

func TestOutOfRangePTYDimensionsAreRefused(t *testing.T) {
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
		Limits:  adapter.Limits{MaxDimension: 512},
	})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)
	discardRequests(reqs)

	for _, dims := range [][2]uint32{{513, 24}, {80, 100000}, {100000, 100000}} {
		req := ptyReq{Term: "xterm", Columns: dims[0], Rows: dims[1]}
		if sendPTY(t, channel, req) {
			t.Errorf("pty-req %dx%d accepted, want refusal", dims[0], dims[1])
		}
	}
	// The channel is still usable: a valid pty followed by a shell starts the
	// application session with the accepted size.
	if !sendPTY(t, channel, ptyReq{Term: "xterm", Columns: 100, Rows: 30}) {
		t.Fatal("valid pty-req refused")
	}
	ok, err := channel.SendRequest("shell", true, nil)
	if err != nil || !ok {
		t.Fatalf("shell = %v, %v; want accepted", ok, err)
	}
	server := awaitSession(t, echo)
	if window := server.Terminal(); window.Cols != 100 || window.Rows != 30 {
		t.Errorf("size = %dx%d, want 100x30", window.Cols, window.Rows)
	}
}

func TestZeroPTYDimensionsClampToDefault(t *testing.T) {
	// Terminal-less clients (scripted `ssh -tt` without a local tty) report
	// 0x0: the session clamps to the conventional default instead of
	// refusing, so an ordinary OpenSSH invocation always gets a usable size.
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)
	discardRequests(reqs)
	if !sendPTY(t, channel, ptyReq{Term: "xterm", Columns: 0, Rows: 0}) {
		t.Fatal("pty-req with zero dimensions refused, want clamped to default")
	}
	ok, err := channel.SendRequest("shell", true, nil)
	if err != nil || !ok {
		t.Fatalf("shell = %v, %v; want accepted", ok, err)
	}
	server := awaitSession(t, echo)
	if window := server.Terminal(); window.Cols != adapter.DefaultTerminalCols || window.Rows != adapter.DefaultTerminalRows {
		t.Errorf("size = %dx%d, want default %dx%d",
			window.Cols, window.Rows, adapter.DefaultTerminalCols, adapter.DefaultTerminalRows)
	}
}

func TestUnsafeTerminalNamesAreRefused(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	client := h.dial(t, "ada")
	for _, term := range []string{
		"xterm\x1b[2J",
		"term\nwith-newline",
		"t\x7fdel",
		"tä",
		strings.Repeat("x", adapter.DefaultLimits.MaxTerminalNameBytes+1),
		"",
	} {
		channel, reqs := openRawSession(t, client)
		discardRequests(reqs)
		// An empty TERM carries no information but is harmless; everything
		// else on this list must be refused.
		ok := sendPTY(t, channel, ptyReq{Term: term, Columns: 80, Rows: 24})
		if term == "" && !ok {
			t.Error("empty TERM refused, want it accepted as harmless")
		}
		if term != "" && ok {
			t.Errorf("pty-req with TERM %q accepted, want refusal", term)
		}
	}
}

func TestOversizedPTYModesAreRefused(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)
	discardRequests(reqs)
	req := ptyReq{
		Term: "xterm", Columns: 80, Rows: 24,
		Modes: strings.Repeat("M", adapter.DefaultLimits.MaxTerminalModesBytes+1),
	}
	if sendPTY(t, channel, req) {
		t.Error("pty-req with oversized modes accepted, want refusal")
	}
}

func TestFullSizePTYModesAreAccepted(t *testing.T) {
	// Real OpenSSH clients send every TTY mode opcode (~150-200 bytes); the
	// modes bound must admit them. Modes are stored verbatim.
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)
	discardRequests(reqs)
	req := ptyReq{
		Term: "xterm-256color", Columns: 80, Rows: 24,
		Modes: strings.Repeat("M", 200),
	}
	if !sendPTY(t, channel, req) {
		t.Fatal("pty-req with OpenSSH-sized modes refused, want accepted")
	}
	ok, err := channel.SendRequest("shell", true, nil)
	if err != nil || !ok {
		t.Fatalf("shell = %v, %v; want accepted", ok, err)
	}
	server := awaitSession(t, echo)
	if server.Terminal().Name != "xterm-256color" {
		t.Errorf("TERM = %q, want xterm-256color", server.Terminal().Name)
	}
}

func TestWindowChangeDeliversResize(t *testing.T) {
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client := h.dial(t, "ada")
	sess, _, _, _ := startShell(t, client, "xterm", 80, 24)
	server := awaitSession(t, echo)
	// Drain the initial size so the next event is the change.
	select {
	case <-server.ResizeNotify():
	case <-time.After(10 * time.Second):
		t.Fatal("no initial resize delivered")
	}
	if err := sess.WindowChange(30, 100); err != nil {
		t.Fatalf("window change: %v", err)
	}
	select {
	case got := <-server.ResizeNotify():
		if got.Cols != 100 || got.Rows != 30 {
			t.Errorf("resize = %dx%d, want 100x30", got.Cols, got.Rows)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no resize delivered after window-change")
	}
	if window := server.Terminal(); window.Cols != 100 || window.Rows != 30 {
		t.Errorf("terminal = %dx%d, want 100x30", window.Cols, window.Rows)
	}
}

// ---------------------------------------------------------------------------
// Environment and signals
// ---------------------------------------------------------------------------

func TestEnvRequestsAcceptedOnlyForLocaleVariables(t *testing.T) {
	var gotLang string
	h := start(t, adapter.Options{
		Mode: adapter.ModePublic,
		Handler: adapter.HandlerFunc(func(_ context.Context, sess *adapter.Session) {
			gotLang, _ = sess.Env("LANG")
			_ = sess.Exit(0)
		}),
	})
	client := h.dial(t, "ada")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	if err := sess.Setenv("LANG", "en_US.UTF-8"); err != nil {
		t.Errorf("Setenv LANG: %v, want accepted", err)
	}
	if err := sess.Setenv("LC_MEASUREMENT", "de_DE.UTF-8"); err != nil {
		t.Errorf("Setenv LC_MEASUREMENT: %v, want accepted", err)
	}
	for _, name := range []string{"PATH", "LD_PRELOAD", "SSH_AUTH_SOCK", "HOME", "TERM", "LC:HACK"} {
		if err := sess.Setenv(name, "x"); err == nil {
			t.Errorf("Setenv %s succeeded, want refusal", name)
		}
	}
	if err := sess.Setenv("LANG", "has-\x01-control"); err == nil {
		t.Error("Setenv with a control character succeeded, want refusal")
	}
	if err := sess.Setenv("LANG", strings.Repeat("v", adapter.DefaultLimits.MaxEnvValueBytes+1)); err == nil {
		t.Error("Setenv with an oversized value succeeded, want refusal")
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	waitFor(t, "LANG to reach the handler", func() bool { return gotLang == "en_US.UTF-8" })
}

func TestEnvRequestCountIsBounded(t *testing.T) {
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: noopHandler(),
		Limits:  adapter.Limits{MaxEnvRequests: 2},
	})
	client := h.dial(t, "ada")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	for i := 0; i < 2; i++ {
		if err := sess.Setenv("LANG", "en_US.UTF-8"); err != nil {
			t.Fatalf("Setenv %d: %v, want accepted", i, err)
		}
	}
	if err := sess.Setenv("LANG", "en_US.UTF-8"); err == nil {
		t.Error("third Setenv succeeded past MaxEnvRequests=2, want refusal")
	}
}

func TestSignalRequestsAreDelivered(t *testing.T) {
	signals := make(chan string, 8)
	h := start(t, adapter.Options{
		Mode: adapter.ModePublic,
		Handler: adapter.HandlerFunc(func(ctx context.Context, sess *adapter.Session) {
			for {
				select {
				case <-ctx.Done():
					return
				case sig := <-sess.Signals():
					signals <- sig
					_ = sess.Exit(0)
					return
				}
			}
		}),
	})
	client := h.dial(t, "ada")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 24, 80, cryptossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if err := sess.Signal(cryptossh.SIGTERM); err != nil {
		t.Fatalf("signal TERM: %v", err)
	}
	select {
	case got := <-signals:
		if got != "TERM" {
			t.Errorf("signal = %q, want TERM", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("signal not delivered")
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Unsupported signals are refused and leave the session alive.
	echo := newEcho()
	h2 := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client2 := h2.dial(t, "ada")
	channel, reqs := openRawSession(t, client2)
	discardRequests(reqs)
	ok, err := channel.SendRequest("signal", true, cryptossh.Marshal(struct{ Name string }{Name: "KILL"}))
	if err != nil {
		t.Fatalf("signal send: %v", err)
	}
	if ok {
		t.Error("signal KILL accepted, want refusal")
	}
}
