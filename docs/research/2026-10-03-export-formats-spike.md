# Export formats spike (PLAN 10.5, task D01): JSONL bundle, UTF-8 transcript, asciicast v2

Date: 2026-10-03. Bounded qualification experiment, not application code.
Worktree: `.worktrees/export-formats` (branch `agent/export-formats`).
Code: `experiments/export-formats/` (self-contained Go module,
`require j0s.at/vibeshell v0.0.0` with `replace => ../..` to reuse the
committed `internal/domain` event envelope).
Receipts: `experiments/export-formats/receipts/` (`test-race-verbose.log`,
`large-session-metrics.txt`, `sample/`). No root `go.mod`/`go.sum`,
`scripts/`, `containers/`, `cmd/`, `internal/`, `schemas/`, or `PLAN.md`
files were modified.

## What was built

One canonical synthetic event stream (`CanonicalStream`) built directly
on the committed domain envelope — session, input (raw/decoded/accepted),
terminal frame/prompt/mode/write kinds, model request/error, tool request,
world commit, app activation, context summary — each with per-session
sequence, UTC millisecond timestamp, monotonic nanosecond offset,
provenance, inline payload JSON, and content-addressed blobs for frame
bytes. The session deliberately ends without `session.end` (client
disconnect). All three formats are derived from the same records:

1. **Versioned JSONL research bundle** (`bundle/`): `events.jsonl`
   (envelope + payload per line), `contents/<id>` content blobs,
   `schema.json` (format/envelope descriptor + known kinds), and
   `manifest.json` (format/event schema versions, session status, scope,
   config/prompt/catalog snapshots, `secrets_excluded: true`, and a
   sha256+size entry per emitted file). Secrets are excluded by
   construction: the synthetic payloads carry no credentials, and the
   manifest records the exclusion.
2. **Readable UTF-8 transcript**: labels `[USER INPUT]`, `[TERMINAL
   OUTPUT]`, `[INTERRUPTION]`, `[FAILURE]`, `[PROMPT]`, `[MODEL]`,
   `[TOOL]`, `[WORLD]`, `[APP]`, `[CONTEXT]`, `[SCREEN SNAPSHOT: alt
   screen]`, `[SCREEN TRANSITION]`, and a `[STATUS]` line for incomplete
   sessions. The header states it is a LOSSY PROJECTION.
3. **asciicast v2** (`session.cast`): v2 header (`version: 2`,
   width/height, unix timestamp, `env`), `"o"` output frames, `"i"`
   accepted input, `"r"` resize events; replay parses the same file.

## Verification (all in `vibeshell-export-formats-dev` container)

- `go vet ./...` — clean.
- `go test -race ./...` — PASS (`receipts/test-race-verbose.log`):
  - `TestProjectionsFromCanonicalStream` — replaying the asciicast
    reconstructs the exact accepted terminal byte sequence (concat of
    `"o"` payloads == concat of committed frame blobs); input events
    equal the accepted command bytes; a `120x40` resize event is
    present; header conforms. Transcript contains every expected label
    and `NO recorded end`; it must NOT and does NOT contain a
    fabricated `session end` line. The JSONL manifest verifies
    (recomputed sha256 for every file) and reports
    `session_status: incomplete_disconnected`, `event_count: 17`.
  - `TestTamperedBlobFailsChecksum` — appending one byte to a content
    blob makes `VerifyManifest` report a sha256 mismatch.
  - `TestJSONLEventLinesRoundTrip` — all 17 event lines decode with
    contiguous sequences and non-empty payloads.
  - `TestLargeSessionStreaming` — 100,000 events exported through all
    three formats; `receipts/large-session-metrics.txt`:
    `events=100000 elapsed≈12s heap_alloc_before=685920
    heap_alloc_after=1370952 bytes_written=71433788`. Heap delta is
    ~1 MB after GC: writers use 64 KiB bufio buffers and the event
    stream is generated on the fly (no materialized session list).
- Sample outputs for review: `receipts/sample/` (manifest, 17-line
  JSONL, transcript, `session.cast` header + events).

## Lossiness and expressiveness

JSONL bundle: lossless for everything the canonical envelope records
(full payloads, refs, provenance, ordering). Cannot express: the
semantic absence of a secret that was never recorded (manifest only
asserts the policy `secrets_excluded`), transport RTTs below the clock
granularity, and intra-event timing detail. Caveats: binary payloads
inline as JSON-base64 (`InputRawPayload.bytes`); identities marshal as
strings; large payloads must live in `contents/` rather than inline.

UTF-8 transcript: lossy by design. Keeps ordering, roles (user input /
output / interruption / failure / mode transition), prompts, and
alt-screen snapshots. Drops exact payload bytes, checksums, timing
(beyond order), transport byte streams, raw input bytes, and cursor/
style state. Full-screen interaction is reduced to labelled snapshots —
reconstruction of the actual screen state is impossible from this
projection. It must always carry its lossy-projection header and an
explicit incomplete-session status instead of a successful ending
(verified in tests).

asciicast v2: expresses linear terminal output with timing, input
bytes, and resizes only. Cannot express: model/tool/world/app/context
events, raw vs decoded vs accepted input distinction, interruptions
(`cancel` maps to no byte event), screen-mode transitions
(`line→app/alt` has no v2 code — the frame bytes are recorded as
output, which replays visually but loses the semantic transition), the
incomplete/disconnected session status (v2 has no end marker — an
incomplete session simply stops), and failure attribution. Interop:
data strings in v2 must be valid UTF-8/JSON-escaped text — ESC-heavy
alternate-screen frames survive via `\u001b` escapes but many players
render them literally; `timestamp`/`time` fields must be consistent;
third-party viewers assume a local terminal semantics, so the
simulated nature of VibeOS output is not represented.

## Open questions

- Whether `terminal.mode` transitions deserve a custom asciicast
  extension code (e.g. `"m"` marker payloads) versus staying JSONL-only.
- Binary-safe asciicast: v2 requires JSON string payloads, so raw
  binary frames need escaping or stay in the JSONL bundle.
- Clock granularity for event times and the exact replay-semantics
  contract for `i` events (echo vs accepted action bytes) should be
  fixed before the projection becomes a public contract.
- A streaming checksum/index (e.g. Merkle over events) may be needed
  for very large bundles; the current manifest is written after the
  stream completes (bounded memory but not itself resumable mid-file).
