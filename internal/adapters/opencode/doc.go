// Package opencode is the OpenCode HTTP/protocol adapter behind the
// ports.ModelGateway contract (PLAN 8.1, 15.3/B04). One Gateway serves the
// four wire-protocol families (Chat Completions, Responses, Messages,
// Gemini generateContent) over stdlib net/http only; no provider SDK is
// used and no dependency is added.
//
// Product route (Console vs Go) and wire protocol are tracked separately:
// a Protocol alone never selects an endpoint, and a model whose protocol
// is unknown fails closed instead of being silently sent to a chat URL.
//
// Codec and catalogue logic is ported from the A06 qualification spike at
// experiments/opencode-protocol (read-only reference). Authenticated live
// inference was never performed there or here, so live streaming fidelity,
// tool-call acceptance, Go x-opencode-session behavior, and inference URL
// suffixes beyond the observed catalogue paths remain UNVERIFIED.
package opencode
