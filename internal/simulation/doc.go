// Package simulation implements the agent-facing tool layer and context
// assembly for the VibeShell simulated world (PLAN 7.2 and 7.3).
//
// The package depends only on internal/domain, internal/ports, and
// internal/application: the adapter fulfills that layer's ToolExecutor seam,
// while application itself reaches only inward to domain and ports, so the
// dependency stays one-way. Every outbound effect goes through a port, so
// tests run against fakes and the composition root can wire real adapters
// without changing behavior here.
//
// Trust model: tool calls arrive from an untrusted model, but authorization
// never does. Each call carries a CallContext injected by trusted application
// context — session/user identity, resolved namespace IDs, the effective
// scope policy, and the current prompt version. Model arguments can name a
// scope but can never supply another user's or session's IDs, and the
// server-owned tool allowlist decides which tools exist at all. Results are
// bounded and failures are typed *domain.DomainError values.
package simulation
