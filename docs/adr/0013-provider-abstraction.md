# ADR 0013: Provider abstraction and the OpenCode CLI provider

Status: accepted.

Date: 2026-10-04.

## Context

The application had one inference implementation: `internal/adapters/opencode`
(the direct provider HTTP API) behind `ports.ModelGateway`. Its gateway held all
routes, accounts, and secrets. Reaching a second transport, or a second way of
authenticating, would have meant teaching that one gateway about both, and the
routing, MOTD, and generation code would have kept depending on a
single-provider shape.

A concrete need forced the question. The OpenCode Console free models (Big
Pickle, Fledge Alpha, MiMo Free, Ling, Nemotron, Muse Contributor Free, and
others) are gated to OpenCode clients: a direct HTTP request is rejected with
`403 FreeTierError` ("can only be used from within OpenCode"), while the
authenticated OpenCode CLI reaches them. Making those models usable therefore
requires a second transport whose credential is owned by the CLI, not by a
configured secret.

## Decision

Inference is multiplexed behind the existing port rather than a new one.

- `internal/provider.Registry` implements `ports.ModelGateway` and dispatches
  each request to the provider registered for its route. A route with no
  provider fails closed as `model_not_found`. It is itself a `ModelGateway`, so
  routing, the admission gate, and request logging are unchanged by adding a
  provider.
- Each provider is constructed from exactly the configuration it serves. The
  direct-HTTP `opencode.Gateway` receives the routes of its providers; the new
  `internal/adapters/opencodecli.Adapter` receives a route-to-model map. Neither
  knows about the other.
- `config.Provider.Kind` selects the implementation: `"http"` (the default) or
  `"cli"`. The choice is configuration, so adding a provider is a composition
  and configuration change; the shell and the SSH adapter stay unaware of it.
- `buildProviders` in the composition root groups configured routes by their
  provider and registers each route with the implementation its kind selects.
- The CLI adapter runs `opencode run -m <provider>/<model> --format json
  <prompt>`, collects the text parts of the NDJSON event stream, and maps
  failures onto the canonical envelopes (`model_not_found`, `invalid_arguments`,
  `network_timeout`, `provider_outage`, `invalid_response`). Its `Runner` is
  injectable, so tests never spawn a process. The CLI owns its credential; the
  adapter never handles a secret.
- An account may omit `secret_ref` only when it lists non-empty
  `permitted_products` and every one is served by a `cli`-kind provider. Such a
  credential-less account participates in routing but is not registered with the
  HTTP gateway, which has no secret to resolve for it.

## Alternatives considered

- Keep the single gateway and special-case CLI models inside it: rejected. It
  mixes two transports and two credential models in one adapter, and it makes
  the routing and generation code aware of the CLI, violating the dependency
  direction the port exists to protect.
- Give `opencode.Gateway` a per-route transport function: rejected. The gateway
  would still own every route and account, and the "provider" would not be an
  independently constructible unit.
- A registry outside `ports.ModelGateway` (consumers hold a registry and ask it
  for a gateway): rejected. Every consumer would have to know about the registry;
  keeping the registry a `ModelGateway` keeps the change invisible to them.
- Run the CLI as an HTTP shim and keep a single HTTP provider: rejected for now.
  It adds a process and an endpoint without removing the second credential
  model, and the CLI already emits a machine-readable stream.

## Consequences

- Adding a provider is additive: a new `Kind`, an implementation of
  `ports.ModelGateway`, and a branch in `buildProviders`. No shell, SSH, or
  routing change is needed.
- Two credential models coexist: secret-backed accounts for HTTP providers and
  credential-less accounts for the CLI, which owns its own login.
- The CLI provider is a subprocess per request (~12s cold in the live run).
  Latency and concurrency are bounded by the existing admission gate, not by the
  adapter.
- A CLI provider's product `base_url` is the CLI executable path, not an HTTP
  endpoint; its products must declare exactly `["chat"]` because the CLI returns
  text.

## Evidence

- Live receipt:
  `experiments/opencode-live-probe/receipts/2026-10-04T035600-opencode-cli-provider.json`
  (validated CLI config -> composed service -> MOTD request dispatched to the
  `cli` route `rte_0123456789ABCDEFGHJKMNPQRT` through the credential-less
  account -> CLI invoked -> rendered MOTD over real SSH, `result: success`,
  12194 ms).
- Commits: `9d20bcc` (frozen contracts), `fff262f` (registry), `8517f32` (CLI
  adapter), `51f856c` (config validation), `fd35f6d` (composition wiring).
