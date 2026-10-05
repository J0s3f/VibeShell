// Package admin exposes VibeShell's operator surface: configuration
// validation, user/hash maintenance, session listing, research export,
// route/account status, bounded probe triggering, integrity checks, backup,
// restore, and startup recovery of interrupted sessions (PLAN 10.5, 12.3).
//
// It is an application service, not a transport. The container executable
// drives it through `podman exec`; nothing here is reachable from the
// simulated shell, and no method forwards a command to a real shell or
// runtime. The service owns the admin vocabulary (BackupOps, RecoveryOps,
// SessionLister) and depends on small injected interfaces, so the SQLite
// adapter and the config adapter plug in without this package importing
// either one.
package admin
