// Package ssh is VibeShell's SSH inbound adapter (PLAN 4.3, task B05).
//
// It translates SSH connections into application sessions and nothing more:
// authentication, channel policy, terminal negotiation, and bounded byte
// streams. It never interprets commands, never starts an operating-system
// process or shell, and never applies client input to the host.
//
// Supported contract:
//
//   - Public mode accepts any valid username with SSH "none" authentication,
//     so a client is never prompted for a password. Password, public-key,
//     and keyboard-interactive methods are not registered.
//   - Secure mode advertises password authentication only. Client
//     public-key/certificate methods are not registered. Password checking
//     itself belongs to the config/password task (B08); this package only
//     owns the transport callback boundary, PasswordAuthenticator.
//   - The host key is persistent: LoadOrGenerateHostKey loads an ed25519 key
//     from the service volume and creates it (mode 0600) when absent, so a
//     restart keeps the server identity.
//   - Only "session" channels are accepted. pty-req (bounded dimensions),
//     shell, window-change, locale env requests, and SSH signals are honored;
//     stdin/stdout/stderr, client EOF, and exit status are carried on the
//     channel.
//   - exec, subsystem, direct-tcpip and other non-session channels,
//     tcpip-forward and other global requests, x11-req,
//     auth-agent-req@openssh.com, and unknown channel requests are refused
//     explicitly. A refused exec/subsystem ends its channel with exit status
//     127 and a stderr explanation; other refusals reply failure and keep an
//     established shell alive.
//   - Each accepted shell channel becomes one Session. Sessions on one
//     connection (or across connections) share nothing except what the
//     application layer shares; in particular they have independent
//     terminals, input, output queues, and exit state.
//   - Disconnect cancels the session context, which the application observes
//     via Session.Context. Slow clients can neither block the application
//     (writes are queued and bounded, oldest bytes dropped and counted) nor
//     grow memory without limit.
//
// # Application boundary
//
// The application-facing boundary is the small Handler interface:
//
//	type Handler interface {
//	    ServeSession(ctx context.Context, sess *Session)
//	}
//
// The future session/turn coordinator (task C01) adapts each Session to an
// application session: it reads stdin bytes through Session.Read, observes
// resizes through Session.ResizeNotify, writes output through Session.Write,
// and reports completion through Session.Exit. A test double satisfies
// Handler with a few lines (see HandlerFunc and the package tests); no
// application business rule lives in this package.
package ssh
