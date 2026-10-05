package ssh_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	adapter "j0s.at/vibeshell/internal/adapters/ssh"
)

// mapAuth is the test double for the application-side password backend (owned
// by task B08 in production). Unknown users, wrong passwords, and disabled
// accounts all produce the same wrapped error so a client cannot distinguish
// them, and the error text never contains the password.
type mapAuth map[string]mapAuthEntry

type mapAuthEntry struct {
	password string
	disabled bool
}

func (m mapAuth) AuthenticatePassword(username, password string) (adapter.Principal, error) {
	entry, ok := m[username]
	if !ok || entry.disabled || entry.password != password {
		return adapter.Principal{}, fmt.Errorf("%w: bad credentials", adapter.ErrUnauthenticated)
	}
	return adapter.Principal{Username: username}, nil
}

// harness runs one Server on a loopback listener for one test.
type harness struct {
	addr string
}

func start(t *testing.T, opts adapter.Options) *harness {
	t.Helper()
	if opts.Handler == nil {
		opts.Handler = adapter.HandlerFunc(func(context.Context, *adapter.Session) {})
	}
	server, err := adapter.NewServer(opts)
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
	return &harness{addr: listener.Addr().String()}
}

func (h *harness) clientConfig(username string, auth ...cryptossh.AuthMethod) *cryptossh.ClientConfig {
	return &cryptossh.ClientConfig{
		User: username,
		Auth: auth,
		// Every test server generates an ephemeral host key; no test may
		// depend on its value. Persistence is covered in hostkey_test.go.
		HostKeyCallback: cryptossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
}

// dial connects a client. An empty auth slice models a client that offers no
// credential at all, which is what public mode must accept.
func (h *harness) dial(t *testing.T, username string, auth ...cryptossh.AuthMethod) *cryptossh.Client {
	t.Helper()
	client, err := cryptossh.Dial("tcp", h.addr, h.clientConfig(username, auth...))
	if err != nil {
		t.Fatalf("dial as %q: %v", username, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// dialFails asserts the server refuses the handshake.
func (h *harness) dialFails(t *testing.T, username string, auth ...cryptossh.AuthMethod) error {
	t.Helper()
	client, err := cryptossh.Dial("tcp", h.addr, h.clientConfig(username, auth...))
	if err == nil {
		_ = client.Close()
		t.Fatalf("dial as %q succeeded but had to be refused", username)
	}
	return err
}

// echoHandler is the stand-in for the application coordinator: it greets the
// session, echoes stdin back to stdout until EOF, and exits 0. Started
// sessions are reported so tests can inspect terminal negotiation.
type echoHandler struct {
	started chan *adapter.Session
}

func newEcho() *echoHandler {
	return &echoHandler{started: make(chan *adapter.Session, 32)}
}

func (e *echoHandler) handler(_ context.Context, sess *adapter.Session) {
	select {
	case e.started <- sess:
	default:
	}
	fmt.Fprintf(sess, "hello %s %s\n", sess.Principal().Username, sess.Terminal())
	buf := make([]byte, 4096)
	for {
		n, err := sess.Read(buf)
		if n > 0 {
			if _, werr := sess.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			_ = sess.Exit(0)
			return
		}
	}
}

func awaitSession(t *testing.T, e *echoHandler) *adapter.Session {
	t.Helper()
	select {
	case sess := <-e.started:
		return sess
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not start")
		return nil
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for captured streams.
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

func startShell(t *testing.T, client *cryptossh.Client, term string, cols, rows int) (*cryptossh.Session, io.WriteCloser, *syncBuffer, *syncBuffer) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.RequestPty(term, rows, cols, cryptossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	sess.Stdout = stdout
	sess.Stderr = stderr
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	return sess, stdin, stdout, stderr
}

// waitFor polls until cond holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForText(t *testing.T, buf *syncBuffer, fragment string) string {
	t.Helper()
	var got string
	waitFor(t, "output containing "+fragment, func() bool {
		got = buf.String()
		return strings.Contains(got, fragment)
	})
	return got
}
