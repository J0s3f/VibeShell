package sshserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termdecode"
)

// Terminal is the terminal a client asked for with a pty-req request. A session
// that never received one reports Requested false and the documented default
// size, so a renderer never has to guess.
type Terminal struct {
	Requested    bool
	Name         string
	Cols         int
	Rows         int
	WidthPixels  int
	HeightPixels int
	// Modes is the client's terminal mode string, kept verbatim and bounded.
	// The spike does not interpret modes; it records that they were received.
	Modes string
}

// String returns a log-friendly description.
func (t Terminal) String() string {
	if !t.Requested {
		return fmt.Sprintf("no-pty %dx%d", t.Cols, t.Rows)
	}
	return fmt.Sprintf("%s %dx%d", t.Name, t.Cols, t.Rows)
}

// Session is one interactive shell session: one accepted session channel and
// its application state. Two channels are two sessions, even on one connection
// and for one user.
type Session struct {
	ctx       context.Context
	cancel    context.CancelFunc
	principal Principal
	channel   ssh.Channel
	requests  <-chan *ssh.Request
	limits    Limits
	logger    *slog.Logger

	handler ShellHandler
	decoder *termdecode.Decoder
	events  chan termdecode.Event
	signals chan string
	output  *outputQueue

	mu             sync.Mutex
	terminal       Terminal
	env            map[string]string
	envRequests    int
	shellStarted   bool
	exited         bool
	exitCode       uint32
	shellOnce      sync.Once
	shellRequested chan struct{}
	finishOnce     sync.Once
	finishing      chan struct{}

	// resizePending is the newest terminal size that has not reached the event
	// stream yet, and resizeWake tells publishResizes that it changed.
	resizeMu      sync.Mutex
	resizePending *Terminal
	resizeWake    chan struct{}

	// inputDone closes when the client's input stream ends, and inputErr holds
	// the reason it ended.
	inputDone chan struct{}
	inputErr  error
}

// newSession builds a session around an accepted channel and its request
// stream. The channel's request stream arrives on requests.
func (s *Server) newSession(
	ctx context.Context,
	principal Principal,
	channel ssh.Channel,
	requests <-chan *ssh.Request,
) *Session {
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &Session{
		ctx:       sessionCtx,
		cancel:    cancel,
		principal: principal,
		channel:   channel,
		requests:  requests,
		limits:    s.limits,
		handler:   s.shell,
		logger:    s.logger.With("user", principal.Username),
		events:    make(chan termdecode.Event, s.limits.EventQueueDepth),
		signals:   make(chan string, 4),
		output:    newOutputQueue(s.limits.MaxOutputQueueBytes),
		terminal:  defaultTerminal,
		env:       map[string]string{},
		inputDone: make(chan struct{}),

		shellRequested: make(chan struct{}),
		finishing:      make(chan struct{}),
		resizeWake:     make(chan struct{}, 1),
	}
	session.decoder = termdecode.NewDecoder(termdecode.DefaultBounds, session.emit)
	return session
}

// Context is done when the client disconnects, the connection fails, or the
// server shuts down.
func (s *Session) Context() context.Context { return s.ctx }

// Principal is the authenticated identity this session runs as.
func (s *Session) Principal() Principal { return s.principal }

// Events delivers decoded input: key presses, bracketed pastes, and terminal
// resizes. Resizes coalesce, so a client that resizes faster than the handler
// consumes loses the intermediate sizes and keeps the newest one.
func (s *Session) Events() <-chan termdecode.Event { return s.events }

// Signals delivers the SSH signal requests the client sent. Ctrl-C is not one
// of them: a client in a pty session delivers Ctrl-C as the byte 0x03, which
// arrives as a termdecode.Key named termdecode.KeyCtrlC.
func (s *Session) Signals() <-chan string { return s.signals }

// Terminal returns the negotiated terminal. It is safe to call at any time; a
// later pty-req or window-change changes what it returns.
func (s *Session) Terminal() Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

// Env returns one accepted environment variable. Client variables are session
// data and are never applied to the server process environment.
func (s *Session) Env(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.env[name]
	return value, ok
}

// Write queues p for the client's stdout. Write never blocks on a client that
// stopped reading: once the session's output queue is full the oldest queued
// bytes are dropped and counted by DroppedBytes.
func (s *Session) Write(p []byte) (int, error) {
	s.output.push(p)
	return len(p), nil
}

// WriteStderr queues p for the client's stderr.
func (s *Session) WriteStderr(p []byte) (int, error) {
	_, err := s.channel.Stderr().Write(p)
	return len(p), err
}

// DroppedBytes reports how many output bytes were discarded for a client that
// did not keep up. A non-zero value means that session's screen may be
// incomplete; it never affects another session.
func (s *Session) DroppedBytes() int64 { return s.output.droppedCount() }

// InputClosed reports when the client's input stream ended, either with SSH EOF
// or with the connection going away.
func (s *Session) InputClosed() <-chan struct{} { return s.inputDone }

// InputError returns why the input stream ended, valid after InputClosed is
// closed.
func (s *Session) InputError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputErr
}

// Exit ends the session with the given SSH exit status. It is safe to call from
// the session handler; the status is sent after the session's queued output has
// had its chance to reach the client. Only the first call takes effect.
func (s *Session) Exit(code uint32) error {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return errors.New("sshserver: exit status already recorded")
	}
	s.exited = true
	s.exitCode = code
	s.mu.Unlock()
	s.finishOnce.Do(func() { close(s.finishing) })
	return nil
}

// run serves one session channel from request handling to channel close.
//
// The application session starts when the client asks for a shell, not when the
// channel opens: requests arrive in order, so a pty-req that precedes the shell
// request is already applied, and a channel that never asks for a shell never
// creates an application session.
//
// The lifecycle is driven by the handler, not by the channel closing: a client
// keeps its session channel open until it receives the exit status, so waiting
// for the request stream to end before sending that status would deadlock.
func (s *Session) run() {
	go s.pumpOutput()
	go s.readInput()
	go s.publishResizes(s.ctx)
	// Requests keep being answered after the session ends so a client is never
	// left waiting for a reply; handleRequest refuses everything once the
	// session context is done.
	go s.serveRequests()

	select {
	case <-s.shellRequested:
	case <-s.ctx.Done():
		s.logger.Debug("channel closed before a shell was requested")
		_ = s.channel.Close()
		return
	}

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handler(s)
	}()

	select {
	case <-handlerDone:
	case <-s.finishing:
	case <-s.ctx.Done():
	}

	// Cancelling stops the input reader, unblocks a handler that is waiting for
	// its context, and makes any further channel request a refusal. The output
	// pump keeps writing what is already queued.
	s.cancel()

	// Let the queued output reach the client before announcing the exit, but
	// not forever: a client that stopped reading must not hold the session.
	drainCtx, stopDraining := context.WithTimeout(context.WithoutCancel(s.ctx), s.limits.ExitDrainTimeout)
	drained := s.output.waitEmpty(drainCtx.Done())
	stopDraining()
	if !drained {
		s.logger.Debug("output still queued at exit", "queued", s.output.len(), "dropped", s.DroppedBytes())
	}

	s.mu.Lock()
	code, recorded := s.exitCode, s.exited
	s.mu.Unlock()
	if !recorded {
		code = 0
	}
	if err := s.sendExitStatus(code); err != nil {
		s.logger.Debug("exit status not sent", "error", err.Error())
	}
	_ = s.channel.Close()

	// A handler that ignores its context outlives its channel; that is the
	// handler's contract to keep, not a reason to hold the connection open.
	select {
	case <-handlerDone:
	case <-time.After(s.limits.ExitDrainTimeout):
		s.logger.Warn("handler did not return after its session ended")
	}
}

// sendExitStatus sends the SSH exit-status request.
func (s *Session) sendExitStatus(code uint32) error {
	_, err := s.channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: code}))
	return err
}

// markShellStarted releases the application session.
func (s *Session) markShellStarted() {
	s.shellOnce.Do(func() { close(s.shellRequested) })
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

// handleRequest applies one channel request or refuses it with its reason.
func (s *Session) handleRequest(req *ssh.Request) {
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
		s.refuseRequest(req, "exec is not supported: vibeshell serves interactive shell sessions only")
	case "subsystem":
		s.refuseRequest(req, "subsystem is not supported: vibeshell serves interactive shell sessions only")
	case "x11-req":
		// X11 and agent forwarding are optional capabilities: a real client
		// carries on without them, so the channel stays open.
		s.refuseRequestKeepingChannel(req, "x11 forwarding is not supported")
	case "auth-agent-req@openssh.com":
		s.refuseRequestKeepingChannel(req, "ssh-agent forwarding is not supported")
	case "eow@openssh.com":
		s.refuseRequest(req, "exit-on-write-close is not supported: close the session instead")
	default:
		s.refuseRequest(req, fmt.Sprintf("channel request %q is not supported", req.Type))
	}
}

// refuseRequest tells the client why a request was refused, on its stderr so a
// plain `ssh host command` user sees it, and answers the request with failure.
func (s *Session) refuseRequest(req *ssh.Request, reason string) {
	s.refuseRequestKeepingChannel(req, reason)
	// A refused exec or subsystem leaves the client with nothing to wait for on
	// this channel, so it is closed rather than left open.
	_ = s.channel.Close()
}

// refuseRequestKeepingChannel refuses a request without closing the channel.
//
// Some requests are optional capabilities rather than the session itself:
// auth-agent-req@openssh.com and x11-req are exactly that. Closing the channel
// for those would kill a session that a real ssh(1) client continues happily
// without the capability, so only the request is refused.
func (s *Session) refuseRequestKeepingChannel(req *ssh.Request, reason string) {
	s.logger.Info("channel request refused", "type", req.Type, "reason", reason)
	_, _ = s.WriteStderr([]byte(reason + "\r\n"))
	if err := req.Reply(false, []byte(reason)); err != nil {
		s.logger.Debug("refusal reply failed", "type", req.Type, "error", err.Error())
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

func (s *Session) handlePTYRequest(req *ssh.Request) {
	var payload ptyRequestPayload
	if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
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
	if s.terminal.Requested {
		s.mu.Unlock()
		s.refuseRequest(req, "a pty was already requested on this session")
		return
	}
	s.terminal = Terminal{
		Requested:    true,
		Name:         payload.Term,
		Cols:         cols,
		Rows:         rows,
		WidthPixels:  int(payload.WidthPixels),
		HeightPixels: int(payload.HeightPixels),
		Modes:        payload.Modes,
	}
	terminal := s.terminal
	s.mu.Unlock()

	s.logger.Info("pty requested",
		"terminal", terminal.String(),
		"modes_bytes", len(payload.Modes),
		"pixels", fmt.Sprintf("%dx%d", payload.WidthPixels, payload.HeightPixels))
	if err := req.Reply(true, nil); err != nil {
		s.logger.Debug("pty-req reply failed", "error", err.Error())
	}
	// The initial size is also a resize, so a handler that only watches
	// Resize sees the first size without reading Terminal before it runs.
	s.pushResize(terminal)
}

func (s *Session) handleEnvRequest(req *ssh.Request) {
	var payload envPayload
	if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
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

func (s *Session) handleShellRequest(req *ssh.Request) {
	s.mu.Lock()
	if s.shellStarted {
		s.mu.Unlock()
		s.refuseRequest(req, "a shell was already started on this session")
		return
	}
	s.shellStarted = true
	s.mu.Unlock()
	s.logger.Info("shell started", "terminal", s.Terminal().String())
	_ = req.Reply(true, nil)
	s.markShellStarted()
}

func (s *Session) handleWindowChange(req *ssh.Request) {
	var payload windowChangePayload
	if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
		s.refuseRequest(req, fmt.Sprintf("window-change payload is invalid: %s", err))
		return
	}
	cols, rows, err := s.boundedDimensions(int(payload.Columns), int(payload.Rows))
	if err != nil {
		s.refuseRequest(req, err.Error())
		return
	}
	s.mu.Lock()
	s.terminal.Cols = cols
	s.terminal.Rows = rows
	if payload.WidthPixels <= 1<<16 {
		s.terminal.WidthPixels = int(payload.WidthPixels)
	}
	if payload.HeightPixels <= 1<<16 {
		s.terminal.HeightPixels = int(payload.HeightPixels)
	}
	terminal := s.terminal
	s.mu.Unlock()
	// RFC 4254 defines no reply for window-change.
	s.logger.Debug("window change", "terminal", terminal.String())
	s.pushResize(terminal)
}

// supportedSignals are the SSH signal names a session accepts.
var supportedSignals = map[string]bool{
	"INT": true, "TERM": true, "HUP": true, "QUIT": true,
}

// signalPayload is the signal request payload of RFC 4254 section 6.9.
type signalPayload struct {
	Name string
}

func (s *Session) handleSignalRequest(req *ssh.Request) {
	var payload signalPayload
	if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
		s.refuseRequest(req, fmt.Sprintf("signal payload is invalid: %s", err))
		return
	}
	if !supportedSignals[payload.Name] {
		s.refuseRequest(req, fmt.Sprintf("signal %q is not supported", payload.Name))
		return
	}
	s.logger.Info("signal received", "signal", payload.Name)
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

// defaultTerminal is the size a session reports when the client asked for a pty
// but gave no usable dimensions. It is a package constant because it is part of
// the documented contract, not a per-session choice.
var defaultTerminal = Terminal{Cols: 80, Rows: 24}

func (s *Session) defaultTerminal() Terminal { return defaultTerminal }

// boundedDimensions validates a client-reported terminal size.
//
// A real OpenSSH client reports a dimension of zero when it has no size to
// report: `ssh -tt host` with stdin from a pipe, or a terminal whose size could
// not be read. Zero therefore means "unknown" and falls back to the default,
// while any other out-of-range value is a malformed request and is refused.
func (s *Session) boundedDimensions(cols, rows int) (int, int, error) {
	cols, err := s.boundedDimension("columns", cols, s.defaultTerminal().Cols)
	if err != nil {
		return 0, 0, err
	}
	rows, err = s.boundedDimension("rows", rows, s.defaultTerminal().Rows)
	if err != nil {
		return 0, 0, err
	}
	return cols, rows, nil
}

func (s *Session) boundedDimension(name string, value, fallback int) (int, error) {
	switch {
	case value == 0:
		return fallback, nil
	case value < s.limits.MinDimension || value > s.limits.MaxDimension:
		return 0, fmt.Errorf("%s %d out of range %d..%d",
			name, value, s.limits.MinDimension, s.limits.MaxDimension)
	default:
		return value, nil
	}
}

// emit hands one decoded input event to the handler, blocking while the event
// queue is full so that input cannot be lost and memory stays bounded.
func (s *Session) emit(event termdecode.Event) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

// pushResize records the newest terminal size for delivery.
//
// A resize is a derived fact: only the newest one matters, so a size that
// arrives while an older one is still undelivered replaces it. Keeping the
// pending size in its own slot, rather than searching the event channel, is what
// makes that safe: a key press in the queue is never dropped or reordered to
// make room for a resize.
func (s *Session) pushResize(terminal Terminal) {
	s.resizeMu.Lock()
	s.resizePending = &terminal
	s.resizeMu.Unlock()
	select {
	case s.resizeWake <- struct{}{}:
	default:
	}
}

// publishResizes moves pending sizes into the event stream. One goroutine owns
// that transfer so the queue stays in order.
func (s *Session) publishResizes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.resizeWake:
		}
		for {
			s.resizeMu.Lock()
			pending := s.resizePending
			s.resizePending = nil
			s.resizeMu.Unlock()
			if pending == nil {
				break
			}
			select {
			case s.events <- termdecode.Resize{Cols: pending.Cols, Rows: pending.Rows}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// readInput feeds the client's stdin bytes to the decoder until they end.
func (s *Session) readInput() {
	defer close(s.inputDone)
	buf := make([]byte, 32<<10)
	for {
		n, err := s.channel.Read(buf)
		if n > 0 {
			if _, writeErr := s.decoder.Write(buf[:n]); writeErr != nil {
				s.setInputError(writeErr)
				return
			}
		}
		if err != nil {
			// EOF is the client's normal end of input; the decoder flushes so a
			// trailing partial key sequence becomes an event.
			s.decoder.Flush()
			if !errors.Is(err, io.EOF) {
				s.setInputError(fmt.Errorf("read input: %w", err))
			}
			return
		}
		select {
		case <-s.ctx.Done():
			return
		default:
		}
	}
}

func (s *Session) setInputError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputErr == nil {
		s.inputErr = err
	}
}

// pumpOutput writes queued bytes to the client's stdout. It keeps writing what
// is already queued after the session context is done, so output the handler
// produced before it exited still reaches the client.
func (s *Session) pumpOutput() {
	const chunk = 32 << 10
	buf := make([]byte, chunk)
	for {
		if data := s.output.pop(buf); len(data) > 0 {
			written, err := s.channel.Write(data)
			s.output.done(written)
			if err != nil {
				s.logger.Debug("output write ended", "error", err.Error())
				return
			}
			continue
		}
		if s.ctx.Err() != nil {
			return
		}
		select {
		case <-s.output.wake:
		case <-s.ctx.Done():
			// Flush what arrived before the cancellation and stop.
			for {
				data := s.output.pop(buf)
				if len(data) == 0 {
					return
				}
				written, err := s.channel.Write(data)
				s.output.done(written)
				if err != nil {
					return
				}
			}
		}
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
