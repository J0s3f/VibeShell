// Command spike-server runs the SSH transport spike as a standalone service so
// that a real OpenSSH client can drive it. The session it serves is a
// deterministic observation session, not a shell: every line it writes is a fact
// about what the transport delivered, so a receipt can be compared against what
// the client believes it sent.
//
// Nothing in this program interprets a command line as a command. "exit" and
// "exit <n>" are the only words it acts on, and they only choose the session's
// own SSH exit status.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/sshserver"
	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termdecode"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2222", "address to listen on")
	hostKeyPath := flag.String("host-key", "/state/spike_host_key", "persistent SSH host key path")
	mode := flag.String("mode", "public", "public (SSH none auth) or password")
	username := flag.String("user", "ada", "user accepted in password mode")
	password := flag.String("password", "", "password accepted in password mode")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	hostKey, err := sshserver.LoadOrCreateHostKey(*hostKeyPath)
	if err != nil {
		logger.Error("host key unavailable", "error", err.Error())
		os.Exit(1)
	}

	options := sshserver.Options{
		Mode:         sshserver.ModePublic,
		HostKey:      hostKey,
		ShellHandler: serveObservationSession,
		Logger:       logger,
	}
	switch *mode {
	case "public":
	case "password":
		if *password == "" {
			logger.Error("password mode needs -password")
			os.Exit(1)
		}
		options.Mode = sshserver.ModePassword
		options.Passwords = sshserver.PasswordFile{*username: {Password: *password}}
	default:
		logger.Error("unknown mode", "mode", *mode)
		os.Exit(1)
	}

	server, err := sshserver.NewServer(options)
	if err != nil {
		logger.Error("server unavailable", "error", err.Error())
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen failed", "addr", *addr, "error", err.Error())
		os.Exit(1)
	}
	logger.Info("spike server listening",
		"addr", listener.Addr().String(), "mode", *mode,
		"host_key", *hostKeyPath, "fingerprint", ssh.FingerprintSHA256(hostKey.PublicKey()))

	// Only an interrupt is handled: the container's stop signal ends the
	// process, and this program deliberately holds no other lifecycle state.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := server.Serve(ctx, listener); err != nil {
		logger.Error("serve failed", "error", err.Error())
		os.Exit(1)
	}
	logger.Info("spike server stopped")
}

// serveObservationSession reports what the session received, one line per fact.
// Lines end with CRLF because an OpenSSH client with a pty puts the local
// terminal in raw mode and performs no newline translation of its own.
func serveObservationSession(session *sshserver.Session) {
	terminal := session.Terminal()
	writeLine(session, fmt.Sprintf("welcome user=%s terminal=%s size=%dx%d pty=%t",
		session.Principal().Username, terminal.Name, terminal.Cols, terminal.Rows, terminal.Requested))
	for _, name := range []string{"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES"} {
		if value, ok := session.Env(name); ok {
			writeLine(session, fmt.Sprintf("env %s=%s", name, value))
		}
	}

	var line strings.Builder
	for {
		select {
		case <-session.Context().Done():
			return
		case <-session.InputClosed():
			// Every event from the last read is already queued when the input
			// stream reports its end, so drain it first. A client that sends
			// "exit 3" and closes stdin in the same write must get the status it
			// asked for, not the end-of-input path.
			for draining := true; draining; {
				select {
				case event := <-session.Events():
					if !observe(session, event, &line) {
						return
					}
				default:
					draining = false
				}
			}
			writeLine(session, fmt.Sprintf("eof err=%v", session.InputError()))
			_ = session.Exit(0)
			return
		case signal := <-session.Signals():
			writeLine(session, "signal "+signal)
		case event := <-session.Events():
			if !observe(session, event, &line) {
				return
			}
		}
	}
}

// observe reports one input event and reports whether the session continues.
func observe(session *sshserver.Session, event termdecode.Event, line *strings.Builder) bool {
	switch typed := event.(type) {
	case termdecode.Resize:
		writeLine(session, fmt.Sprintf("resize %dx%d", typed.Cols, typed.Rows))
		return true
	case termdecode.Paste:
		writeLine(session, fmt.Sprintf("paste bytes=%d truncated=%t", len(typed.Text), typed.Truncated))
		return true
	case termdecode.Key:
		return observeKey(session, typed, line)
	}
	return true
}

// observeKey reports one key press. Ctrl-C is called out separately because it
// arrives as the byte 0x03 in band, not as an SSH signal request.
func observeKey(session *sshserver.Session, key termdecode.Key, line *strings.Builder) bool {
	switch key.Name {
	case termdecode.KeyRune:
		line.WriteRune(key.Rune)
		return true
	case termdecode.KeyEnter:
		command := strings.TrimSpace(line.String())
		line.Reset()
		if command == "" {
			return true
		}
		return runCommand(session, command)
	case termdecode.KeyCtrlC:
		// A real shell discards the pending line; so does this session.
		line.Reset()
		writeLine(session, "key ctrl+c")
		writeLine(session, "interrupt ctrl-c")
		return true
	default:
		writeLine(session, "key "+key.String())
		return true
	}
}

// runCommand acts on the only two words this session understands.
func runCommand(session *sshserver.Session, command string) bool {
	rest, ok := strings.CutPrefix(command, "exit")
	if !ok || (rest != "" && !strings.HasPrefix(rest, " ")) {
		writeLine(session, "unknown-command "+command)
		return true
	}
	status := uint32(0)
	if fields := strings.Fields(rest); len(fields) > 0 {
		parsed, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			writeLine(session, "error exit status is not a number")
			return true
		}
		status = uint32(parsed)
	}
	writeLine(session, fmt.Sprintf("exit %d", status))
	_ = session.Exit(status)
	return false
}

func writeLine(session *sshserver.Session, line string) {
	_, _ = session.Write([]byte(line + "\r\n"))
}
