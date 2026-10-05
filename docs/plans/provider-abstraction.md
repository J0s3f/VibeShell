# Plan: generic providers with an OpenCode CLI provider

Status: complete (2026-10-04). Contracts in `9d20bcc`; implementations merged in
`fff262f` (registry), `8517f32` (CLI adapter), `51f856c` (config validation),
`fd35f6d` (composition); ADR 0013 and a live receipt in `309611c`.

## Goal

Add a provider abstraction so VibeShell can use several independent inference
implementations behind one port, and add the OpenCode CLI as a provider
alongside the existing direct-HTTP provider. Adding a provider must not change
shell behavior, the SSH adapter, or the routing layer (hexagonal rule).

## Why

The Console free models (Big Pickle, Fledge, MiMo Free, Ling, Nemotron, Muse
Contributor Free, Jev Free, …) are gated to OpenCode clients: direct HTTP gets
`403 FreeTierError`, but the authenticated OpenCode CLI reaches them. A CLI
provider makes those models usable by the product; the abstraction also makes
future providers (other CLIs, other gateways) additive.

## Frozen contracts

### `internal/provider` — route dispatch

```go
type Registry struct{ ... }
func NewRegistry(clock ports.Clock) *Registry
func (r *Registry) Register(route domain.RouteID, gateway ports.ModelGateway)
func (r *Registry) Provider(route domain.RouteID) (ports.ModelGateway, bool)
func (r *Registry) Request(ctx, req) (domain.ModelResponse, error) // ports.ModelGateway
func (r *Registry) Len() int
```

A route with no registered provider fails closed as `model_not_found`.

### `internal/adapters/opencodecli` — CLI provider

```go
type Runner interface {
    Run(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}
type Adapter struct {
    Binary     string   // default "opencode"
    ProviderID string   // default "opencode"; used as -m <ProviderID>/<model>
    Models     map[domain.RouteID]string
    Clock      ports.Clock
    Runner     Runner   // nil = os/exec
    WorkingDir string   // empty = inherit
}
```

`Request` runs `opencode run -m <ProviderID>/<model> --format json <prompt>`,
collects the NDJSON `text` parts, and returns the joined text. Error mapping:
missing model → `model_not_found`; no messages → `invalid_arguments`; deadline →
`network_timeout`; non-zero exit → `provider_outage`; unparseable/empty output →
`invalid_response`.

### config

- `Provider.Kind` ∈ {`http` (default), `cli`}.
- For `kind: "cli"`: product `base_url` is the CLI binary path and is optional
  (default `opencode`); the product's protocols must be `["chat"]`.
- An account may omit `secret_ref` **only** when it lists non-empty
  `permitted_products` and every one is served by a `cli`-kind provider.

### composition (`cmd/vibeshell`)

`buildProviders(cfg, snapshot) (*provider.Registry, error)` groups routes by
their configured provider and constructs the implementation per kind, then
registers each route. The registry replaces the single gateway in the
composition; the admission gate and request-logging wrapper wrap the registry.

## Task split (parallel free subagents)

| Task | Owner scope | Deliverable |
| --- | --- | --- |
| P1 | `internal/provider/**` | Registry as frozen, with tests: dispatch, missing route fails closed, re-register replaces, `Len`, concurrent use. |
| P2 | `internal/adapters/opencodecli/**` | Adapter as frozen, with a fake Runner: NDJSON text join, deadline, empty/unparseable, non-zero exit, missing model, `WorkingDir` honored. |
| P3 | `internal/adapters/config/**` | `Provider.Kind` validation, `cli` product rules, secret-less account rules, tests. |
| P4 (main) | `cmd/vibeshell/**` | `buildProviders` wiring + composition test; ADR/FEATURES/guide; live test. |

P1–P3 are independent and compile against only `internal/ports` and
`internal/domain`. P4 depends on P1–P3 merged and is owned by the main agent.

## Integration and acceptance

- Main agent merges P1–P3, then implements P4 and runs `go test -race ./...`.
- A composition test builds a config with one `http` and one `cli` provider and
  asserts each route dispatches to the expected implementation.
- Live: add a `cli` route to the dev config and run an unknown command through
  the CLI provider, confirming an artifact is produced.
- Docs: ADR (provider abstraction), FEATURES row, model-routes guide section.
