# Research Exports

VibeShell produces three projections from one canonical event stream (PLAN
10.5), implemented in `internal/export`. All three page through the event store
in bounded batches and stream to disk through a `bufio` buffer, so a large
export never holds the whole record in memory. Secrets are redacted according
to the query's `domain.RedactionPolicy`, and an export of a session that never
recorded an end is explicitly marked incomplete rather than given a fabricated
successful ending (`internal/export/doc.go`).

The `export.Service` implements `ports.Exporter`; it owns no storage. Events,
retrieval, and content arrive through ports, and time/randomness through the
`Clock`/`Random` ports, so an export is deterministic under test.

## Query and scoping

An `domain.ExportQuery` selects a format and a scope. Scope labels
(`internal/export/service.go` `scopeName`):

| Selector | Scope label |
| --- | --- |
| `Scope.SessionIDs` | `session` |
| `Scope.UserIDs` | `user` |
| `Filter.FromTime`/`ToTime` | `timerange` |
| nothing narrower | `experiment` |

Explicit session scope reads through `EventStore.List`, so ordering is the
durable per-session **sequence**, not a timestamp. User/time/experiment scope
reads through `RetrievalStore.Query` under the injected scope policy. A filter
can select event kinds, time range, turn/attempt/app-version, provenance source,
and sequence bounds. `OutputPathFor` derives a default filename per format, but
a caller may set `OutputPath` explicitly.

`asciicast` requires **exactly one session** — a replay of interleaved sessions
would not be a truthful recording. The JSONL bundle and transcript support
multi-session exports.

## 1. JSONL research bundle

`FormatJSONL` writes a self-contained directory
(`internal/export/bundle.go`; format name `vibeshell-jsonl-research-bundle`,
format version 1):

```
<bundle>/events.jsonl    one {envelope, payload} JSON object per event
<bundle>/contents/<id>   one file per emitted content blob, named by content ID
<bundle>/schema.json     format version, event schema, known event kinds, encoding + redaction notes
<bundle>/manifest.json   the verifiable index (see below)
```

Each event line is `{"envelope": <EventEnvelope>, "payload": <resolved payload>}`;
a nil payload is normalized to `{}`. Binary payloads use JSON base64, and
separately stored blobs are emitted under `contents/<content-id>`.

The **manifest** records: `format`/`format_version`, `event_schema_version`,
`exporter_version`, `scope`, `generated_at`, a per-session entry
(`session_id`, `status`, `end_reason`, `event_count`), the total `event_count`,
the accountable snapshots (`config_snapshot`, `prompt_snapshot`,
`catalog_snapshot`), the `secrets_excluded` decision, `content_blobs_verified`,
and a **`sha256` and size for every emitted file** (the manifest itself is not
listed, as it holds the index and cannot contain its own checksum).

`VerifyBundle(dir)` recomputes every listed file's sha256 and reports every
mismatch, unreadable entry, size mismatch, and unlisted file. An empty result
proves the bundle is intact. This is verified by the D05 receipt
(`TestExportJSONLBundleChecksums`; `export.VerifyBundle` reports no problems).

Session `status` is `complete` or `incomplete_disconnected`. Status is derived
from the recorded `session.end` event regardless of the query filter, so a
filter that omits the end event from the output does not silently flip the
status.

## 2. Readable transcript

`FormatTranscript` writes a UTF-8 projection that labels itself **LOSSY** up
front (`internal/export/transcript.go`; format name `vibeshell-transcript`,
version 1). It intentionally drops: exact payload bytes, checksums, provenance
detail, transport byte streams, full-screen cursor motion, and timing beyond
ordering. Alternate-screen output appears only as labelled screen snapshots or
transitions.

It distinguishes user input (command/key/paste/cancel/eof), terminal output
(including prompt frames), screen transitions, prompts, failures (model/tool/
route), model request/response, tool, world, app, and context events. Escape
sequences are rendered visible (`<ESC>`) and control characters dropped, so a
frame is safe as text.

On close it writes an explicit status line for any session with no recorded end:
`[STATUS] session <id> has NO recorded end: connection incomplete/disconnected;
no successful ending was recorded or reconstructed`. It never writes an end
line the stream did not contain (D05 `TestExportTranscriptLabels`).

## 3. asciicast v2 terminal recording

`FormatAsciicast` writes an asciinema-compatible **asciicast v2** recording
(`internal/export/asciicast.go`; `AsciicastFormatVersion`). The header is written
lazily once the session-start metadata is known (only a small bounded prefix —
64 records — is buffered while waiting); if it never arrives, conventional 80×24
`xterm-256color` defaults are used so a replay never fails on a missing header.

- Accepted frames become output events (`"o"`) carrying the referenced blob.
- Accepted input becomes `"i"` events: a command is `command + "\r"`, a key is
  its bytes; `resize` becomes `"r"` events with `COLSxROWS`.
- `cancel`/`eof`/`paste` carry no byte stream the replay format can express and
  are preserved in the JSONL bundle instead.
- Timestamps are seconds relative to the first event; a non-monotonic wall clock
  is clamped so replay timing never runs backwards.
- Asciicast v2 has **no end marker**, so an incomplete session simply ends
  mid-stream; the JSONL bundle carries the explicit disconnected status and the
  transcript states it.

`Replay(reader)` parses only version 2 and returns the header plus
`[time, code, data]` events; helpers `ResizeSchedule`, `AcceptedOutput`, and
`InputBytes` let a reader reconstruct window changes and the accepted streams.
The D05 receipt `TestExportAsciicastReplay` verifies an 80×24 header, both
committed frames replayed, and a recorded resize.

## Incomplete sessions

An export never manufactures a successful ending. Incomplete/disconnected
sessions are included with an explicit status: `incomplete_disconnected` in the
JSONL manifest, a `[STATUS]` line in the transcript, and a mid-stream end in the
asciicast. This is verified at the crash/consistency seam (D05
`TestCrashAtPartialEmissionRecoversTruthfully`): a committed world plus an
accepted frame with no transport-write outcome recovers truthfully, with no
fabricated success.

## Redaction

Redaction is applied before a projection sees a record, through
`internal/observability`'s `Redactor` according to `domain.RedactionPolicy`. The
D05 receipt `TestExportRedactsSecrets` confirms fabricated `sk-*`/Bearer secret
shapes are absent from every file of all three formats after redaction, and that
redacted bundle checksums remain valid. The bundle records the
`secrets_excluded` decision.

---
*Traceable to: `internal/export/{doc.go,service.go,source.go,bundle.go,transcript.go,asciicast.go,redact.go}`, `tests/integration/receipts/{README.md,go-test-integration.log}`, PLAN 10.5. Verified by D05 receipts (merged `91c7beb`).*
