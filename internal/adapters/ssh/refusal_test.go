package ssh_test

import (
	"context"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	adapter "j0s.at/vibeshell/internal/adapters/ssh"
)

// ---------------------------------------------------------------------------
// Refused session kinds: exec and subsystem
// ---------------------------------------------------------------------------

func TestExecIsRefusedWithStatus127(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)

	// The high-level Session API reports a refused exec as a start failure,
	// so the contract is asserted at the channel level: the request is
	// refused, the reason reaches stderr, and the channel ends with 127.
	ok, err := channel.SendRequest("exec", true, cryptossh.Marshal(struct{ Command string }{
		Command: "echo pwned",
	}))
	if err != nil {
		t.Fatalf("exec send: %v", err)
	}
	if ok {
		t.Fatal("exec accepted, want refusal")
	}
	if got := readStderrText(t, channel); !strings.Contains(got, "exec is not supported") {
		t.Errorf("stderr = %q, want the refusal reason", got)
	}
	if got := exitStatusOf(t, awaitRequest(t, reqs)); got != 127 {
		t.Errorf("exit status = %d, want 127", got)
	}
	// The channel is over: reads report EOF.
	if _, err := channel.Read(make([]byte, 1)); err == nil {
		t.Error("read after refused exec = nil, want EOF")
	}
}

func TestSubsystemIsRefused(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)
	ok, err := channel.SendRequest("subsystem", true, cryptossh.Marshal(struct{ Name string }{
		Name: "sftp",
	}))
	if err != nil {
		t.Fatalf("subsystem send: %v", err)
	}
	if ok {
		t.Fatal("subsystem accepted, want refusal")
	}
	// The server reports the refusal status before closing the channel.
	if got := exitStatusOf(t, awaitRequest(t, reqs)); got != 127 {
		t.Errorf("exit status = %d, want 127", got)
	}
}

// ---------------------------------------------------------------------------
// Refusals that keep the session alive
// ---------------------------------------------------------------------------

func TestNonSessionChannelTypesAreRefused(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	client := h.dial(t, "ada")

	type directTCPIP struct {
		HostToConnect  string
		PortToConnect  uint32
		OriginatorIP   string
		OriginatorPort uint32
	}
	payload := cryptossh.Marshal(directTCPIP{
		HostToConnect: "example.com", PortToConnect: 80,
		OriginatorIP: "127.0.0.1", OriginatorPort: 9999,
	})
	if _, _, err := client.OpenChannel("direct-tcpip", payload); err == nil {
		t.Error("direct-tcpip channel opened, want refusal")
	}
	if _, _, err := client.OpenChannel("forwarded-tcpip", payload); err == nil {
		t.Error("forwarded-tcpip channel opened, want refusal")
	}
	if _, _, err := client.OpenChannel("x11", nil); err == nil {
		t.Error("x11 channel opened, want refusal")
	}
}

func TestForwardingGlobalRequestsAreRefused(t *testing.T) {
	h := start(t, adapter.Options{Mode: adapter.ModePublic})
	client := h.dial(t, "ada")
	for _, typ := range []string{
		"tcpip-forward", "cancel-tcpip-forward", "vibeshell-unknown-global",
	} {
		ok, _, err := client.SendRequest(typ, true, cryptossh.Marshal(struct {
			Addr string
			Port uint32
		}{Addr: "0.0.0.0", Port: 9999}))
		if err != nil {
			t.Fatalf("global request %s: %v", typ, err)
		}
		if ok {
			t.Errorf("global request %s accepted, want refusal", typ)
		}
	}
}

func TestForwardingChannelRequestsKeepTheShellAlive(t *testing.T) {
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)
	discardRequests(reqs)

	type x11Req struct {
		SingleConnection bool
		AuthProtocol     string
		AuthCookie       string
		ScreenNumber     uint32
	}
	if ok, _ := channel.SendRequest("x11-req", true, cryptossh.Marshal(x11Req{
		AuthProtocol: "MIT-MAGIC-COOKIE-1", AuthCookie: " Apu not a real cookie",
	})); ok {
		t.Error("x11-req accepted, want refusal")
	}
	if ok, _ := channel.SendRequest("auth-agent-req@openssh.com", true, nil); ok {
		t.Error("auth-agent-req accepted, want refusal")
	}
	if ok, _ := channel.SendRequest("vibeshell-unknown-request", true, nil); ok {
		t.Error("unknown channel request accepted, want refusal")
	}
	if ok, _ := channel.SendRequest("eow@openssh.com", true, nil); ok {
		t.Error("eow@openssh.com accepted, want refusal")
	}

	// Refusals did not kill the channel: a shell still starts a session.
	ok, err := channel.SendRequest("shell", true, nil)
	if err != nil || !ok {
		t.Fatalf("shell after refusals = %v, %v; want accepted", ok, err)
	}
	server := awaitSession(t, echo)
	if server.Principal().Username != "ada" {
		t.Errorf("session user = %q, want ada", server.Principal().Username)
	}
	// A second shell on the same channel is refused without killing it.
	if ok, _ := channel.SendRequest("shell", true, nil); ok {
		t.Error("second shell accepted, want refusal")
	}
	// So is an exec that arrives after the shell started: the live session
	// survives the refusal.
	if ok, _ := channel.SendRequest("exec", true, cryptossh.Marshal(struct{ Command string }{
		Command: "echo late",
	})); ok {
		t.Error("post-shell exec accepted, want refusal")
	}
	if _, err := channel.SendRequest("shell", true, nil); err != nil {
		t.Errorf("channel died after post-shell exec refusal: %v", err)
	}
}

func TestChannelWithoutShellNeverReachesTheHandler(t *testing.T) {
	reached := make(chan struct{}, 1)
	h := start(t, adapter.Options{
		Mode: adapter.ModePublic,
		Handler: adapter.HandlerFunc(func(context.Context, *adapter.Session) {
			reached <- struct{}{}
		}),
	})
	client := h.dial(t, "ada")
	channel, _, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open session channel: %v", err)
	}
	// Close without asking for a shell, then prove the server still works.
	if err := channel.Close(); err != nil {
		t.Fatalf("close channel: %v", err)
	}
	select {
	case <-reached:
		t.Fatal("handler ran without a shell request")
	case <-time.After(200 * time.Millisecond):
	}
	client2 := h.dial(t, "ada")
	sess, err := client2.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not run for the second channel")
	}
}

// ---------------------------------------------------------------------------
// Independent channels, cancellation, slow clients
// ---------------------------------------------------------------------------

func TestChannelsOnOneConnectionKeepIndependentState(t *testing.T) {
	echo := newEcho()
	h := start(t, adapter.Options{
		Mode:    adapter.ModePublic,
		Handler: adapter.HandlerFunc(echo.handler),
	})
	client := h.dial(t, "ada")

	sessA, stdinA, stdoutA, _ := startShell(t, client, "xterm", 80, 24)
	sessB, stdinB, stdoutB, _ := startShell(t, client, "xterm", 132, 43)
	_ = sessA
	_ = sessB

	serverA := awaitSession(t, echo)
	serverB := awaitSession(t, echo)
	if serverA == serverB {
		t.Fatal("both channels share one *Session, want independent sessions")
	}
	sizes := map[[2]int]bool{}
	for _, s := range []*adapter.Session{serverA, serverB} {
		w := s.Terminal()
		sizes[[2]int{w.Cols, w.Rows}] = true
	}
	if !sizes[[2]int{80, 24}] || !sizes[[2]int{132, 43}] {
		t.Errorf("terminal sizes = %v, want independent 80x24 and 132x43", sizes)
	}

	if _, err := stdinA.Write([]byte("marker-alpha\n")); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if _, err := stdinB.Write([]byte("marker-beta\n")); err != nil {
		t.Fatalf("write B: %v", err)
	}
	waitForText(t, stdoutA, "marker-alpha")
	waitForText(t, stdoutB, "marker-beta")
	if strings.Contains(stdoutA.String(), "marker-beta") {
		t.Errorf("channel A received channel B output: %q", stdoutA.String())
	}
	if strings.Contains(stdoutB.String(), "marker-alpha") {
		t.Errorf("channel B received channel A output: %q", stdoutB.String())
	}
}

func TestDisconnectCancelsTheSessionContext(t *testing.T) {
	cancelled := make(chan struct{})
	h := start(t, adapter.Options{
		Mode: adapter.ModePublic,
		Handler: adapter.HandlerFunc(func(ctx context.Context, sess *adapter.Session) {
			<-ctx.Done()
			close(cancelled)
		}),
	})
	client := h.dial(t, "ada")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	// A disconnect (not a clean EOF) ends the application session through
	// context cancellation.
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("session context not cancelled after disconnect")
	}
	// The server itself survives the disconnect.
	h.dial(t, "ada")
}

func TestSlowClientDropsOldestBytesWithoutBlocking(t *testing.T) {
	dropped := make(chan int64, 1)
	h := start(t, adapter.Options{
		Mode: adapter.ModePublic,
		Handler: adapter.HandlerFunc(func(_ context.Context, sess *adapter.Session) {
			// Far more than any channel window, so the pump must stall and
			// the bounded queue must drop. Write itself never blocks.
			blob := make([]byte, 4<<20)
			for i := range blob {
				blob[i] = 'x'
			}
			if _, err := sess.Write(blob); err != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
			dropped <- sess.DroppedBytes()
			_ = sess.Exit(0)
		}),
		Limits: adapter.Limits{MaxOutputQueueBytes: 4096, ExitDrainTimeout: 300 * time.Millisecond},
	})
	client := h.dial(t, "ada")
	channel, reqs := openRawSession(t, client)
	ok, err := channel.SendRequest("shell", true, nil)
	if err != nil || !ok {
		t.Fatalf("shell = %v, %v; want accepted", ok, err)
	}
	// Never read: the client stalls while the handler keeps running.
	select {
	case n := <-dropped:
		if n <= 0 {
			t.Errorf("DroppedBytes = %d, want > 0 for a stalled client", n)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("handler did not finish for a stalled client")
	}
	_ = reqs
}
