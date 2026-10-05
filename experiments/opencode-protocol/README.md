# OpenCode provider/protocol qualification (A06)

Self-contained nested Go module. No live authenticated inference is
performed here; the authenticated portion of A06 stays pending for lack of
an API key and must not be faked.

- Root `gateway` package: one canonical `Result` over four SSE families
  (Chat Completions, Responses, Messages, Gemini), bounded SSE framing,
  error normalization with credential redaction, product/protocol
  endpoint separation.
- `catalog` package: keyless catalogue parsing plus PLAN 8.3
  suitable-free filtering against vendored models.dev metadata.
- `testdata`: recorded fixtures and vendored snapshots (byte-identical
  copies of `.research` snapshots, except live `created` stamps).
- `receipts`: live public-GET responses with hashes and the race test log.

Run in the checkout container:

```sh
podman exec --workdir /workspace/experiments/opencode-protocol \
  vibeshell-provider-protocol-dev sh -c 'go test -race ./...'
```

See `docs/research/2026-10-03-opencode-protocol-spike.md` for observed
facts, UNVERIFIED items, and open questions.
