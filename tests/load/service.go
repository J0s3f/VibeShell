package load

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"j0s.at/vibeshell/internal/adapters/sqlite"
	ssh "j0s.at/vibeshell/internal/adapters/ssh"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/system"
	"j0s.at/vibeshell/internal/terminal/renderer"
)

// StackOptions configures one harness service instance.
type StackOptions struct {
	// Dir is the working directory for the harness database. It must be
	// writable and is owned by the caller.
	Dir string
	// Engine performs the turn work.
	Engine application.TurnEngine
	// SharingEnabled selects the scope policy the coordinator pins.
	SharingEnabled bool
	// WriterQueueDepth bounds the event store's accepted-but-uncommitted depth.
	WriterQueueDepth int
	// Limits overrides the coordinator's operational bounds.
	Limits application.Limits
	// Logger receives adapter observations; nil discards them.
	Logger *slog.Logger
}

// Stack is one assembled service: real storage, real coordinator, real SSH
// transport, listening on loopback. The harness owns its lifecycle.
type Stack struct {
	DB          *sqlite.DB
	Events      *sqlite.Events
	Coordinator *application.Coordinator
	Server      *ssh.Server
	Listener    net.Listener
	Address     string
	DBPath      string
	Handler     *shellHandler

	cancel    context.CancelFunc
	serveDone chan error
	stopOnce  sync.Once
}

// NewStack builds and starts a harness service. Every layer below the SSH
// adapter is the shipped implementation: the SQLite world and event store, the
// terminal renderer, and the session coordinator.
func NewStack(ctx context.Context, opts StackOptions) (*Stack, error) {
	if opts.Dir == "" {
		return nil, errors.New("load harness: StackOptions.Dir is required")
	}
	if opts.Engine == nil {
		return nil, errors.New("load harness: StackOptions.Engine is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	limits := opts.Limits
	if limits == (application.Limits{}) {
		limits = application.DefaultLimits()
	}

	dbPath := filepath.Join(opts.Dir, "vibeshell-load.db")
	db, err := sqlite.Open(dbPath, sqlite.Options{})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	stack := &Stack{DB: db, DBPath: dbPath}
	fail := func(err error) (*Stack, error) {
		_ = db.Close()
		return nil, err
	}

	eventsOpts := sqlite.EventsOptions{}
	if opts.WriterQueueDepth > 0 {
		eventsOpts.QueueCapacity = opts.WriterQueueDepth
	}
	events, err := sqlite.NewEvents(db.SQL(), eventsOpts)
	if err != nil {
		return fail(fmt.Errorf("open event store: %w", err))
	}
	stack.Events = events

	rend, err := renderer.New(db, renderer.DefaultLimits())
	if err != nil {
		events.Close()
		return fail(fmt.Errorf("build renderer: %w", err))
	}

	scopePolicy := domain.RestrictedScopePolicy()
	if opts.SharingEnabled {
		scopePolicy = domain.DefaultScopePolicy()
	}

	coordinator, err := application.NewCoordinator(application.CoordinatorOptions{
		Engine:    opts.Engine,
		Tools:     RejectTools{},
		Events:    events,
		World:     db,
		Content:   db,
		Renderer:  rend,
		Clock:     system.NewClock(),
		Random:    system.NewRandom(),
		Snapshots: &StaticSnapshots{Snapshot: harnessSnapshot(scopePolicy)},
		Limits:    limits,
	})
	if err != nil {
		events.Close()
		return fail(fmt.Errorf("build coordinator: %w", err))
	}
	stack.Coordinator = coordinator

	handler := &shellHandler{
		coordinator: coordinator,
		identity:    harnessIdentity(),
		sharing:     opts.SharingEnabled,
	}
	stack.Handler = handler
	server, err := ssh.NewServer(ssh.Options{
		Mode:    ssh.ModePublic,
		Handler: handler,
		Logger:  logger,
		Limits:  ssh.DefaultLimits,
	})
	if err != nil {
		events.Close()
		return fail(fmt.Errorf("build SSH server: %w", err))
	}
	stack.Server = server

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		events.Close()
		return fail(fmt.Errorf("listen: %w", err))
	}
	stack.Listener = listener
	stack.Address = listener.Addr().String()

	serveCtx, cancel := context.WithCancel(context.Background())
	stack.cancel = cancel
	stack.serveDone = make(chan error, 1)
	go func() { stack.serveDone <- server.Serve(serveCtx, listener) }()
	return stack, nil
}

// Close ends the service in the composition root's order: stop accepting
// connections, end live sessions, drain the event writer, checkpoint storage.
func (s *Stack) Close(ctx context.Context) error {
	var firstErr error
	s.stopOnce.Do(func() {
		s.cancel()
		select {
		case err := <-s.serveDone:
			if err != nil {
				firstErr = err
			}
		case <-ctx.Done():
			firstErr = ctx.Err()
		case <-time.After(30 * time.Second):
			firstErr = errors.New("load harness: SSH server did not stop")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Coordinator.Shutdown(shutdownCtx, application.EndShutdown); err != nil && firstErr == nil {
			firstErr = err
		}
		s.Events.Close()
		if err := s.DB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	})
	return firstErr
}

// harnessSnapshot pins the per-turn coordinator configuration. The values are
// stated explicitly so a receipt can be read against the same bounds every run.
func harnessSnapshot(policy domain.ScopePolicy) application.ConfigSnapshot {
	return application.ConfigSnapshot{
		ConfigVersion:    1,
		PromptVersion:    "loadharness/v1",
		CatalogueVersion: "loadharness/v1",
		ScopePolicy:      policy,
		TurnDeadlineMs:   120000,
		MaxAttempts:      3,
		MaxRebases:       2,
	}
}

// presentationIdentity is the subset of presentation facts the harness handler
// prints. It names the simulated system and never a real host.
type presentationIdentity struct {
	System     string
	Shell      string
	Hostname   string
	HomePrefix string
}

func harnessIdentity() presentationIdentity {
	return presentationIdentity{
		System:     "VibeOS",
		Shell:      "VibeShell",
		Hostname:   "vibeshell",
		HomePrefix: "/home",
	}
}

// shellHandler bridges one accepted SSH shell channel to one coordinator
// session. It is byte plumbing only: it decodes a realistic terminal vocabulary
// (line editing, bracketed paste, navigation keys, resize) and forwards
// semantic events to the application, which owns every rule.
type shellHandler struct {
	coordinator *application.Coordinator
	identity    presentationIdentity
	sharing     bool

	mu             sync.Mutex
	sessions       int64
	prompts        int64
	refused        int64
	refusalReasons []string
	inputs         map[string]int64

	droppedBytes atomic.Int64
}

// DroppedBytes reports how many output bytes the transport discarded for
// clients that were not reading fast enough.
func (h *shellHandler) DroppedBytes() int64 { return h.droppedBytes.Load() }

// Prompts reports how many prompts the handler drew. A turn that produced output
// but no following prompt would show up here as a shortfall.
func (h *shellHandler) Prompts() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prompts
}

var _ ssh.Handler = (*shellHandler)(nil)

// InputCounts reports how many semantic inputs the handler forwarded, by kind.
// It is the evidence that the mix scenario really exercised each input class.
func (h *shellHandler) InputCounts() map[string]int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int64, len(h.inputs))
	for k, v := range h.inputs {
		out[k] = v
	}
	return out
}

// Refusals reports how many inputs the application refused and a few example
// reasons. The refusal count is the capacity signal behind the coordinator's
// bounded turn queue; the reasons are what a reader needs to tell a full queue
// apart from a lost recording.
func (h *shellHandler) Refusals() (int64, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refused, append([]string(nil), h.refusalReasons...)
}

// countRefusal records one refused input and keeps a few reasons.
func (h *shellHandler) countRefusal(err error) {
	h.mu.Lock()
	h.refused++
	if len(h.refusalReasons) < 5 {
		h.refusalReasons = append(h.refusalReasons, err.Error())
	}
	h.mu.Unlock()
}

// SessionCount reports how many sessions this handler has opened.
func (h *shellHandler) SessionCount() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions
}

// countInput records one forwarded input kind.
func (h *shellHandler) countInput(kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inputs == nil {
		h.inputs = map[string]int64{}
	}
	h.inputs[kind]++
}

// ServeSession runs one shell channel and returns when the channel or its
// context ends.
func (h *shellHandler) ServeSession(ctx context.Context, sess *ssh.Session) {
	window := sess.Terminal()
	username := sess.Principal().Username
	principal, home, err := resolvePrincipal(h.identity, username)
	if err != nil {
		_, _ = sess.Write([]byte("vibeshell: cannot establish your identity: " + err.Error() + "\r\n"))
		_ = sess.Exit(1)
		return
	}

	cols, rows := window.Cols, window.Rows
	if cols <= 0 || cols > application.MaxTerminalCols {
		cols = 80
	}
	if rows <= 0 || rows > application.MaxTerminalRows {
		rows = 24
	}

	shell, err := h.coordinator.Open(ctx, application.OpenSessionRequest{
		Principal: principal,
		AuthMode:  application.AuthModePublic,
		Terminal: application.TerminalMetadata{
			Term: window.Name,
			Size: domain.TermSize{Cols: uint16(cols), Rows: uint16(rows)},
		},
		CWD:          home,
		TransportRef: "ssh",
	})
	if err != nil {
		_, _ = sess.Write([]byte("vibeshell: session could not be started: " + err.Error() + "\r\n"))
		_ = sess.Exit(1)
		return
	}
	h.mu.Lock()
	h.sessions++
	h.mu.Unlock()

	// Output is pumped by a dedicated goroutine so input handling never blocks
	// on a slow client: the transport's bounded queue stays the only
	// backpressure point, which is what the SSH scenarios measure.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		h.pumpOutputs(ctx, sess, shell, username)
	}()

	h.writeLine(sess, h.greeting())
	h.writePrompt(sess, username, home)

	resizeDone := make(chan struct{})
	go func() {
		defer close(resizeDone)
		h.forwardResizes(ctx, sess, shell)
	}()

	h.readLoop(ctx, sess, shell, username)

	h.droppedBytes.Add(sess.DroppedBytes())
	_ = shell.End(context.Background(), application.EndDisconnect)
	<-resizeDone
	select {
	case <-pumpDone:
	case <-ctx.Done():
	}
	_ = sess.Exit(0)
}

// resolvePrincipal maps the presented username to a durable identity and its
// visible home directory, matching the composition root's public-mode rule
// without importing package main.
func resolvePrincipal(identity presentationIdentity, username string) (domain.UserID, domain.ValidPath, error) {
	principal, err := publicIdentity(username)
	if err != nil {
		return domain.UserID{}, "", err
	}
	return principal, domain.ValidPath(identity.HomePrefix + "/" + username), nil
}

// forwardResizes turns real SSH window-change requests into resize inputs.
func (h *shellHandler) forwardResizes(ctx context.Context, sess *ssh.Session, shell application.Shell) {
	changes := sess.ResizeNotify()
	for {
		select {
		case <-ctx.Done():
			return
		case <-shell.Done():
			return
		case window, ok := <-changes:
			if !ok {
				return
			}
			size := domain.TermSize{Cols: uint16(window.Cols), Rows: uint16(window.Rows)}
			if _, err := shell.Accept(ctx, application.SessionInput{Kind: application.InputResize, Size: &size}); err != nil {
				continue
			}
			h.countInput(string(application.InputResize))
		}
	}
}

// pumpOutputs drains accepted output and outcomes until the session ends.
func (h *shellHandler) pumpOutputs(ctx context.Context, sess *ssh.Session, shell application.Shell, username string) {
	outputs := shell.Outputs()
	outcomes := shell.Outcomes()
	for outputs != nil || outcomes != nil {
		select {
		case <-ctx.Done():
			return
		case out, ok := <-outputs:
			if !ok {
				outputs = nil
				continue
			}
			h.writeOutput(sess, shell, out, username)
		case outcome, ok := <-outcomes:
			if !ok {
				outcomes = nil
				continue
			}
			if outcome.Kind == application.OutcomeFailed && outcome.Failure != nil {
				h.writeLine(sess, "vibeshell: "+outcome.Failure.Message)
			}
		}
	}
}

// writeOutput renders one accepted output item to the transport.
func (h *shellHandler) writeOutput(sess *ssh.Session, shell application.Shell, out application.SessionOutput, username string) {
	switch out.Kind {
	case application.OutputText:
		_, _ = sess.Write([]byte(out.Text + "\r\n"))
		_ = shell.ReportWrite(context.Background(), application.WriteResult{
			Session: out.Session, Turn: out.Turn, Sequence: out.Sequence,
			Status: application.WriteOK, ByteCount: int64(len(out.Text)),
		})
	case application.OutputPrompt:
		if out.Prompt != nil {
			h.writePrompt(sess, username, out.Prompt.CWD)
		}
	default:
		_ = shell.ReportWrite(context.Background(), application.WriteResult{
			Session: out.Session, Turn: out.Turn, Sequence: out.Sequence,
			Status: application.WriteDropped,
		})
	}
}

// readLoop decodes client bytes into semantic inputs. Escape sequences are
// accumulated as CSI and resolved once their final byte arrives, which is what
// lets bracketed paste and arrow keys share one small parser.
func (h *shellHandler) readLoop(ctx context.Context, sess *ssh.Session, shell application.Shell, username string) {
	decoder := newTerminalDecoder(h, shell, sess, username)
	buf := make([]byte, 32<<10)
	for {
		n, err := sess.Read(buf)
		if n > 0 {
			decoder.feed(buf[:n])
		}
		if err != nil {
			_, _ = shell.Accept(context.Background(), application.SessionInput{Kind: application.InputEOF})
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// greeting states the simulated environment and whether sharing is enabled, so
// a reader of the captured transcript can tell which policy the run used.
func (h *shellHandler) greeting() string {
	sharing := "off"
	if h.sharing {
		sharing = "on"
	}
	return fmt.Sprintf("%s 1.0.0 simulated GNU/Hurd-style environment; shell is %s; sharing %s.",
		h.identity.System, h.identity.Shell, sharing)
}

// writeLine emits one CRLF-terminated line.
func (h *shellHandler) writeLine(sess *ssh.Session, text string) {
	_, _ = sess.Write([]byte(text + "\r\n"))
}

// writePrompt emits the trusted prompt from accepted session state.
func (h *shellHandler) writePrompt(sess *ssh.Session, username string, cwd domain.ValidPath) {
	h.mu.Lock()
	h.prompts++
	h.mu.Unlock()
	_, _ = sess.Write([]byte(promptString(h.identity, username, cwd)))
}

// promptString composes the shell prompt. The home directory shows as "~".
func promptString(identity presentationIdentity, username string, cwd domain.ValidPath) string {
	home := domain.ValidPath(identity.HomePrefix + "/" + username)
	shown := string(cwd)
	switch {
	case cwd == home:
		shown = "~"
	case strings.HasPrefix(string(cwd), string(home)+"/"):
		shown = "~" + strings.TrimPrefix(string(cwd), string(home))
	}
	sigil := "$"
	if username == "root" {
		sigil = "#"
	}
	return fmt.Sprintf("%s@%s:%s%s ", username, identity.Hostname, shown, sigil)
}

// ---------------------------------------------------------------------------
// Terminal byte decoding
// ---------------------------------------------------------------------------

// terminalDecoder turns raw client bytes into semantic inputs. It is owned by
// one session channel and is not shared between channels.
type terminalDecoder struct {
	handler  *shellHandler
	shell    application.Shell
	sess     *ssh.Session
	username string
	line     []byte
	paste    []byte
	pasting  bool
	csi      []byte
	escape   escapeState
	sequence uint64
}

// newTerminalDecoder returns a decoder for one session channel.
func newTerminalDecoder(h *shellHandler, shell application.Shell, sess *ssh.Session, username string) *terminalDecoder {
	return &terminalDecoder{handler: h, shell: shell, sess: sess, username: username}
}

// feed consumes one read of client bytes.
func (d *terminalDecoder) feed(chunk []byte) {
	for _, b := range chunk {
		d.byte(b)
	}
}

// escapeState is the decoder's escape-sequence progress. ESC and the CSI
// introducer "[" are distinct steps: treating "[" as a final byte would resolve
// every sequence after one byte and silently corrupt bracketed paste.
type escapeState int

const (
	escapeNone escapeState = iota
	escapeSeen
	escapeCSI
)

// byte advances the decoder by one input byte.
func (d *terminalDecoder) byte(b byte) {
	switch d.escape {
	case escapeSeen:
		if b == '[' {
			d.escape = escapeCSI
			d.csi = d.csi[:0]
			return
		}
		// A two-byte escape sequence this shell does not use is discarded.
		d.escape = escapeNone
		return

	case escapeCSI:
		if len(d.csi) >= maxCSILength {
			// An unbounded escape sequence is a hostile or broken client.
			d.escape = escapeNone
			d.csi = d.csi[:0]
			return
		}
		d.csi = append(d.csi, b)
		if isCSIFinal(b) {
			params := string(d.csi)
			d.csi = d.csi[:0]
			d.escape = escapeNone
			d.dispatchCSI(params)
		}
		return
	}

	switch {
	case b == 0x1b:
		d.escape = escapeSeen

	case d.pasting:
		// Inside a bracketed paste the payload is data, so a byte that is not
		// part of the closing sequence is appended verbatim.
		if len(d.paste) < application.MaxPasteBytes {
			d.paste = append(d.paste, b)
		}

	case b == '\r' || b == '\n':
		d.sequence++
		d.submitLine(strings.TrimRight(string(d.line), "\r\n"))
		d.line = d.line[:0]

	case b == 0x03: // Ctrl-C
		_, _ = d.shell.CancelActive(context.Background(), "ctrl-c")
		_, _ = d.sess.Write([]byte("^C"))
		d.line = d.line[:0]
		d.handler.countInput("cancel")

	case b == 0x04: // Ctrl-D on an empty line is end of input
		if len(d.line) == 0 {
			_, _ = d.shell.Accept(context.Background(), application.SessionInput{Kind: application.InputEOF})
			d.handler.countInput("eof")
		}

	case b == 0x7f || b == 0x08: // backspace
		if len(d.line) > 0 {
			d.line = d.line[:len(d.line)-1]
			_, _ = d.sess.Write([]byte("\b \b"))
		}

	case b < 0x20:
		// Other control bytes carry no meaning in this shell.

	default:
		if len(d.line) < application.MaxCommandBytes {
			d.line = append(d.line, b)
			_, _ = d.sess.Write([]byte{b})
		}
	}
}

// maxCSILength bounds one escape sequence so a client cannot make the decoder
// buffer without limit. The longest sequence the harness understands is five
// bytes ("ESC [ 2 0 1 ~").
const maxCSILength = 16

// isCSIFinal reports whether b terminates a control sequence introducer.
func isCSIFinal(b byte) bool { return b >= 0x40 && b <= 0x7e }

// dispatchCSI resolves one complete escape sequence. Inside a bracketed paste a
// sequence that is not the closing introducer is literal payload, so it is
// appended rather than interpreted.
func (d *terminalDecoder) dispatchCSI(params string) {
	if d.pasting {
		if params == "201~" {
			d.pasting = false
			d.submitPaste()
			return
		}
		if len(d.paste) < application.MaxPasteBytes {
			d.paste = append(d.paste, 0x1b)
			d.paste = append(d.paste, []byte(params)...)
		}
		return
	}
	switch params {
	case "200~":
		d.pasting = true
		d.paste = d.paste[:0]
		return
	case "A":
		d.submitKey("up")
		return
	case "B":
		d.submitKey("down")
		return
	case "C":
		d.submitKey("right")
		return
	case "D":
		d.submitKey("left")
		return
	case "H":
		d.submitKey("home")
		return
	case "F":
		d.submitKey("end")
		return
	case "Z":
		d.submitKey("shift-tab")
		return
	case "5~":
		d.submitKey("pageup")
		return
	case "6~":
		d.submitKey("pagedown")
		return
	case "3~":
		d.submitKey("delete")
		return
	default:
		// An unrecognised sequence is ignored rather than submitted as text.
	}
}

// submitKey forwards one navigation key. A refused key is reported like any
// other refused input, so a client waiting for the terminal's answer is never
// left waiting for a write that will not come.
func (d *terminalDecoder) submitKey(key string) {
	if _, err := d.shell.Accept(context.Background(), application.SessionInput{Kind: application.InputKey, Key: key}); err != nil {
		d.refuse("key "+key, err)
		return
	}
	d.handler.countInput("key:" + key)
}

// submitLine forwards one complete command line. A refused input (a full turn
// queue, a lost recording) is reported on the terminal and followed by a
// prompt, exactly as a real shell would: a client that never sees a prompt would
// otherwise block until its own timeout and turn a bounded refusal into an
// unbounded stall.
func (d *terminalDecoder) submitLine(command string) {
	if strings.TrimSpace(command) == "" {
		return
	}
	if _, err := d.shell.Accept(context.Background(), application.SessionInput{
		Kind:     application.InputCommand,
		Command:  command,
		Sequence: d.sequence,
	}); err != nil {
		d.refuse("command", err)
		return
	}
	d.handler.countInput("command")
}

// submitPaste forwards one bracketed-paste payload. A paste starts a turn, so
// it advances the same monotonic sequence a command line would.
func (d *terminalDecoder) submitPaste() {
	payload := string(d.paste)
	d.paste = d.paste[:0]
	if payload == "" {
		return
	}
	d.sequence++
	if _, err := d.shell.Accept(context.Background(), application.SessionInput{
		Kind:     application.InputPaste,
		Text:     payload,
		Sequence: d.sequence,
	}); err != nil {
		d.refuse("paste", err)
		return
	}
	d.handler.countInput("paste")
}

// refuse reports a refused input on the terminal and redraws the prompt, so a
// bounded refusal stays bounded instead of looking like a hang: a client waiting
// for the next prompt would otherwise block until its own timeout.
func (d *terminalDecoder) refuse(what string, err error) {
	d.handler.countRefusal(err)
	_, _ = d.sess.Write([]byte("vibeshell: " + what + " refused: " + err.Error() + "\r\n"))
	d.handler.writePrompt(d.sess, d.username, domain.ValidPath(d.handler.identity.HomePrefix+"/"+d.username))
}
