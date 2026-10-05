// Package load is VibeShell's capacity and soak harness (PLAN 13, task D06).
//
// It measures the shipped service layers under a planning workload of roughly
// 100 concurrent users and writes machine-readable receipts under
// tests/load/receipts. Every scenario drives the real inbound SSH adapter, the
// real session coordinator, the real terminal renderer, and the real SQLite
// world/event store. Model time is never real: the harness supplies a
// deterministic delayed ports.ModelGateway double, so no paid provider load
// test happens and no credential is required.
//
// Two facts the receipts must never blur:
//
//   - Local event latency (client write to first output byte on the same
//     connection) is measured and reported separately from provider time,
//     which in this harness is an injected delay, never an upstream service.
//   - Resource figures describe the machine that produced them. The harness
//     records CPU count, memory, cgroup bounds, and Go version with every
//     receipt so a reader can tell a real measurement from a promise.
//
// The harness is not a product surface: its turn engines are deterministic
// doubles standing in for simulation generation, and its admission gate is
// harness-owned because the runtime does not yet enforce
// inference.global_concurrency (recorded as a finding, not hidden here).
package load
