package ssh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

// Mode selects which SSH authentication methods the transport advertises.
type Mode int

const (
	// ModePublic authenticates any valid username with the SSH "none" method.
	// The server never offers password authentication, so no client can be
	// prompted for a password.
	ModePublic Mode = iota
	// ModePassword advertises password authentication only. Client
	// public-key/certificate methods are never registered.
	ModePassword
)

// String returns the mode name used in logs.
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

// Limits bound what clients may consume. Zero values select defaults via
// withDefaults; NewServer rejects inconsistent explicit values.
type Limits struct {
	// HandshakeTimeout bounds key exchange and authentication.
	HandshakeTimeout time.Duration
	// MaxAuthTries bounds authentication attempts per connection.
	MaxAuthTries int
	// MaxSessionsPerConnection bounds concurrent session channels on one
	// connection.
	MaxSessionsPerConnection int
	// MaxEnvRequests bounds accepted env requests per session.
	MaxEnvRequests int
	// MaxEnvNameBytes and MaxEnvValueBytes bound one env request.
	MaxEnvNameBytes  int
	MaxEnvValueBytes int
	// MaxTerminalNameBytes bounds the TERM string of a pty request.
	MaxTerminalNameBytes int
	// MaxTerminalModesBytes bounds the modes string of a pty request.
	// Real OpenSSH clients send every TTY mode opcode (roughly 150-200
	// bytes); the bound stays small but must admit them. Modes are stored
	// verbatim and never interpreted.
	MaxTerminalModesBytes int
	// MinDimension and MaxDimension bound reported columns and rows.
	MinDimension int
	MaxDimension int
	// MaxOutputQueueBytes bounds the bytes queued per stream for a client
	// that is not reading.
	MaxOutputQueueBytes int
	// ExitDrainTimeout bounds how long a session waits for queued output to
	// reach the client before it sends the exit status.
	ExitDrainTimeout time.Duration
}

// DefaultLimits are the starting bounds for the service, not measurements of
// a production load. The main agent may tune them after the capacity
// benchmark (PLAN 13); every field stays explicit and documented.
var DefaultLimits = Limits{
	HandshakeTimeout:         30 * time.Second,
	MaxAuthTries:             6,
	MaxSessionsPerConnection: 16,
	MaxEnvRequests:           16,
	MaxEnvNameBytes:          64,
	MaxEnvValueBytes:         256,
	MaxTerminalNameBytes:     64,
	MaxTerminalModesBytes:    1024,
	MinDimension:             1,
	MaxDimension:             4096,
	MaxOutputQueueBytes:      64 << 10,
	ExitDrainTimeout:         2 * time.Second,
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
	if out.MaxTerminalNameBytes == 0 {
		out.MaxTerminalNameBytes = defaults.MaxTerminalNameBytes
	}
	if out.MaxTerminalModesBytes == 0 {
		out.MaxTerminalModesBytes = defaults.MaxTerminalModesBytes
	}
	if out.MaxEnvValueBytes == 0 {
		out.MaxEnvValueBytes = defaults.MaxEnvValueBytes
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
	return out
}

// validate reports inconsistent explicit bounds.
func (l Limits) validate() error {
	switch {
	case l.HandshakeTimeout <= 0:
		return errors.New("ssh: HandshakeTimeout must be positive")
	case l.MaxAuthTries <= 0:
		return errors.New("ssh: MaxAuthTries must be positive")
	case l.MaxSessionsPerConnection <= 0:
		return errors.New("ssh: MaxSessionsPerConnection must be positive")
	case l.MaxEnvRequests < 0:
		return errors.New("ssh: MaxEnvRequests must not be negative")
	case l.MaxEnvNameBytes <= 0 || l.MaxEnvValueBytes <= 0:
		return errors.New("ssh: env size bounds must be positive")
	case l.MaxTerminalNameBytes <= 0 || l.MaxTerminalModesBytes <= 0:
		return errors.New("ssh: terminal size bounds must be positive")
	case l.MinDimension <= 0 || l.MaxDimension < l.MinDimension:
		return errors.New("ssh: dimension bounds must satisfy 0 < min <= max")
	case l.MaxOutputQueueBytes <= 0:
		return errors.New("ssh: MaxOutputQueueBytes must be positive")
	case l.ExitDrainTimeout <= 0:
		return errors.New("ssh: ExitDrainTimeout must be positive")
	default:
		return nil
	}
}

// Options configure a Server. The zero value is invalid; NewServer validates.
type Options struct {
	// Mode selects the advertised authentication methods.
	Mode Mode
	// Passwords checks passwords in ModePassword. It must be nil in
	// ModePublic (where no password method exists) and non-nil in
	// ModePassword.
	Passwords PasswordAuthenticator
	// HostKey signs key exchange. Production servers pass a persistent key
	// from LoadOrGenerateHostKey; a nil key generates an ephemeral one for
	// tests only.
	HostKey cryptossh.Signer
	// Handler runs each accepted shell channel. Required: it is the seam to
	// the application coordinator and the test double point.
	Handler Handler
	// Logger receives transport observations. A nil logger discards output.
	Logger *slog.Logger
	// Limits overrides DefaultLimits field by field; a zero field keeps the
	// default.
	Limits Limits
}

// Server accepts SSH connections and implements the VibeShell channel
// contract. It owns no process-global state: one process may run several
// servers with different options, and tests run one per loopback listener.
type Server struct {
	config  *cryptossh.ServerConfig
	handler Handler
	logger  *slog.Logger
	limits  Limits
}

// NewServer validates opts and returns a server ready to Serve.
func NewServer(opts Options) (*Server, error) {
	switch {
	case opts.Handler == nil:
		return nil, errors.New("ssh: Handler is required")
	}
	limits := opts.Limits.withDefaults(DefaultLimits)
	if err := limits.validate(); err != nil {
		return nil, err
	}

	signer := opts.HostKey
	if signer == nil {
		generated, err := GenerateHostKey()
		if err != nil {
			return nil, fmt.Errorf("ssh: generate host key: %w", err)
		}
		signer = generated
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	config := &cryptossh.ServerConfig{
		ServerVersion: "SSH-2.0-VibeShell",
		MaxAuthTries:  limits.MaxAuthTries,
	}
	config.AddHostKey(signer)

	server := &Server{
		config:  config,
		handler: opts.Handler,
		logger:  logger,
		limits:  limits,
	}

	switch opts.Mode {
	case ModePublic:
		if opts.Passwords != nil {
			return nil, errors.New("ssh: ModePublic must not set Passwords")
		}
		// NoClientAuth makes the SSH "none" method succeed, and leaving
		// PasswordCallback nil keeps the password method refused, so the
		// client never receives a password prompt in public mode. Public-key
		// and keyboard-interactive callbacks stay nil, so those methods are
		// refused as well.
		config.NoClientAuth = true
		config.NoClientAuthCallback = func(meta cryptossh.ConnMetadata) (*cryptossh.Permissions, error) {
			principal, err := publicPrincipal(meta.User())
			if err != nil {
				server.logger.Info("public session refused",
					"remote", meta.RemoteAddr().String(), "reason", err.Error())
				return nil, err
			}
			server.logger.Debug("public session accepted", "user", principal.Username)
			return &cryptossh.Permissions{Extensions: map[string]string{
				"vibeshell-user": principal.Username,
			}}, nil
		}
	case ModePassword:
		if opts.Passwords == nil {
			return nil, errors.New("ssh: ModePassword requires Passwords")
		}
		// Only password authentication is registered. Public-key,
		// certificate, and keyboard-interactive callbacks stay nil, so those
		// methods are refused.
		passwords := opts.Passwords
		maxPassword := MaxPasswordBytes
		config.PasswordCallback = func(meta cryptossh.ConnMetadata, password []byte) (*cryptossh.Permissions, error) {
			username := meta.User()
			if err := ValidateUsername(username); err != nil {
				server.logger.Info("password session refused",
					"remote", meta.RemoteAddr().String(), "reason", err.Error())
				return nil, err
			}
			if len(password) > maxPassword {
				server.logger.Info("password session refused",
					"user", username,
					"remote", meta.RemoteAddr().String(),
					"reason", "password exceeds length bound")
				return nil, fmt.Errorf("%w: oversized password", ErrUnauthenticated)
			}
			principal, err := passwords.AuthenticatePassword(username, string(password))
			if err != nil {
				server.logger.Info("password session refused",
					"user", username,
					"remote", meta.RemoteAddr().String(),
					"reason", err.Error())
				return nil, err
			}
			if principal.Username == "" {
				server.logger.Info("password session refused",
					"user", username,
					"remote", meta.RemoteAddr().String(),
					"reason", "authenticator returned an empty principal")
				return nil, fmt.Errorf("%w: empty principal", ErrUnauthenticated)
			}
			server.logger.Debug("password session accepted", "user", principal.Username)
			return &cryptossh.Permissions{Extensions: map[string]string{
				"vibeshell-user": principal.Username,
			}}, nil
		}
	default:
		return nil, fmt.Errorf("ssh: unknown mode %d", int(opts.Mode))
	}

	return server, nil
}

// Serve accepts connections until ctx ends or the listener fails. It closes
// the listener, every open connection, and every session before returning, so
// a cancelled context is also a shutdown: handlers observe it through their
// session context and Serve waits for them to return.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	var (
		mu     sync.Mutex
		active = map[*cryptossh.ServerConn]struct{}{}
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
			return fmt.Errorf("ssh: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveConnection(ctx, conn,
				func(sshConn *cryptossh.ServerConn) {
					mu.Lock()
					active[sshConn] = struct{}{}
					mu.Unlock()
				},
				func(sshConn *cryptossh.ServerConn) {
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
	register func(*cryptossh.ServerConn),
	unregister func(*cryptossh.ServerConn),
) {
	_ = conn.SetDeadline(time.Now().Add(s.limits.HandshakeTimeout))
	sshConn, channels, requests, err := cryptossh.NewServerConn(conn, s.config)
	if err != nil {
		s.logger.Info("handshake refused", "remote", conn.RemoteAddr().String(), "error", err.Error())
		_ = conn.Close()
		return
	}
	register(sshConn)
	defer unregister(sshConn)
	defer sshConn.Close()

	// The handshake may legitimately take a while for a password user typing
	// at a prompt, but the deadline now exists to catch a connection that
	// stops talking entirely.
	_ = conn.SetDeadline(time.Time{})

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Every session of this connection dies with it, which is how a client
	// disconnect becomes an application cancellation.
	go func() {
		if err := sshConn.Wait(); err != nil {
			s.logger.Debug("connection loop ended", "remote", conn.RemoteAddr().String(), "error", err.Error())
		}
		cancel()
	}()

	go s.refuseGlobalRequests(connCtx, requests)

	principal := Principal{Username: sshConn.User()}
	// The semaphore is per connection: one greedy client cannot starve other
	// connections, and idle connections hold no shared slot.
	sem := make(chan struct{}, s.limits.MaxSessionsPerConnection)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			s.refuseChannel(newChannel)
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-connCtx.Done():
			_ = newChannel.Reject(cryptossh.ConnectionFailed, "server is shutting down")
			continue
		default:
			_ = newChannel.Reject(cryptossh.ResourceShortage, "too many session channels on this connection")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			<-sem
			s.logger.Info("session channel rejected at accept",
				"user", principal.Username, "error", err.Error())
			continue
		}
		session := s.newSession(connCtx, principal, channel, channelRequests)
		go func() {
			defer func() { <-sem }()
			session.run()
		}()
	}
	cancel()
}

// refuseChannel rejects a channel type the contract does not allow. This
// covers direct-tcpip, forwarded-tcpip, x11, and session-agent channels: none
// is ever opened, so no forwarded byte can reach the application.
func (s *Server) refuseChannel(newChannel cryptossh.NewChannel) {
	reason := "vibeshell only opens interactive session channels: " +
		newChannel.ChannelType() + " channels are not supported"
	s.logger.Info("channel refused", "type", newChannel.ChannelType())
	_ = newChannel.Reject(cryptossh.Prohibited, reason)
}

// refuseGlobalRequests answers connection-level requests. The contract
// advertises no forwarding, so every one (tcpip-forward,
// cancel-tcpip-forward, and anything else) is refused with its reason.
func (s *Server) refuseGlobalRequests(ctx context.Context, requests <-chan *cryptossh.Request) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-requests:
			if !ok {
				return
			}
			s.logger.Info("global request refused", "type", req.Type)
			_ = req.Reply(false, []byte("connection request "+req.Type+" is not supported"))
		}
	}
}
