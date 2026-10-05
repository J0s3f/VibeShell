package sshserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Mode selects which SSH authentication methods the transport advertises.
type Mode int

const (
	// ModePublic authenticates any acceptable username with the SSH "none"
	// method. The server never offers password authentication, so no client
	// can be prompted for a password.
	ModePublic Mode = iota
	// ModePassword advertises password authentication only.
	ModePassword
)

// String returns the mode name used in logs and receipts.
func (m Mode) String() string {
	switch m {
	case ModePublic:
		return "public"
	case ModePassword:
		return "password"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Limits bound what one connection may consume. The spike values are starting
// points for the service, not measurements of a production load.
type Limits struct {
	// HandshakeTimeout bounds key exchange and authentication.
	HandshakeTimeout time.Duration
	// MaxAuthTries bounds authentication attempts per connection.
	MaxAuthTries int
	// MaxSessionsPerConnection bounds concurrent session channels on one
	// connection.
	MaxSessionsPerConnection int
	// MaxEnvRequests bounds env requests accepted per session.
	MaxEnvRequests int
	// MaxEnvNameBytes and MaxEnvValueBytes bound one env request.
	MaxEnvNameBytes  int
	MaxEnvValueBytes int
	// MaxTerminalNameBytes bounds the TERM string of a pty request.
	MaxTerminalNameBytes int
	// MaxTerminalModesBytes bounds the modes string of a pty request. OpenSSH
	// 9.2 sends 150 bytes of terminal modes for a stock xterm, so this has to
	// be comfortably above that; see
	// docs/research/2026-10-03-ssh-terminal-spike.md for the measurement.
	MaxTerminalModesBytes int
	// MinDimension and MaxDimension bound reported columns and rows.
	MinDimension int
	MaxDimension int
	// MaxOutputQueueBytes bounds the bytes a session buffers for a client that
	// is not reading.
	MaxOutputQueueBytes int
	// ExitDrainTimeout bounds how long a session waits for its queued output to
	// reach a client before it sends the exit status.
	ExitDrainTimeout time.Duration
	// EventQueueDepth bounds the decoded input events a session holds for its
	// handler.
	EventQueueDepth int
}

// DefaultLimits are the limits the qualification spike used.
var DefaultLimits = Limits{
	HandshakeTimeout:         30 * time.Second,
	MaxAuthTries:             6,
	MaxSessionsPerConnection: 16,
	MaxEnvRequests:           16,
	MaxEnvNameBytes:          64,
	MaxEnvValueBytes:         256,
	MaxTerminalNameBytes:     64,
	MaxTerminalModesBytes:    512,
	MinDimension:             1,
	MaxDimension:             4096,
	MaxOutputQueueBytes:      64 << 10,
	ExitDrainTimeout:         2 * time.Second,
	EventQueueDepth:          256,
}

// ShellHandler runs one interactive session. It must return when the session's
// context is done or when the client ends its input; the server then sends the
// exit status and closes the channel. A handler that returns early ends only its
// own session.
type ShellHandler func(*Session)

// Options configure a Server.
type Options struct {
	// Mode selects the advertised authentication methods.
	Mode Mode
	// Passwords is required in ModePassword and must not be set in
	// ModePublic, where no password method exists.
	Passwords PasswordFile
	// HostKey signs the server's key exchange. A persistent host key in the
	// service volume is the production arrangement; the spike generates an
	// ephemeral one when this field is nil.
	HostKey ssh.Signer
	// ShellHandler runs each accepted interactive session. Required.
	ShellHandler ShellHandler
	// Logger receives transport observations. Required.
	Logger *slog.Logger
	// Limits overrides DefaultLimits field by field; a zero field keeps the
	// default.
	Limits Limits
}

// Server accepts SSH connections and implements the VibeShell channel
// contract. It owns no global state, so one process may run several with
// different options.
type Server struct {
	config      *ssh.ServerConfig
	shell       ShellHandler
	logger      *slog.Logger
	limits      Limits
	hostKeyFP   string
	sessionsPer chan struct{}
}

// NewServer validates opts and returns a server ready to Serve.
func NewServer(opts Options) (*Server, error) {
	switch {
	case opts.ShellHandler == nil:
		return nil, errors.New("sshserver: ShellHandler is required")
	case opts.Logger == nil:
		return nil, errors.New("sshserver: Logger is required")
	}
	limits := opts.Limits.withDefaults(DefaultLimits)

	var signer ssh.Signer
	if opts.HostKey != nil {
		signer = opts.HostKey
	} else {
		generated, err := GenerateHostKey()
		if err != nil {
			return nil, fmt.Errorf("sshserver: generate host key: %w", err)
		}
		signer = generated
	}

	config := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-vibeshell_spike",
		MaxAuthTries:  limits.MaxAuthTries,
	}
	config.AddHostKey(signer)

	server := &Server{
		config:      config,
		shell:       opts.ShellHandler,
		logger:      opts.Logger,
		limits:      limits,
		hostKeyFP:   ssh.FingerprintSHA256(signer.PublicKey()),
		sessionsPer: make(chan struct{}, limits.MaxSessionsPerConnection),
	}

	switch opts.Mode {
	case ModePublic:
		// NoClientAuth makes the SSH "none" method succeed, and leaving
		// PasswordCallback nil keeps the password method refused
		// ("ssh: password auth not configured"). The client therefore never
		// receives a password prompt in public mode.
		config.NoClientAuth = true
		config.NoClientAuthCallback = func(meta ssh.ConnMetadata) (*ssh.Permissions, error) {
			principal, err := PublicPrincipal(connMetadata{meta})
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrUnauthenticated, err)
			}
			server.logger.Debug("public session accepted",
				"user", principal.Username, "fingerprint", server.hostKeyFP)
			return &ssh.Permissions{Extensions: map[string]string{
				"vibeshell-identity": principal.Username,
			}}, nil
		}
	case ModePassword:
		if opts.Passwords == nil {
			return nil, errors.New("sshserver: ModePassword requires Passwords")
		}
		// Only password authentication is registered. Public-key,
		// certificate, and keyboard-interactive callbacks stay nil, so those
		// methods are refused.
		config.PasswordCallback = func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			username := meta.User()
			principal, err := opts.Passwords.Password(connMetadata{meta}, username, string(password))
			if err != nil {
				server.logger.Info("password session refused",
					"user", username, "remote", meta.RemoteAddr().String(), "reason", err.Error())
				return nil, fmt.Errorf("%w: %s", ErrUnauthenticated, err)
			}
			server.logger.Debug("password session accepted",
				"user", principal.Username, "fingerprint", server.hostKeyFP)
			return &ssh.Permissions{Extensions: map[string]string{
				"vibeshell-identity": principal.Username,
			}}, nil
		}
	default:
		return nil, fmt.Errorf("sshserver: unknown mode %d", int(opts.Mode))
	}

	return server, nil
}

// withDefaults fills the zero fields of l from defaults.
func (l Limits) withDefaults(defaults Limits) Limits {
	out := l
	if out.HandshakeTimeout == 0 {
		out.HandshakeTimeout = defaults.HandshakeTimeout
	}
	if out.MaxAuthTries == 0 {
		out.MaxAuthTries = defaults.MaxAuthTries
	}
	if out.MaxSessionsPerConnection == 0 {
		out.MaxSessionsPerConnection = defaults.MaxSessionsPerConnection
	}
	if out.MaxEnvRequests == 0 {
		out.MaxEnvRequests = defaults.MaxEnvRequests
	}
	if out.MaxEnvNameBytes == 0 {
		out.MaxEnvNameBytes = defaults.MaxEnvNameBytes
	}
	if out.MaxEnvValueBytes == 0 {
		out.MaxEnvValueBytes = defaults.MaxEnvValueBytes
	}
	if out.MaxTerminalNameBytes == 0 {
		out.MaxTerminalNameBytes = defaults.MaxTerminalNameBytes
	}
	if out.MaxTerminalModesBytes == 0 {
		out.MaxTerminalModesBytes = defaults.MaxTerminalModesBytes
	}
	if out.MinDimension == 0 {
		out.MinDimension = defaults.MinDimension
	}
	if out.MaxDimension == 0 {
		out.MaxDimension = defaults.MaxDimension
	}
	if out.MaxOutputQueueBytes == 0 {
		out.MaxOutputQueueBytes = defaults.MaxOutputQueueBytes
	}
	if out.ExitDrainTimeout == 0 {
		out.ExitDrainTimeout = defaults.ExitDrainTimeout
	}
	if out.EventQueueDepth == 0 {
		out.EventQueueDepth = defaults.EventQueueDepth
	}
	return out
}

// connMetadata adapts ssh.ConnMetadata to the narrower ConnMetadata the
// Authenticator sees, so the spike's authenticators cannot reach transport
// internals they do not need.
type connMetadata struct{ meta ssh.ConnMetadata }

func (c connMetadata) RemoteAddress() net.Addr { return c.meta.RemoteAddr() }
func (c connMetadata) User() string            { return c.meta.User() }

// Serve accepts connections until ctx is done or the listener fails. It closes
// the listener, every open connection, and every session before returning, so a
// cancelled context is also a shutdown.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	var (
		mu     sync.Mutex
		active = map[*ssh.ServerConn]struct{}{}
		wg     sync.WaitGroup
	)
	stopped := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopped) }) }

	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
			return
		}
		stop()
		_ = listener.Close()
		mu.Lock()
		for conn := range active {
			_ = conn.Close()
		}
		mu.Unlock()
	}()

	defer func() {
		// Unblock the watcher goroutine when Serve returns for another reason.
		stop()
		wg.Wait()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("sshserver: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveConnection(ctx, conn, func(sshConn *ssh.ServerConn) {
				mu.Lock()
				active[sshConn] = struct{}{}
				mu.Unlock()
			}, func(sshConn *ssh.ServerConn) {
				mu.Lock()
				delete(active, sshConn)
				mu.Unlock()
			})
		}()
	}
}

// serveConnection performs the handshake and runs the connection's channels.
func (s *Server) serveConnection(
	ctx context.Context,
	conn net.Conn,
	register func(*ssh.ServerConn),
	unregister func(*ssh.ServerConn),
) {
	_ = conn.SetDeadline(time.Now().Add(s.limits.HandshakeTimeout))
	sshConn, channels, requests, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		s.logger.Info("handshake refused", "remote", conn.RemoteAddr().String(), "error", err.Error())
		_ = conn.Close()
		return
	}
	register(sshConn)
	defer unregister(sshConn)
	defer sshConn.Close()

	// The handshake may legitimately take longer than one round trip for a
	// password user typing a password, but the deadline now exists to catch a
	// connection that stops talking entirely.
	_ = conn.SetDeadline(time.Time{})

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Every session of this connection dies with it, which is how a client
	// disconnect becomes a prompt context cancellation.
	go func() {
		_ = sshConn.Wait()
		cancel()
	}()

	go s.refuseGlobalRequests(connCtx, requests)

	principal := Principal{Username: sshConn.User()}
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			s.refuseChannel(newChannel)
			continue
		}
		select {
		case s.sessionsPer <- struct{}{}:
		case <-connCtx.Done():
			_ = newChannel.Reject(ssh.ConnectionFailed, "server is shutting down")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			<-s.sessionsPer
			s.logger.Warn("session channel rejected at accept", "user", principal.Username, "error", err.Error())
			continue
		}
		session := s.newSession(connCtx, principal, channel, channelRequests)
		go func() {
			defer func() { <-s.sessionsPer }()
			session.run()
		}()
	}
	cancel()
}

// refuseChannel rejects a channel type the contract does not allow.
func (s *Server) refuseChannel(newChannel ssh.NewChannel) {
	channelType := newChannel.ChannelType()
	reason := fmt.Sprintf("vibeshell opens interactive session channels only; %s is not supported", channelType)
	s.logger.Info("channel refused", "type", channelType, "reason", reason)
	_ = newChannel.Reject(ssh.Prohibited, reason)
}

// refuseGlobalRequests answers connection-level requests. The contract
// advertises no forwarding, so every one is refused with its reason.
func (s *Server) refuseGlobalRequests(ctx context.Context, requests <-chan *ssh.Request) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-requests:
			if !ok {
				return
			}
			reason := fmt.Sprintf("connection request %q is not supported", req.Type)
			s.logger.Info("global request refused", "type", req.Type)
			_ = req.Reply(false, []byte(reason))
		}
	}
}
