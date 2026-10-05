package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/observability"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/presentation"
	"j0s.at/vibeshell/internal/terminal/editor"
	"j0s.at/vibeshell/internal/terminal/input"
	"j0s.at/vibeshell/internal/terminal/screen"

	ssh "j0s.at/vibeshell/internal/adapters/ssh"
)

// sshHandler bridges one accepted SSH shell channel to one application
// session. It resolves the authenticated username to a durable identity,
// generates the session MOTD, runs a minimal line editor, and forwards
// submitted lines to the turn coordinator. It performs byte I/O only; it
// owns no simulation, routing, or persistence rule (PLAN 3.3).
type sshHandler struct {
	coordinator *application.Coordinator
	// content fetches rendered frames for the client. Nil drops them.
	content  ports.ContentStore
	motd     *presentation.MOTDService
	identity presentation.SystemIdentity
	// authMode selects how a username becomes a durable identity.
	authMode  configAuthMode
	passwords *passwordLookup
	sharing   bool
	recording bool
	logger    *observability.Logger
	terminal  terminalBounds
	// maxPasteBytes is the configured bound on one bracketed paste. Zero keeps
	// the input decoder's default.
	maxPasteBytes int
	// sessions counts in-flight ServeSession calls so shutdown can wait for
	// every accepted session to finish recording its end before the event
	// store closes (PLAN 12.3).
	sessions sessionTracker
}

// sessionTracker counts in-flight session handlers. It stops admitting new
// sessions at shutdown and lets the composition root wait for the admitted
// ones to finish within the grace period.
type sessionTracker struct {
	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

// enter registers a starting session handler. It returns false once shutdown
// has begun so no session is admitted after the wait starts.
func (t *sessionTracker) enter() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	t.wg.Add(1)
	return true
}

// leave marks one session handler finished.
func (t *sessionTracker) leave() { t.wg.Done() }

// wait stops admission and waits for in-flight handlers until deadline,
// reporting whether every handler returned in time.
func (t *sessionTracker) wait(deadline time.Time) bool {
	t.mu.Lock()
	t.closing = true
	t.mu.Unlock()

	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// configAuthMode mirrors the configured authentication mode without importing
// the config adapter into this file's identity logic.
type configAuthMode int

const (
	authModePublic configAuthMode = iota
	authModeSecure
)

// terminalBounds caps client-reported dimensions for the handler's own
// bookkeeping. The transport already validated them; this keeps the handler
// from trusting an adapter change implicitly.
type terminalBounds struct {
	maxCols int
	maxRows int
}

// ServeSession runs one shell channel. It must return when ctx ends.
func (h *sshHandler) ServeSession(ctx context.Context, sess *ssh.Session) {
	// Refuse a session that arrives after shutdown has stopped admission, so
	// no session is accepted once the composition root begins waiting.
	if !h.sessions.enter() {
		h.write(sess, "vibeshell: the service is shutting down")
		_ = sess.Exit(1)
		return
	}
	// leave is registered before any session.End defer, so defers run LIFO and
	// the session end is recorded before the handler is counted as finished.
	defer h.sessions.leave()

	window := sess.Terminal()
	principal, home, err := h.resolvePrincipal(sess.Principal().Username)
	if err != nil {
		h.write(sess, "vibeshell: cannot establish your identity: "+err.Error())
		_ = sess.Exit(1)
		return
	}

	termCols, termRows := window.Cols, window.Rows
	if termCols <= 0 || termCols > h.terminal.maxCols {
		termCols = ssh.DefaultTerminalCols
	}
	if termRows <= 0 || termRows > h.terminal.maxRows {
		termRows = ssh.DefaultTerminalRows
	}
	// The application layer enforces its own maximum; clamp to the smaller of
	// the configured transport bound and that application bound so accept
	// never fails on a size the transport already accepted.
	if termCols > application.MaxTerminalCols {
		termCols = application.MaxTerminalCols
	}
	if termRows > application.MaxTerminalRows {
		termRows = application.MaxTerminalRows
	}

	shell, err := h.coordinator.Open(ctx, application.OpenSessionRequest{
		Principal: principal,
		AuthMode:  authModeOf(h.authMode),
		Terminal: application.TerminalMetadata{
			Term: window.Name,
			Size: domain.TermSize{Cols: uint16(termCols), Rows: uint16(termRows)},
			// The transport logs the remote address but does not expose it
			// on the session, so the recorded value is empty rather than
			// fabricated.
			ClientAddr: "",
		},
		CWD:          home,
		TransportRef: "ssh",
	})
	if err != nil {
		h.logger.Error("session refused", attr(observability.FieldOperation, "session_open"),
			attr(observability.FieldResult, "error"), attr(observability.FieldError, err.Error()))
		h.write(sess, "vibeshell: session could not be started: "+err.Error())
		_ = sess.Exit(1)
		return
	}
	defer func() { _ = shell.End(context.Background(), application.EndDisconnect) }()

	// The output pump writes accepted output; input is read in this goroutine so
	// Accept never blocks the write path. The pump forwards each new prompt to
	// the reader, which owns the editable prompt region.
	prompts := make(chan string, 4)
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		h.pumpOutputs(ctx, sess, shell, prompts)
	}()

	h.writeMOTD(ctx, sess, sess.Principal().Username)

	h.readLoop(ctx, sess, shell, prompts)

	_ = shell.End(context.Background(), application.EndDisconnect)
	select {
	case <-pumpDone:
	case <-ctx.Done():
	}
	_ = sess.Exit(0)
}

// resolvePrincipal maps the presented username to its durable identity and
// visible home directory. Public and secure namespaces never mix.
func (h *sshHandler) resolvePrincipal(username string) (domain.UserID, domain.ValidPath, error) {
	switch h.authMode {
	case authModeSecure:
		id, err := h.passwords.identityFor(username)
		if err != nil {
			return domain.UserID{}, "", err
		}
		return id, homePath(username), nil
	default:
		id, err := publicIdentity(username)
		if err != nil {
			return domain.UserID{}, "", err
		}
		return id, homePath(username), nil
	}
}

// writeMOTD generates and writes the session welcome. A generation failure
// yields the truthful service-unavailable message; the session still starts.
func (h *sshHandler) writeMOTD(ctx context.Context, sess *ssh.Session, username string) {
	if h.motd == nil {
		// The prompt file could not be loaded at startup, so there is no
		// MOTD policy to run. The session still greets truthfully instead of
		// presenting a blank login (PLAN 12.3).
		h.write(sess, serviceUnavailableMOTD)
		h.write(sess, "")
		return
	}
	result, err := h.motd.GenerateMOTD(ctx, presentation.MOTDInputs{
		Username:             username,
		SessionTimeUnixMilli: nowUnixMilli(),
		SharingEnabled:       h.sharing,
		PermanentRecording:   h.recording,
	})
	if err != nil {
		// A policy error here is not fatal to the session; show the
		// truthful fallback so the login is never blank.
		result = presentation.MOTDResult{Text: serviceUnavailableMOTD, ServiceUnavailable: true}
	}
	h.write(sess, result.Text)
	h.write(sess, "")
}

// serviceUnavailableMOTD mirrors the presentation fallback for the rare case
// where even the policy returns an error.
const serviceUnavailableMOTD = "Welcome to VibeOS, a simulated GNU/Hurd-style environment whose shell is VibeShell.\n" +
	"The generated login message is unavailable because the model service is not reachable right now."

// promptString composes the familiar "user@host:cwd$" prompt. The home
// directory is shown as "~". While a line-based application is foreground the
// application owns the prompt instead.
func promptString(identity presentation.SystemIdentity, username string, state application.PromptState) string {
	if state.App != nil {
		return appPromptString(state.App)
	}
	cwd := string(state.CWD)
	if home := homePath(username); state.CWD == home {
		cwd = "~"
	} else if strings.HasPrefix(cwd, string(home)+"/") {
		cwd = "~" + strings.TrimPrefix(cwd, string(home))
	}
	sigil := "$"
	if username == "root" {
		sigil = "#"
	}
	return fmt.Sprintf("%s@%s:%s%s ", username, identity.Hostname, cwd, sigil)
}

// appPromptString renders a foreground application's continuation prompt. The
// prompt is generated text written straight to the client, so it is reduced to a
// single printable line: control runes are dropped and a trailing space is added
// so the cursor never sits against the typed line. An app that supplies no
// usable prompt falls back to its name.
func appPromptString(app *application.AppPrompt) string {
	text := printableLine(app.Prompt)
	if text == "" {
		text = printableLine(app.Name) + ">"
	}
	if !strings.HasSuffix(text, " ") {
		text += " "
	}
	return text
}

// printableLine keeps only the runes that render on one line, dropping every
// control rune including line breaks and tabs.
func printableLine(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
}

// pumpOutputs drains accepted output and outcomes until the session ends.
func (h *sshHandler) pumpOutputs(ctx context.Context, sess *ssh.Session, shell application.Shell, prompts chan<- string) {
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
			h.writeOutput(ctx, sess, shell, out, prompts)
		case outcome, ok := <-outcomes:
			if !ok {
				outcomes = nil
				continue
			}
			if outcome.Kind == application.OutcomeFailed && outcome.Failure != nil {
				h.write(sess, "vibeshell: "+outcome.Failure.Message)
			}
		}
	}
}

// writeOutput renders one accepted output item to the transport. A prompt is
// not written here: the reader owns the editable prompt region, so the new
// prompt is handed to it instead of being drawn behind the cursor.
func (h *sshHandler) writeOutput(_ context.Context, sess *ssh.Session, shell application.Shell, out application.SessionOutput, prompts chan<- string) {
	switch out.Kind {
	case application.OutputText:
		h.write(sess, out.Text)
		_ = shell.ReportWrite(context.Background(), application.WriteResult{
			Session: out.Session, Turn: out.Turn, Sequence: out.Sequence,
			Status: application.WriteOK, ByteCount: int64(len(out.Text)),
		})
	case application.OutputPrompt:
		if out.Prompt != nil {
			select {
			case prompts <- promptString(h.identity, sess.Principal().Username, *out.Prompt):
			default:
				// The reader is behind; the next prompt will arrive after the
				// following turn, and the reader still holds a usable prompt.
			}
		}
	case application.OutputFrame, application.OutputContent:
		// A rendered frame or a content chunk is already exact bytes in the
		// content store; write them straight to the client.
		if h.content == nil {
			_ = shell.ReportWrite(context.Background(), application.WriteResult{
				Session: out.Session, Turn: out.Turn, Sequence: out.Sequence,
				Status: application.WriteDropped,
			})
			return
		}
		data, err := h.content.Get(context.Background(), out.Content, 0, out.ByteCount)
		if err != nil {
			_ = shell.ReportWrite(context.Background(), application.WriteResult{
				Session: out.Session, Turn: out.Turn, Sequence: out.Sequence,
				Status: application.WriteDropped,
			})
			return
		}
		_, _ = sess.Write(data)
		_ = shell.ReportWrite(context.Background(), application.WriteResult{
			Session: out.Session, Turn: out.Turn, Sequence: out.Sequence,
			Status: application.WriteOK, ByteCount: int64(len(data)),
		})
	default:
		// An output kind with no wired transport path is reported as dropped
		// rather than silently ignored.
		_ = shell.ReportWrite(context.Background(), application.WriteResult{
			Session: out.Session, Turn: out.Turn, Sequence: out.Sequence,
			Status: application.WriteDropped,
		})
	}
}

// readLoop edits one command line at a time with the terminal editor, so a
// session gets command history, cursor motion, and the common Emacs keys. It
// owns the prompt region: the output pump hands it each new prompt after a
// turn, and this loop repaints the prompt and the draft together.
func (h *sshHandler) readLoop(ctx context.Context, sess *ssh.Session, shell application.Shell, prompts <-chan string) {
	decoder, err := input.NewDecoder(h.inputLimits())
	if err != nil {
		return
	}
	ed, err := editor.New(editor.DefaultLimits())
	if err != nil {
		return
	}
	ed.SetEnterBehavior(editor.EnterSubmits)

	cols := h.clampCols(sess.Terminal().Cols)
	painter := editor.NewPainter(promptString(h.identity, sess.Principal().Username, snapshotPrompt(shell.Snapshot())))
	_, _ = sess.Write(painter.Redraw(ed, cols))

	// A dedicated reader feeds bytes so the loop can also react to a resize or
	// a new prompt while it waits for input.
	inputCh := make(chan []byte)
	go func() {
		defer close(inputCh)
		buf := make([]byte, 4096)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case inputCh <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	resizes := sess.ResizeNotify()
	repaint := func() { _, _ = sess.Write(painter.Redraw(ed, cols)) }
	var sequence uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-shell.Done():
			// The session ended, which end of input does at the shell but not
			// while an application is foreground.
			return
		case chunk, ok := <-inputCh:
			if !ok {
				_, _ = shell.Accept(context.Background(), application.SessionInput{Kind: application.InputEOF})
				return
			}
			for _, event := range decoder.Feed(chunk) {
				switch result := ed.Apply(event); result.Action {
				case editor.ActionSubmit:
					_, _ = sess.Write([]byte("\r\n"))
					sequence++
					if h.submitLine(ctx, shell, sess, result.Text, sequence) {
						return
					}
				case editor.ActionModified:
					repaint()
				case editor.ActionCancel:
					_, _ = shell.CancelActive(ctx, "ctrl-c")
					_, _ = sess.Write([]byte("^C\r\n"))
					repaint()
				case editor.ActionClearScreen:
					_, _ = sess.Write([]byte(screen.ClearScreen + screen.CursorHome))
					repaint()
				case editor.ActionEOF:
					// End of input ends the session at the shell, but while an
					// application is foreground it only leaves that application.
					// The session's Done channel distinguishes the two: the loop
					// keeps reading until it fires, and the shell prompt that
					// follows an app exit arrives through the prompt channel.
					_, _ = shell.Accept(context.Background(), application.SessionInput{Kind: application.InputEOF})
				}
			}
		case window, ok := <-resizes:
			if !ok {
				resizes = nil
				continue
			}
			cols = h.clampCols(window.Cols)
			repaint()
		case prompt, ok := <-prompts:
			if !ok {
				prompts = nil
				continue
			}
			painter = editor.NewPainter(prompt)
			repaint()
		}
	}
}

// submitLine submits one accepted command line and reports submission errors
// without tearing down the session. It reports whether the line asked to end
// the session, which the shell's own exit words do only while the shell is
// foreground: the same word submitted to a foreground application belongs to
// that application.
func (h *sshHandler) submitLine(ctx context.Context, shell application.Shell, sess *ssh.Session, command string, sequence uint64) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	if _, err := shell.Accept(ctx, application.SessionInput{
		Kind:     application.InputCommand,
		Command:  command,
		Sequence: sequence,
	}); err != nil {
		h.write(sess, "vibeshell: "+err.Error())
	}
	return endsSession(command, shell.Snapshot().Foreground)
}

// endsSession reports whether a submitted line asks to close the session. Only
// the shell's own exit words end it, and only while the shell is foreground: a
// foreground application owns its lines, so its "quit" belongs to the app.
func endsSession(command string, foreground application.ForegroundState) bool {
	return foreground.Kind != application.ForegroundApp && isExitCommand(command)
}

// isExitCommand reports whether a command asks to close the session.
func isExitCommand(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "exit", "quit", "logout":
		return true
	}
	return false
}

// inputLimits bounds one session's input decoder, using the configured paste
// bound when set.
func (h *sshHandler) inputLimits() input.Limits {
	limits := input.DefaultLimits()
	if h.maxPasteBytes > 0 {
		limits.MaxPasteBytes = h.maxPasteBytes
	}
	return limits
}

// clampCols bounds a client-reported width for the editor's wrapping, matching
// the transport and application bounds the session was opened with.
func (h *sshHandler) clampCols(cols int) int {
	if cols <= 0 || cols > h.terminal.maxCols {
		cols = ssh.DefaultTerminalCols
	}
	if cols > application.MaxTerminalCols {
		cols = application.MaxTerminalCols
	}
	return cols
}

// write emits one line of text to the client's stdout. Embedded line breaks are
// normalized to CRLF because the client's terminal is in raw mode and does not
// translate them itself.
func (h *sshHandler) write(sess *ssh.Session, text string) {
	if text == "" {
		_, _ = sess.Write([]byte("\r\n"))
		return
	}
	_, _ = sess.Write([]byte(screen.NormalizeLineFeeds(text) + "\r\n"))
}

// promptState adapts a session snapshot to a prompt state.
func snapshotPrompt(s application.SessionSnapshot) application.PromptState {
	return application.PromptState{CWD: s.CWD, ExitCode: s.ExitStatus}
}
