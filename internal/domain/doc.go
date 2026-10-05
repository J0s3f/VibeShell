// Package domain owns VibeShell's contract vocabulary: identities, scopes,
// world state, events, generated-application artifacts, model requests, and
// routing/health policies.
//
// Dependency-direction rule: adapters depend on application ports and the
// domain; the domain never depends on adapters. This package therefore
// imports only the Go standard library, performs no I/O, reads no clock and
// no randomness (callers inject timestamps and seeds explicitly so policy
// tests stay deterministic), and references no SSH, SQL, HTTP, provider SDK,
// or terminal types. The architecture guard test
// (contracts_guard_test.go) enforces this by scanning imports.
//
// All world-affecting decisions are pure values in this package; side effects
// live behind the small interfaces in internal/ports.
package domain
