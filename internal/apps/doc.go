// Package apps implements the generated-application registry and
// versioning capability (PLAN 5.6, task C03): immutable app
// artifacts, the candidate lifecycle (register, validate in an
// isolated staging sandbox, atomically activate, roll back),
// user/shared ownership, session version pinning, and state
// compatibility with additive migrations and safe fallback.
//
// Dependency direction: this package depends on the domain and on
// the outbound ports it consumes (AppRegistry, AppSandbox, Clock,
// Random). It owns no I/O; persistence and sandbox execution are
// supplied through the ports. The in-memory doubles in this package
// are contract doubles for the B01 persistence and B07 sandbox
// adapters; the composition root replaces them with the real
// adapters, which must preserve the reference semantics documented
// on each double.
package apps
