package ssh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"unicode"
	"unicode/utf8"

	cryptossh "golang.org/x/crypto/ssh"
)

// Window is the terminal size a client reported with pty-req or window-change.
// A session that never received a pty request reports Requested false and the
// conventional 80x24 default, so the application never has to guess.
type Window struct {
	// Requested is true after the client sent an accepted pty-req.
	Requested bool
	// Name is the TERM string from pty-req.
	Name string
	// Cols and Rows are the bounded character dimensions.
	Cols int
	Rows int
	// WidthPx and HeightPx are the reported pixel dimensions, if any.
	WidthPx  int
	HeightPx int
}

// String returns a log-friendly description.
func (w Window) String() string {
	if !w.Requested {
		return fmt.Sprintf("no-pty %dx%d", w.Cols, w.Rows)
	}
	return fmt.Sprintf("%s %dx%d", w.Name, w.Cols, w.Rows)
}

// Handler is the application-facing boundary of one SSH shell channel. The
// session/turn coordinator (task C01) implements it; tests use HandlerFunc or
// a recording double.
//
// The server calls ServeSession once per accepted shell channel, after the
// client sent the shell request, and returns from run only after ServeSession
// returns (or the session context ends). ServeSession must return when ctx is
// done: ctx ends on client disconnect, connection failure, or server shutdown.
// A handler that returns early ends only its own session.
//
// The handler performs raw byte I/O on sess: Read for stdin (io.EOF when the
// client closes its input), Write for stdout, WriteStderr for stderr, and
// Exit for the final status. It must not assume a PTY exists; Terminal
// reports whether one was requested. It must not start processes, open host
// files, or dial the network; this package provides no such capability, and
// the simulation behind Handler runs generated logic inside the bounded
// sandbox only.
type Handler interface {
	ServeSession(ctx context.Context, sess *Session)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, sess *Session)

// ServeSession calls f.
func (f HandlerFunc) ServeSession(ctx context.Context, sess *Session) {
	f(ctx, sess)
}

// Session is one interactive shell session: one accepted SSH session channel
// and its application state. Two channels are two sessions, even on one
// connection and for one user. The zero value is unusable; the server builds
// sessions for accepted channels.
//
// The Session is safe for the handler goroutine plus the server's internal
// pump goroutines. Concurrent Read calls are not supported; exactly one
// handler goroutine should read stdin.
type Session struct {
	ctx       context.Context
	cancel    context.CancelFunc
	principal Principal
	channel   cryptossh.Channel
	requests  <-chan *cryptossh.Request
	limits    Limits
	logger    *slog.Logger

	handler Handler
	stdout  *outputQueue
	stderrQ *outputQueue
	resize  chan Window
	signals chan string

	mu            sync.Mutex
	window        Window
	env           map[string]string
	envRequests   int
	shellStarted  bool
	shellOnce     sync.Once
	shellAcquired chan struct{}
	exited        bool
	exitCode      uint32
	inputErr      error
}

// newSession builds a session around an accepted channel and its request
// stream.
func (s *Server) newSession(
	ctx context.Context,
	principal Principal,
	channel cryptossh.Channel,
	requests <-chan *cryptossh.Request,
) *Session {
	sessionCtx, cancel := context.WithCancel(ctx)
	sess := &Session{
		ctx:           sessionCtx,
		cancel:        cancel,
		principal:     principal,
		channel:       channel,
		requests:      requests,
		limits:        s.limits,
		handler:       s.handler,
		logger:        s.logger.With("user", principal.Username),
		stdout:        newOutputQueue(s.limits.MaxOutputQueueBytes),
		stderrQ:       newOutputQueue(s.limits.MaxOutputQueueBytes),
		resize:        make(chan Window, 1),
		signals:       make(chan string, 4),
		window:        Window{Cols: DefaultTerminalCols, Rows: DefaultTerminalRows},
		env:           map[string]string{},
		shellAcquired: make(chan struct{}),
	}
	return sess
}

// Context ends on client disconnect, connection failure, or server shutdown.
func (s *Session) Context() context.Context { return s.ctx }

// Principal is the authenticated identity this session runs as.
func (s *Session) Principal() Principal { return s.principal }

// Terminal returns the negotiated terminal size. It is safe to call at any
// time; a later pty-req or window-change changes what it returns.
func (s *Session) Terminal() Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.window
}

// ResizeNotify receives the latest window whenever an accepted pty-req or
// window-change updates it. Delivery coalesces: a client that resizes faster
// than the handler consumes loses the intermediate sizes and keeps the
// newest one. The channel never closes before the session ends.
func (s *Session) ResizeNotify() <-chan Window { return s.resize }

// Signals delivers the SSH signal requests the client sent (INT, TERM, HUP,
// QUIT). Ctrl-C is not one of them: a client in a PTY session delivers Ctrl-C
// as the byte 0x03 on stdin. Delivery coalesces like ResizeNotify.
func (s *Session) Signals() <-chan string { return s.signals }

// Env returns one accepted client environment variable. Client variables are
// session data and are never applied to the server process environment.
func (s *Session) Env(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.env[name]
	return value, ok
}

// Read consumes stdin bytes. It returns io.EOF when the client closes its
// input or the session ends. Exactly one goroutine should call Read.
func (s *Session) Read(p []byte) (int, error) {
	n, err := s.channel.Read(p)
	if err != nil {
		s.mu.Lock()
		if s.inputErr == nil {
			s.inputErr = err
		}
		s.mu.Unlock()
	}
	return n, err
}

// InputError reports why stdin ended, or nil when it has not ended. A normal
// client EOF is reported as io.EOF.
func (s *Session) InputError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputErr
}

// Write queues p for the client's stdout and implements io.Writer, so
// fmt.Fprint(sess, ...) works. Write never blocks on a client that stopped
// reading: once the session queue is full the oldest queued bytes are dropped
// and counted by DroppedBytes.
func (s *Session) Write(p []byte) (int, error) {
	s.stdout.push(p)
	return len(p), nil
}

// WriteStderr queues p for the client's stderr with the same bounded,
// never-blocking behavior as Write.
func (s *Session) WriteStderr(p []byte) (int, error) {
	s.stderrQ.push(p)
	return len(p), nil
}

// DroppedBytes reports how many stdout plus stderr bytes were discarded for a
// client that did not keep up.
func (s *Session) DroppedBytes() int64 {
	return s.stdout.droppedCount() + s.stderrQ.droppedCount()
}

// Exit sends the SSH exit status. Only the first call sends one; later calls
// report an error. When the handler returns without calling Exit, the server
// sends status 0 after flushing queued output.
func (s *Session) Exit(code uint32) error {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return errors.New("ssh: exit status already sent")
	}
	s.exited = true
	s.exitCode = code
	s.mu.Unlock()

	payload := cryptossh.Marshal(struct{ Status uint32 }{Status: code})
	_, err := s.channel.SendRequest("exit-status", false, payload)
	return err
}

// run serves one session channel from request handling to channel close.
//
// The application session starts when the client asks for a shell, not when
// the channel opens: requests arrive in order, so a pty-req that precedes the
// shell request is already applied, and a channel that never asks for a shell
// never reaches the Handler.
func (s *Session) run() {
	go s.pumpOutput(s.stdout, false)
	go s.pumpOutput(s.stderrQ, true)
	// Requests keep being answered after the handler returns so a client is
	// never left waiting for a reply; handleRequest refuses everything once
	// the session context is done.
	reqsDone := make(chan struct{})
	go func() {
		defer close(reqsDone)
		s.serveRequests()
	}()

	select {
	case <-s.shellAcquired:
	case <-s.ctx.Done():
		s.logger.Debug("channel closed before a shell was requested")
		_ = s.channel.Close()
		return
	case <-reqsDone:
		// The channel went away before any shell request: there is no
		// application session to run and nothing to report.
		s.logger.Debug("channel closed before a shell was requested")
		_ = s.channel.Close()
		return
	}

	// A handler blocked in Read ends with its session: every way the
	// session context can end (client disconnect, connection failure,
	// server shutdown, or the end of run below) coincides with the
	// channel's connection going away or the channel itself closing,
	// and either unblocks the read with an error. There is deliberately
	// no separate "unblock on context done" watcher: closing the channel
	// from a second goroutine could overtake the final exit status on
	// the wire and leave the client with a missing-status error, a race
	// no disarm flag can close once the watcher has been scheduled late.
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handler.ServeSession(s.ctx, s)
	}()

	select {
	case <-handlerDone:
	case <-s.ctx.Done():
		<-handlerDone
	}

	// Let the queued output reach the client before announcing the exit, but
	// not forever: a client that stopped reading must not hold the session.
	drainCtx, stopDraining := context.WithTimeout(context.Background(), s.limits.ExitDrainTimeout)
	drained := s.stdout.waitEmpty(drainCtx.Done()) && s.stderrQ.waitEmpty(drainCtx.Done())
	stopDraining()
	if !drained {
		s.logger.Debug("output still queued at exit",
			"queuedStdout", s.stdout.queued(), "queuedStderr", s.stderrQ.queued(),
			"dropped", s.DroppedBytes())
	}

	// Cancelling stops the pumps and makes any further channel request a
	// refusal.
	s.cancel()

	s.mu.Lock()
	alreadyExited := s.exited
	sentCode := s.exitCode
	s.mu.Unlock()
	if !alreadyExited {
		if err := s.Exit(0); err != nil {
			s.logger.Debug("exit status not sent", "error", err.Error())
		}
	} else {
		s.logger.Debug("handler sent its own exit status", "code", sentCode)
	}
	_ = s.channel.Close()
}

// markShellAcquired releases the application session exactly once.
func (s *Session) markShellAcquired() {
	s.shellOnce.Do(func() { close(s.shellAcquired) })
}

// serveRequests answers channel requests until the channel closes.
func (s *Session) serveRequests() {
	for req := range s.requests {
		if s.ctx.Err() != nil {
			_ = req.Reply(false, nil)
			continue
		}
		s.handleRequest(req)
	}
}

// refuseSessionKind refuses a request for a different session kind. Before
// any shell started, the channel has nothing else to do and ends with status
// 127; after a shell started, the refusal keeps the live session.
func (s *Session) refuseSessionKind(req *cryptossh.Request, reason string) {
	s.mu.Lock()
	started := s.shellStarted
	s.mu.Unlock()
	if started {
		s.refuseRequest(req, reason)
		return
	}
	s.exitUnstarted(req, reason)
}

// exitUnstarted ends a channel that will never run a shell: exec and
// subsystem ask for a different session kind, so after refusing there is
// nothing else for this channel to do. The stderr note explains the refusal
// to a plain `ssh host command` user; the 127 status matches the familiar
// "command not found" convention without running anything.
func (s *Session) exitUnstarted(req *cryptossh.Request, reason string) {
	s.logger.Info("channel request refused", "type", req.Type, "reason", reason)
	s.WriteStderr([]byte(reason + "\r\n"))
	_ = req.Reply(false, []byte(reason))
	// Flush the note before reporting the status: closing the channel first
	// would discard queued bytes the pump has not written yet. The wait is
	// bounded so a stalled client cannot hold the refusal either.
	drainCtx, stopDraining := context.WithTimeout(context.Background(), s.limits.ExitDrainTimeout)
	s.stderrQ.waitEmpty(drainCtx.Done())
	stopDraining()
	_, _ = s.channel.SendRequest("exit-status", false,
		cryptossh.Marshal(struct{ Status uint32 }{Status: 127}))
	_ = s.channel.Close()
}

// refuseRequest answers a request with failure and keeps the channel open.
// Refusal reasons stay out of the shell byte stream: writing them to stderr
// would corrupt an established session, so they go to the reply and the
// server log only.
func (s *Session) refuseRequest(req *cryptossh.Request, reason string) {
	s.logger.Info("channel request refused", "type", req.Type, "reason", reason)
	_ = req.Reply(false, []byte(reason))
}

// handleRequest applies one channel request or refuses it with its reason.
// exec and subsystem end the channel (exitUnstarted); every other refusal
// keeps an established or establishing shell alive.
func (s *Session) handleRequest(req *cryptossh.Request) {
	switch req.Type {
	case "pty-req":
		s.handlePTYRequest(req)
	case "env":
		s.handleEnvRequest(req)
	case "shell":
		s.handleShellRequest(req)
	case "window-change":
		s.handleWindowChange(req)
	case "signal":
		s.handleSignalRequest(req)
	case "exec":
		s.refuseSessionKind(req, "exec is not supported: vibeshell serves interactive shell sessions only")
	case "subsystem":
		s.refuseSessionKind(req, "subsystem is not supported: vibeshell serves interactive shell sessions only")
	case "x11-req":
		s.refuseRequest(req, "x11 forwarding is not supported")
	case "auth-agent-req@openssh.com":
		s.refuseRequest(req, "ssh-agent forwarding is not supported")
	case "eow@openssh.com":
		s.refuseRequest(req, "exit-on-write-close is not supported: close the session instead")
	default:
		s.refuseRequest(req, fmt.Sprintf("channel request %q is not supported", req.Type))
	}
}

// ptyRequestPayload is the pty-req payload of RFC 4254 section 6.2.
type ptyRequestPayload struct {
	Term                      string
	Columns, Rows             uint32
	WidthPixels, HeightPixels uint32
	Modes                     string
}

// windowChangePayload is the window-change payload of RFC 4254 section 6.7.
type windowChangePayload struct {
	Columns, Rows             uint32
	WidthPixels, HeightPixels uint32
}

// envPayload is the env request payload of RFC 4254 section 6.4.
type envPayload struct {
	Name  string
	Value string
}

// DefaultTerminalCols and DefaultTerminalRows are the conventional size
// reported when nothing better is known: for a session without a pty, and
// for a dimension a client reports as zero (unknown), which terminal-less
// environments such as scripted `ssh -tt` do.
const (
	DefaultTerminalCols = 80
	DefaultTerminalRows = 24
)

func (s *Session) handlePTYRequest(req *cryptossh.Request) {
	var payload ptyRequestPayload
	if err := cryptossh.Unmarshal(req.Payload, &payload); err != nil {
		s.refuseRequest(req, fmt.Sprintf("pty-req payload is invalid: %s", err))
		return
	}
	if len(payload.Term) > s.limits.MaxTerminalNameBytes {
		s.refuseRequest(req, fmt.Sprintf("TERM longer than %d bytes", s.limits.MaxTerminalNameBytes))
		return
	}
	if !isPrintableASCII(payload.Term) {
		s.refuseRequest(req, "TERM contains a control or non-ASCII character")
		return
	}
	if len(payload.Modes) > s.limits.MaxTerminalModesBytes {
		s.refuseRequest(req, fmt.Sprintf("pty modes longer than %d bytes", s.limits.MaxTerminalModesBytes))
		return
	}
	cols, rows, err := s.boundedDimensions(int(payload.Columns), int(payload.Rows))
	if err != nil {
		s.refuseRequest(req, err.Error())
		return
	}
	if payload.WidthPixels > 1<<16 || payload.HeightPixels > 1<<16 {
		s.refuseRequest(req, "pty pixel dimensions out of range")
		return
	}

	s.mu.Lock()
	if s.window.Requested {
		s.mu.Unlock()
		s.refuseRequest(req, "a pty was already requested on this session")
		return
	}
	s.window = Window{
		Requested: true,
		Name:      payload.Term,
		Cols:      cols,
		Rows:      rows,
		WidthPx:   int(payload.WidthPixels),
		HeightPx:  int(payload.HeightPixels),
	}
	window := s.window
	s.mu.Unlock()

	s.logger.Debug("pty requested", "terminal", window.String(), "modes", len(payload.Modes))
	if err := req.Reply(true, nil); err != nil {
		s.logger.Debug("pty-req reply failed", "error", err.Error())
	}
	// The initial size is also a resize, so a handler that only watches
	// ResizeNotify sees the first size without reading Terminal first.
	s.pushResize(window)
}

func (s *Session) handleEnvRequest(req *cryptossh.Request) {
	var payload envPayload
	if err := cryptossh.Unmarshal(req.Payload, &payload); err != nil {
		s.refuseRequest(req, fmt.Sprintf("env payload is invalid: %s", err))
		return
	}
	if len(payload.Name) > s.limits.MaxEnvNameBytes || len(payload.Value) > s.limits.MaxEnvValueBytes {
		s.refuseRequest(req, "env name or value out of range")
		return
	}
	if !isAllowedEnvName(payload.Name) {
		s.logger.Info("env refused", "name", payload.Name, "reason", "not a locale variable")
		_ = req.Reply(false, []byte("only locale variables are accepted"))
		return
	}
	if !isValidEnvValue(payload.Value) {
		s.logger.Info("env refused", "name", payload.Name, "reason", "value holds a control character")
		_ = req.Reply(false, []byte("value holds a control character"))
		return
	}
	s.mu.Lock()
	if s.envRequests >= s.limits.MaxEnvRequests {
		s.mu.Unlock()
		_ = req.Reply(false, []byte("too many env requests"))
		return
	}
	s.envRequests++
	s.env[payload.Name] = payload.Value
	s.mu.Unlock()
	s.logger.Debug("env accepted", "name", payload.Name)
	_ = req.Reply(true, nil)
}

func (s *Session) handleShellRequest(req *cryptossh.Request) {
	s.mu.Lock()
	if s.shellStarted {
		s.mu.Unlock()
		s.refuseRequest(req, "a shell was already started on this session")
		return
	}
	s.shellStarted = true
	s.mu.Unlock()
	s.logger.Debug("shell started", "terminal", s.Terminal().String())
	_ = req.Reply(true, nil)
	s.markShellAcquired()
}

func (s *Session) handleWindowChange(req *cryptossh.Request) {
	var payload windowChangePayload
	if err := cryptossh.Unmarshal(req.Payload, &payload); err != nil {
		s.refuseRequest(req, fmt.Sprintf("window-change payload is invalid: %s", err))
		return
	}
	cols, rows, err := s.boundedDimensions(int(payload.Columns), int(payload.Rows))
	if err != nil {
		s.refuseRequest(req, err.Error())
		return
	}
	s.mu.Lock()
	s.window.Cols = cols
	s.window.Rows = rows
	if payload.WidthPixels <= 1<<16 {
		s.window.WidthPx = int(payload.WidthPixels)
	}
	if payload.HeightPixels <= 1<<16 {
		s.window.HeightPx = int(payload.HeightPixels)
	}
	window := s.window
	s.mu.Unlock()
	// RFC 4254 defines no reply for window-change.
	s.logger.Debug("window change", "terminal", window.String())
	s.pushResize(window)
}

// supportedSignals are the SSH signal names a session accepts. They are
// delivered to the application, which decides what an interrupt means for the
// simulated foreground program; the transport itself never kills anything.
var supportedSignals = map[string]bool{
	"INT": true, "TERM": true, "HUP": true, "QUIT": true,
}

// signalPayload is the signal request payload of RFC 4254 section 6.9.
type signalPayload struct {
	Name string
}

func (s *Session) handleSignalRequest(req *cryptossh.Request) {
	var payload signalPayload
	if err := cryptossh.Unmarshal(req.Payload, &payload); err != nil {
		s.refuseRequest(req, fmt.Sprintf("signal payload is invalid: %s", err))
		return
	}
	if !supportedSignals[payload.Name] {
		s.refuseRequest(req, fmt.Sprintf("signal %q is not supported", payload.Name))
		return
	}
	s.logger.Debug("signal received", "signal", payload.Name)
	_ = req.Reply(true, nil)
	select {
	case s.signals <- payload.Name:
	default:
		// The handler is behind on signals; the newest one still arrives.
		select {
		case <-s.signals:
		default:
		}
		select {
		case s.signals <- payload.Name:
		default:
		}
	}
}

// boundedDimensions validates a client-reported terminal size. A zero
// dimension means the client does not know its size (terminal-less
// environments report 0x0) and clamps to the conventional default; the
// result is then validated against the configured bounds like any other
// size, so clamping can still be refused under an operator range that
// excludes the default.
func (s *Session) boundedDimensions(cols, rows int) (int, int, error) {
	if cols == 0 {
		cols = DefaultTerminalCols
	}
	if rows == 0 {
		rows = DefaultTerminalRows
	}
	if cols < s.limits.MinDimension || cols > s.limits.MaxDimension {
		return 0, 0, fmt.Errorf("columns %d out of range %d..%d",
			cols, s.limits.MinDimension, s.limits.MaxDimension)
	}
	if rows < s.limits.MinDimension || rows > s.limits.MaxDimension {
		return 0, 0, fmt.Errorf("rows %d out of range %d..%d",
			rows, s.limits.MinDimension, s.limits.MaxDimension)
	}
	return cols, rows, nil
}

// pushResize delivers a terminal size, coalescing with an undelivered one so
// a resizing client cannot grow the queue without limit.
func (s *Session) pushResize(window Window) {
	select {
	case s.resize <- window:
	default:
		select {
		case <-s.resize:
		default:
		}
		select {
		case s.resize <- window:
		default:
			s.logger.Debug("resize dropped", "terminal", window.String())
		}
	}
}

// pumpOutput writes one stream's queued bytes to the client until the session
// ends.
func (s *Session) pumpOutput(queue *outputQueue, extended bool) {
	const chunk = 32 << 10
	buf := make([]byte, chunk)
	for {
		data := queue.pop(buf)
		if len(data) == 0 {
			select {
			case <-queue.wake:
				continue
			case <-s.ctx.Done():
				return
			}
		}
		var err error
		if extended {
			_, err = s.channel.Stderr().Write(data)
		} else {
			_, err = s.channel.Write(data)
		}
		if err != nil {
			s.logger.Debug("output write ended", "extended", extended, "error", err.Error())
			return
		}
		// Delivery, not dequeueing, releases a drain waiter: exit-status and
		// channel close must follow this write on the wire.
		queue.markWritten(len(data))
	}
}

// isAllowedEnvName reports whether a client environment variable is one this
// contract accepts. Only locale data is accepted; nothing can influence the
// server process environment.
func isAllowedEnvName(name string) bool {
	switch name {
	case "LANG", "LANGUAGE":
		return true
	}
	if len(name) > 3 && name[:3] == "LC_" {
		return isValidEnvName(name)
	}
	return false
}

func isValidEnvName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// isValidEnvValue reports whether a value is safe to keep as session data: it
// must be valid UTF-8 and free of control characters, so a value can never
// inject terminal control sequences into a later screen.
func isValidEnvValue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\t' {
			return false
		}
	}
	return true
}

// isPrintableASCII reports whether s is a usable TERM value.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
