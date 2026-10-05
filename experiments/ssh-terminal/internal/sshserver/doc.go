// Package sshserver implements the SSH transport half of the VibeShell
// channel contract for the qualification spike.
//
// The contract, taken from PLAN.md section 4.3:
//
//   - Public mode accepts any username with SSH "none" authentication, so a
//     client never sees a password prompt.
//   - Password mode advertises password authentication only. No client
//     public-key, certificate, or keyboard-interactive method is registered.
//   - Session channels support pty-req, env, shell, window-change, signal,
//     stdin/stdout, EOF, and exit-status, with every payload validated and
//     bounded.
//   - exec, subsystem, port and agent forwarding, X11, and any other channel or
//     request type is refused with a message naming the refusal reason.
//
// # No operating-system process is ever started
//
// This package never creates a process, a pseudo-terminal, or a shell. A
// session is a callback over a byte stream, and a refused request only produces
// an SSH failure message and an exit status. TestNoProcessSpawningPrimitives
// enforces that mechanically over every Go file in the spike module, so the
// property cannot rot as the spike grows.
package sshserver
