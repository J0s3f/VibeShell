package load

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

// Client is one interactive shell session over a real SSH connection. It is the
// harness's only way to drive load: every scenario talks to the shipped
// transport through it, so no internal function is called that a real terminal
// could not reach.
type Client struct {
	conn     *cryptossh.Client
	sess     *cryptossh.Session
	stdin    io.WriteCloser
	tap      *tapWriter
	username string
}

// Dial opens a connection and starts one shell channel with a PTY. The harness
// never authenticates: public mode accepts any valid username with no
// credential, which keeps the load test free of secrets.
func Dial(address, username string, cols, rows int) (*Client, error) {
	config := &cryptossh.ClientConfig{
		User: username,
		// Every harness stack generates an ephemeral host key, so no session
		// may depend on its value; persistence is covered by the SSH adapter's
		// own tests.
		HostKeyCallback: cryptossh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	conn, err := cryptossh.Dial("tcp", address, config)
	if err != nil {
		return nil, fmt.Errorf("dial %s as %s: %w", address, username, err)
	}
	sess, err := conn.NewSession()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open session: %w", err)
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, cryptossh.TerminalModes{}); err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}
	tap := newTapWriter()
	sess.Stdout = tap
	sess.Stderr = io.Discard
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	if err := sess.Shell(); err != nil {
		_ = stdin.Close()
		_ = sess.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	return &Client{conn: conn, sess: sess, stdin: stdin, tap: tap, username: username}, nil
}

// Username reports the username this client presented.
func (c *Client) Username() string { return c.username }

// Write sends raw client bytes, which is how a real terminal reaches the shell.
func (c *Client) Write(p []byte) error {
	_, err := c.stdin.Write(p)
	return err
}

// SubmitLine types a command line and waits for the prompt that follows its
// turn. The returned duration is the end-to-end local event latency for that
// input. It contains no provider time because the local engine contacts none.
//
// The wait counts prompts rather than matching a prompt frame: the SSH client
// copies channel data to stdout on its own goroutine, so a frame from the
// previous turn can arrive after this turn's timer started. Counting makes a
// stale frame unable to satisfy the current wait.
func (c *Client) SubmitLine(command string, timeout time.Duration) (time.Duration, error) {
	if err := c.Write([]byte(command)); err != nil {
		return 0, err
	}
	before := c.tap.promptCount()
	started := time.Now()
	if err := c.Write([]byte("\r")); err != nil {
		return 0, err
	}
	if err := c.tap.awaitPrompt(before, timeout); err != nil {
		return time.Since(started), err
	}
	return time.Since(started), nil
}

// Paste sends a bracketed paste and waits for the resulting prompt.
func (c *Client) Paste(payload string, timeout time.Duration) (time.Duration, error) {
	if err := c.Write([]byte("\x1b[200~")); err != nil {
		return 0, err
	}
	if err := c.Write([]byte(payload)); err != nil {
		return 0, err
	}
	before := c.tap.promptCount()
	started := time.Now()
	if err := c.Write([]byte("\x1b[201~")); err != nil {
		return 0, err
	}
	if err := c.tap.awaitPrompt(before, timeout); err != nil {
		return time.Since(started), err
	}
	return time.Since(started), nil
}

// TypeKeys sends raw keystrokes and waits for the echo that acknowledges them.
// Navigation keys start no turn, so the echo is the whole local round trip.
func (c *Client) TypeKeys(keys string, timeout time.Duration) (time.Duration, error) {
	started := time.Now()
	if err := c.Write([]byte(keys)); err != nil {
		return 0, err
	}
	if err := c.tap.awaitWrite(started, timeout); err != nil {
		return time.Since(started), err
	}
	return time.Since(started), nil
}

// Resize sends a real SSH window-change request and reports how long the write
// took. A shell legitimately answers a resize with no terminal output, so the
// application-side acceptance is proven by the handler's resize counter rather
// than by a byte on the wire.
func (c *Client) Resize(cols, rows int) (time.Duration, error) {
	started := time.Now()
	if err := c.sess.WindowChange(rows, cols); err != nil {
		return time.Since(started), err
	}
	return time.Since(started), nil
}

// Keepalive sends an SSH-level keepalive request. The VibeShell contract
// refuses every connection-level request, so a refused reply is the correct,
// documented outcome rather than a failure.
func (c *Client) Keepalive(timeout time.Duration) error {
	type result struct {
		reply bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := c.sess.SendRequest("keepalive@openssh.com", true, nil)
		done <- result{reply: ok, err: err}
	}()
	select {
	case <-time.After(timeout):
		return errors.New("keepalive did not complete within the timeout")
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		if r.reply {
			return errors.New("server answered a keepalive global request, which the transport contract refuses")
		}
		return nil
	}
}

// Output returns every byte the server has written so far.
func (c *Client) Output() string { return c.tap.String() }

// Close ends the session and the connection.
func (c *Client) Close() error {
	var firstErr error
	if err := c.stdin.Close(); err != nil && !errors.Is(err, io.EOF) {
		firstErr = err
	}
	if err := c.sess.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := c.conn.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// frame is one server write as the client observed it.
type frame struct {
	at    time.Time
	bytes []byte
}

// tapWriter records the timestamp and payload of every server write and signals
// waiters. Latency is therefore measured at the client boundary, which is the
// only place from which a user could observe it.
type tapWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	frames []frame
	notify chan struct{}
}

func newTapWriter() *tapWriter {
	return &tapWriter{notify: make(chan struct{}, 256)}
}

// Write implements io.Writer for the SSH session's stdout.
func (t *tapWriter) Write(p []byte) (int, error) {
	owned := append([]byte(nil), p...)
	t.mu.Lock()
	t.buf.Write(p)
	t.frames = append(t.frames, frame{at: time.Now(), bytes: owned})
	t.mu.Unlock()
	select {
	case t.notify <- struct{}{}:
	default:
	}
	return len(p), nil
}

// String returns everything written so far.
func (t *tapWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// awaitWrite blocks until a write happens after started.
func (t *tapWriter) awaitWrite(started time.Time, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if t.seenAfter(started, nil) {
			return nil
		}
		if !t.waitStep(deadline) {
			return fmt.Errorf("no output within %s", timeout)
		}
	}
}

// awaitPrompt blocks until a prompt frame arrives after the count taken before
// the input was submitted. Counting prompts rather than scanning for a prompt
// marker keeps a late frame from an earlier turn from ending this wait early.
func (t *tapWriter) awaitPrompt(before int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if t.promptCount() > before {
			return nil
		}
		if !t.waitStep(deadline) {
			return fmt.Errorf("no prompt within %s", timeout)
		}
	}
}

// promptCount counts how many writes have carried a shell prompt.
func (t *tapWriter) promptCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, f := range t.frames {
		if containsAny(f.bytes, promptMarkers) {
			count++
		}
	}
	return count
}

// promptMarkers are the two prompt sigils the harness handler can draw.
var promptMarkers = []string{"$ ", "# "}

// containsAny reports whether text carries one of the markers.
func containsAny(text []byte, markers []string) bool {
	for _, marker := range markers {
		if bytes.Contains(text, []byte(marker)) {
			return true
		}
	}
	return false
}

// awaitContains blocks until a write after started carried one of match. Only
// that write's own payload is searched, so a stale frame from an earlier input
// cannot satisfy a later expectation.
func (t *tapWriter) awaitContains(started time.Time, timeout time.Duration, match ...string) error {
	deadline := time.Now().Add(timeout)
	for {
		if t.seenAfter(started, match) {
			return nil
		}
		if !t.waitStep(deadline) {
			return fmt.Errorf("no output matching %s within %s", strings.Join(match, "/"), timeout)
		}
	}
}

// seenAfter reports whether any write after started carries one of match. An
// empty match accepts any write.
func (t *tapWriter) seenAfter(started time.Time, match []string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.frames) - 1; i >= 0; i-- {
		f := t.frames[i]
		if !f.at.After(started) {
			return false
		}
		if len(match) == 0 {
			return true
		}
		for _, want := range match {
			if bytes.Contains(f.bytes, []byte(want)) {
				return true
			}
		}
	}
	return false
}

// waitStep blocks for the next notification and reports whether it arrived
// before deadline.
func (t *tapWriter) waitStep(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-t.notify:
		return true
	case <-timer.C:
		return false
	}
}
